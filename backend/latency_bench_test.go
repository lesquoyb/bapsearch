package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This file is a reproducible latency harness: it wires the real pipeline
// (SummarizeService + LLMService + FetchService) to fake SearXNG / llama.cpp /
// trafilatura servers with controlled delays, then measures the user-visible
// latencies. Run with `go test -run TestLatency -v` to see the numbers.

// fakeLLM simulates the two llama.cpp servers with configurable latencies.
type fakeLLM struct {
	chatDelay       time.Duration // non-stream utility calls (reformulations…)
	firstTokenDelay time.Duration // stream: prompt processing before 1st token
	tokenDelay      time.Duration // stream: per-token generation time
	streamTokens    []string
	embedCallDelay  time.Duration // fixed cost per embeddings HTTP call
	embedItemDelay  time.Duration // additional cost per embedded input

	chatCalls        atomic.Int64
	embedCalls       atomic.Int64
	embeddedItems    atomic.Int64
	utilityInFlight  atomic.Int64
}

func (f *fakeLLM) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/props", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"default_generation_settings":{"n_ctx":2048}}`)
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Input any `json:"input"`
		}
		_ = json.Unmarshal(body, &req)
		items := 1
		if arr, ok := req.Input.([]any); ok {
			items = len(arr)
		}
		f.embedCalls.Add(1)
		f.embeddedItems.Add(int64(items))
		time.Sleep(f.embedCallDelay + time.Duration(items)*f.embedItemDelay)

		type datum struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}
		out := struct {
			Data []datum `json:"data"`
		}{}
		for i := 0; i < items; i++ {
			out.Data = append(out.Data, datum{Index: i, Embedding: []float64{1, 0.5, float64(i%7) * 0.1, 0.2}})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		f.chatCalls.Add(1)

		if !req.Stream {
			f.utilityInFlight.Add(1)
			defer f.utilityInFlight.Add(-1)
			time.Sleep(f.chatDelay)
			fmt.Fprint(w, `{"choices":[{"message":{"content":"alternative phrasing one\nalternative phrasing two"}}]}`)
			return
		}

		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		time.Sleep(f.firstTokenDelay)
		for _, tok := range f.streamTokens {
			chunk := fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, tok)
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			fl.Flush()
			time.Sleep(f.tokenDelay)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// startFakeWeb returns a SearXNG lookalike plus page servers with controlled
// delays, and the URL list it will return.
func startFakeWeb(t *testing.T, searchDelay, pageDelay time.Duration, pageCount int) (searxURL string, trafilaturaURL string) {
	t.Helper()

	longText := strings.Repeat("This sentence pads the extracted source text so ranking accepts it. ", 12) // > 500 chars

	pages := http.NewServeMux()
	pages.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(pageDelay)
		fmt.Fprintf(w, "<html><body><p>%s</p></body></html>", longText)
	})
	pageSrv := httptest.NewServer(pages)
	t.Cleanup(pageSrv.Close)

	searx := http.NewServeMux()
	searx.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(searchDelay)
		type sr struct {
			URL     string `json:"url"`
			Title   string `json:"title"`
			Content string `json:"content"`
			Engine  string `json:"engine"`
		}
		out := struct {
			Results             []sr  `json:"results"`
			UnresponsiveEngines []any `json:"unresponsive_engines"`
		}{}
		q := r.URL.Query().Get("q")
		for i := 0; i < pageCount; i++ {
			out.Results = append(out.Results, sr{
				URL:     fmt.Sprintf("%s/%s/page-%d", pageSrv.URL, strings.ReplaceAll(q, " ", "-"), i),
				Title:   fmt.Sprintf("Result %d for %s", i, q),
				Content: "A search snippet that is intentionally descriptive enough to embed.",
				Engine:  "fake",
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	searxSrv := httptest.NewServer(searx)
	t.Cleanup(searxSrv.Close)

	extractor := http.NewServeMux()
	extractor.HandleFunc("/extract", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, longText)
	})
	extractorSrv := httptest.NewServer(extractor)
	t.Cleanup(extractorSrv.Close)

	return searxSrv.URL + "/search", extractorSrv.URL + "/extract"
}

func newBenchServices(t *testing.T, llm *fakeLLM, searxURL, trafilaturaURL string, batchSize, reformulations int) (*SummarizeService, *EventBroker, int64) {
	t.Helper()

	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	llmSrv := llm.server(t)

	conversations := &ConversationService{db: db, logger: logger, summaryTarget: 3}
	llmService := &LLMService{
		baseURL:            llmSrv.URL + "/v1/chat/completions",
		embeddingsURL:      llmSrv.URL + "/v1/embeddings",
		client:             &http.Client{Timeout: 30 * time.Second},
		logger:             logger,
		maxResponseTokens:  256,
		contextTokens:      8192,
		maxEmbeddingTokens: 256,
		embeddingBatchSize: batchSize,
		temperature:        0.2, topP: 1.0, topK: 40,
	}
	fetchService := NewFetchService(logger, "trafilatura", trafilaturaURL, 3, 6000, 10)
	events := NewEventBroker()

	service := &SummarizeService{
		conversations:       conversations,
		search:              &SearchService{baseURL: searxURL, client: &http.Client{Timeout: 10 * time.Second}},
		fetch:               fetchService,
		llm:                 llmService,
		logger:              logger,
		urlLimit:            3,
		queryReformulations: reformulations,
		events:              events,
		cancellations:       NewCancellations(),
	}

	if _, err := db.Exec(`INSERT INTO users (id) VALUES (?)`, "bench-user"); err != nil {
		t.Fatalf("insert user failed: %v", err)
	}
	convID := insertConversation(t, db, "bench-user", "benchmark query")
	return service, events, convID
}

// TestLatencyFirstResultsBeforeReformulation verifies (and measures) that raw
// results are on screen before the reformulation LLM call has finished — the
// core of the time-to-first-results fix.
func TestLatencyFirstResultsBeforeReformulation(t *testing.T) {
	llm := &fakeLLM{
		chatDelay:      400 * time.Millisecond, // slow reformulation call
		embedCallDelay: 5 * time.Millisecond,
		streamTokens:   []string{"answer"},
	}
	searxURL, trafURL := startFakeWeb(t, 20*time.Millisecond, 5*time.Millisecond, 5)
	service, events, convID := newBenchServices(t, llm, searxURL, trafURL, 4, 2)

	searchJobs := make(chan SummaryJob, 4)
	processJobs := make(chan ProcessJob, 4)
	service.StartWorkers(searchJobs, processJobs, 1, 1)

	sub := events.Subscribe(convID)
	defer events.Unsubscribe(convID, sub)

	started := time.Now()
	searchJobs <- SummaryJob{ConversationID: convID, UserID: "bench-user", Query: "benchmark query"}

	var firstResults, ready time.Duration
	reformulationStillRunning := false
	deadline := time.After(15 * time.Second)
	for ready == 0 {
		select {
		case evt := <-sub:
			switch evt.Name {
			case "results":
				if firstResults == 0 {
					firstResults = time.Since(started)
					reformulationStillRunning = llm.utilityInFlight.Load() > 0 || llm.chatCalls.Load() == 0
				}
			case "pipeline":
				var p PipelineEvent
				_ = json.Unmarshal([]byte(evt.Data), &p)
				if p.Status == "ready" {
					ready = time.Since(started)
				}
				if p.Status == "error" {
					t.Fatalf("pipeline failed: %s", p.Detail)
				}
			}
		case <-deadline:
			t.Fatalf("pipeline did not reach ready (firstResults=%v)", firstResults)
		}
	}

	t.Logf("time-to-first-results: %v", firstResults)
	t.Logf("time-to-ready (fetch+extract+embed+rank): %v", ready)
	t.Logf("embedding HTTP calls: %d for %d embedded inputs", llm.embedCalls.Load(), llm.embeddedItems.Load())

	if firstResults == 0 {
		t.Fatal("results event never arrived")
	}
	if !reformulationStillRunning {
		t.Error("raw results should be published before the reformulation call completes")
	}
	if firstResults > 300*time.Millisecond {
		t.Errorf("first results took %v; expected well under the 400ms reformulation delay", firstResults)
	}
}

// TestLatencyEmbeddingBatchingReducesCalls compares batch sizes 1 and 4 on the
// same workload and reports the difference.
func TestLatencyEmbeddingBatchingReducesCalls(t *testing.T) {
	run := func(batchSize int) (time.Duration, int64) {
		llm := &fakeLLM{
			embedCallDelay: 40 * time.Millisecond, // per-call overhead dominates
			embedItemDelay: 5 * time.Millisecond,
			streamTokens:   []string{"answer"},
		}
		searxURL, trafURL := startFakeWeb(t, 5*time.Millisecond, 2*time.Millisecond, 6)
		service, events, convID := newBenchServices(t, llm, searxURL, trafURL, batchSize, 0)

		searchJobs := make(chan SummaryJob, 4)
		processJobs := make(chan ProcessJob, 4)
		service.StartWorkers(searchJobs, processJobs, 1, 1)
		sub := events.Subscribe(convID)
		defer events.Unsubscribe(convID, sub)

		started := time.Now()
		searchJobs <- SummaryJob{ConversationID: convID, UserID: "bench-user", Query: "batching benchmark"}
		deadline := time.After(15 * time.Second)
		for {
			select {
			case evt := <-sub:
				if evt.Name != "pipeline" {
					continue
				}
				var p PipelineEvent
				_ = json.Unmarshal([]byte(evt.Data), &p)
				if p.Status == "ready" {
					return time.Since(started), llm.embedCalls.Load()
				}
				if p.Status == "error" {
					t.Fatalf("pipeline failed: %s", p.Detail)
				}
			case <-deadline:
				t.Fatal("pipeline did not reach ready")
			}
		}
	}

	serialTime, serialCalls := run(1)
	batchTime, batchCalls := run(4)

	t.Logf("batch=1: ready in %v with %d embedding calls", serialTime, serialCalls)
	t.Logf("batch=4: ready in %v with %d embedding calls", batchTime, batchCalls)

	if batchCalls >= serialCalls {
		t.Errorf("batching should reduce embedding HTTP calls: batch=%d serial=%d", batchCalls, serialCalls)
	}
}

// TestLatencyAnswerTokensStreamedBeforeCompletion measures time-to-first-token
// vs total generation through the real stream path + guard — the fix for the
// "nothing shows until the whole answer is generated" problem.
func TestLatencyAnswerTokensStreamedBeforeCompletion(t *testing.T) {
	tokens := make([]string, 60)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("word%d ", i)
	}
	llm := &fakeLLM{
		firstTokenDelay: 50 * time.Millisecond,
		tokenDelay:      10 * time.Millisecond,
		streamTokens:    tokens,
		embedCallDelay:  time.Millisecond,
	}
	llmSrv := llm.server(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := &LLMService{
		baseURL:           llmSrv.URL + "/v1/chat/completions",
		client:            &http.Client{Timeout: 30 * time.Second},
		logger:            logger,
		maxResponseTokens: 256,
		contextTokens:     8192,
		temperature:       0.2, topP: 1.0, topK: 40,
	}

	var firstEmit time.Duration
	started := time.Now()
	guard := newAnswerStreamGuard(func(string) {
		if firstEmit == 0 {
			firstEmit = time.Since(started)
		}
	})
	sources := []RankedSource{{URL: "https://example.com", Title: "Example", SourceText: strings.Repeat("source text ", 60), SimilarityScore: 0.9}}
	reply, err := service.GenerateGroundedSearchAnswerStream(t.Context(), RequestMeta{}, "q", "q", sources, nil, guard.OnToken)
	if err != nil {
		t.Fatalf("GenerateGroundedSearchAnswerStream() failed: %v", err)
	}
	total := time.Since(started)
	guard.Finalize(reply)

	t.Logf("time-to-first-visible-token: %v", firstEmit)
	t.Logf("total generation time:       %v", total)

	if firstEmit == 0 {
		t.Fatal("no token was ever streamed to the client")
	}
	if firstEmit > total/2 {
		t.Errorf("first visible token at %v is too late relative to total %v", firstEmit, total)
	}
}
