package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

const (
	summaryCandidateMultiplier = 4
	minSummarySourceChars      = 500
	minSummaryOutputChars      = 80
)

// SummaryJob runs the fast search stage: query SearXNG, publish the raw
// results, then hand a ProcessJob to the heavy stage. Keeping the two stages on
// separate worker pools means a new search returns raw results quickly instead
// of queueing behind another conversation's fetch/embed work.
type SummaryJob struct {
	ConversationID int64
	UserID         string
	Query          string
}

// ProcessJob runs the heavy stage: fetch + extract + embed the given results,
// then rank. Produced by the search stage, or enqueued directly (ForceFull) to
// reprocess already-stored results without a new web search.
type ProcessJob struct {
	ConversationID int64
	UserID         string
	Query          string
	Results        []SearchResult
	Reformulations []string
	ForceFull      bool
}

type RankedSource struct {
	URL             string
	Title           string
	Snippet         string
	SourceText      string
	SimilarityScore float64
	RerankPosition  int
	QueryVariant    string
	EmbeddingJSON   string
}

type SummarizeService struct {
	conversations       *ConversationService
	search              *SearchService
	fetch               *FetchService
	llm                 *LLMService
	memory              *MemoryService
	logger              *slog.Logger
	urlLimit            int
	queryReformulations int
	events              *EventBroker
	cancellations       *Cancellations
	processJobs         chan<- ProcessJob
}

func (service *SummarizeService) StartWorkers(searchJobs <-chan SummaryJob, processJobs chan ProcessJob, searchWorkers, processWorkers int) {
	service.processJobs = processJobs
	if searchWorkers < 1 {
		searchWorkers = 1
	}
	if processWorkers < 1 {
		processWorkers = 1
	}
	// Search workers are network-bound (SearXNG) and safe to parallelize, so a
	// new search returns raw results quickly. Process workers are LLM-bound
	// (embeddings) and kept few to avoid thrashing constrained hardware.
	for index := 0; index < searchWorkers; index++ {
		go func() {
			for job := range searchJobs {
				service.runSearchStage(job)
			}
		}()
	}
	for index := 0; index < processWorkers; index++ {
		go func() {
			for job := range processJobs {
				service.runProcessStage(job)
			}
		}()
	}
}

// runSearchStage queries SearXNG (original query first, then optional LLM
// reformulations), publishes raw results as they arrive, and hands the new rows
// to the heavy process stage. It never fetches or embeds, so it stays fast and a
// new search isn't blocked by another conversation's processing.
func (service *SummarizeService) runSearchStage(job SummaryJob) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if service.cancellations != nil {
		deregister := service.cancellations.Register(job.ConversationID, cancel)
		defer deregister()
	}
	jobContext := context.WithValue(ctx, requestMetaKey, RequestMeta{RequestID: newRequestID(), UserID: job.UserID, ConversationID: job.ConversationID})
	meta := RequestMeta{RequestID: newRequestID(), UserID: job.UserID, ConversationID: job.ConversationID}
	logger := loggerWithMeta(jobContext, service.logger, job.ConversationID)
	logger.Info("search_stage_started", "query", job.Query)

	if err := service.conversations.UpdateAnswerStatus(jobContext, job.ConversationID, "searching", "Searching the web."); err != nil {
		logger.Error("updating answer status failed", "error", err)
	}
	service.publishPipeline(jobContext, job.ConversationID, "searching", "Searching the web.")

	query := strings.TrimSpace(job.Query)

	// seen deduplicates URLs across the original search and any reformulation
	// searches (first occurrence wins). inserted accumulates the rows that were
	// actually new so the process stage only fetches/embeds each once.
	seen := make(map[string]bool)
	var inserted []SearchResult
	var reformulations []string

	type searchBatch struct {
		results      []SearchResult
		engineStatus []SearchEngineStatus
		err          error
	}

	// 1) Search the ORIGINAL query first and publish its raw results
	//    immediately. The reformulation LLM call (when enabled) used to run
	//    *before* this, so nothing appeared on screen until the answer model had
	//    finished generating paraphrases — the main "time to first results"
	//    regression. Reformulations now happen after results show.
	originalResp, originalErr := service.search.Search(jobContext, query)
	if originalErr != nil {
		logger.Warn("original search failed", "error", originalErr)
	} else {
		var firstResults []SearchResult
		for _, r := range originalResp.Results {
			if !seen[r.URL] {
				seen[r.URL] = true
				firstResults = append(firstResults, r)
			}
		}
		if len(firstResults) > 0 {
			ins, err := service.conversations.AppendSearchResults(jobContext, job.ConversationID, firstResults, originalResp.EngineStatus)
			if err != nil {
				service.failPipeline(jobContext, logger, job.ConversationID, err)
				return
			}
			inserted = append(inserted, ins...)
			// Raw results are visible now, before any LLM work.
			service.events.Publish(job.ConversationID, "results", struct{}{})
		}
	}

	// 2) Optionally widen coverage with LLM-generated reformulations, searched
	//    in parallel and merged in — only after the original results are shown.
	if service.queryReformulations > 0 {
		refs, err := service.llm.GenerateQueryReformulations(jobContext, meta, query, service.queryReformulations)
		if err != nil {
			logger.Warn("generating query reformulations failed, continuing with original results", "error", err)
		} else if len(refs) > 0 {
			reformulations = refs
			logger.Info("query_reformulations_generated", "count", len(refs), "queries", refs)

			batchCh := make(chan searchBatch, len(refs))
			for _, q := range refs {
				go func(q string) {
					resp, err := service.search.Search(jobContext, q)
					if err != nil {
						batchCh <- searchBatch{err: err}
						return
					}
					batchCh <- searchBatch{results: resp.Results, engineStatus: resp.EngineStatus}
				}(q)
			}

			var moreResults []SearchResult
			var moreEngine []SearchEngineStatus
			for range refs {
				batch := <-batchCh
				if batch.err != nil {
					logger.Warn("reformulation search failed", "error", batch.err)
					continue
				}
				moreEngine = append(moreEngine, batch.engineStatus...)
				for _, r := range batch.results {
					if !seen[r.URL] {
						seen[r.URL] = true
						moreResults = append(moreResults, r)
					}
				}
			}

			if len(moreResults) > 0 {
				ins, err := service.conversations.AppendSearchResults(jobContext, job.ConversationID, moreResults, moreEngine)
				if err != nil {
					logger.Error("appending reformulation results failed", "error", err)
				} else {
					inserted = append(inserted, ins...)
					// More results trickle in — refresh the panel again.
					service.events.Publish(job.ConversationID, "results", struct{}{})
				}
			}
		}
	}

	if len(seen) == 0 {
		service.failPipeline(jobContext, logger, job.ConversationID, fmt.Errorf("all search batches failed or returned no results"))
		return
	}
	if len(inserted) == 0 {
		service.failPipeline(jobContext, logger, job.ConversationID, fmt.Errorf("no new search results could be processed"))
		return
	}

	// Hand the heavy fetch/embed/rank work to the process stage.
	service.processJobs <- ProcessJob{
		ConversationID: job.ConversationID,
		UserID:         job.UserID,
		Query:          query,
		Results:        inserted,
		Reformulations: reformulations,
	}
}

// runProcessStage fetches, extracts and embeds the job's results, then ranks all
// of the conversation's stored sources. This is the LLM-bound half of the
// pipeline; the answer itself is streamed separately on demand.
func (service *SummarizeService) runProcessStage(job ProcessJob) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if service.cancellations != nil {
		deregister := service.cancellations.Register(job.ConversationID, cancel)
		defer deregister()
	}
	jobContext := context.WithValue(ctx, requestMetaKey, RequestMeta{RequestID: newRequestID(), UserID: job.UserID, ConversationID: job.ConversationID})
	meta := RequestMeta{RequestID: newRequestID(), UserID: job.UserID, ConversationID: job.ConversationID}
	logger := loggerWithMeta(jobContext, service.logger, job.ConversationID)
	logger.Info("process_stage_started", "results", len(job.Results), "force_full", job.ForceFull)

	// Only deep-process (fetch → extract → embed) the top results. The main
	// pipeline previously fetched EVERY search result — dozens once
	// reformulations fan out — each spawning a trafilatura subprocess, which is
	// what made "cleaning and extracting" drag. Scale the cap by the number of
	// queries (like the iterative path) and mark the rest skipped so their card
	// dot doesn't sit stuck on "pending"; they remain available as raw results.
	limit := service.urlLimit
	if limit <= 0 {
		limit = 3
	}
	maxFetch := limit * (1 + len(job.Reformulations))
	toProcess := job.Results
	if len(job.Results) > maxFetch {
		toProcess = job.Results[:maxFetch]
		for _, r := range job.Results[maxFetch:] {
			service.updateSummaryStatus(jobContext, logger, job.ConversationID, r.URL, "skipped", "Beyond the per-search URL limit; kept as a raw result.")
		}
		logger.Info("process_stage_capped", "fetched", maxFetch, "skipped", len(job.Results)-maxFetch)
	}

	query := strings.TrimSpace(job.Query)

	// Build the embedding text: repeat the original query once per reformulation
	// then append each reformulation. This gives the original query N× more
	// weight than any individual reformulation while still pulling documents that
	// match the alternative phrasings.
	embeddingText := query
	if len(job.Reformulations) > 0 {
		parts := make([]string, 0, len(job.Reformulations)*2)
		for range job.Reformulations {
			parts = append(parts, query)
		}
		parts = append(parts, job.Reformulations...)
		embeddingText = strings.Join(parts, " ")
	}

	// The query embedding doesn't depend on the fetched documents, so compute
	// it while the fetch/extract/embed loop runs instead of after it.
	type queryEmbedResult struct {
		vector []float64
		err    error
	}
	queryEmbedCh := make(chan queryEmbedResult, 1)
	go func() {
		vector, err := service.llm.EmbedText(jobContext, meta, embeddingText)
		queryEmbedCh <- queryEmbedResult{vector: vector, err: err}
	}()

	if err := service.processResults(jobContext, meta, logger, job.ConversationID, toProcess); err != nil {
		service.failPipeline(jobContext, logger, job.ConversationID, err)
		return
	}

	if err := service.conversations.StoreRewrittenQuery(jobContext, job.ConversationID, query); err != nil {
		logger.Error("persisting query failed", "error", err)
	}

	if err := service.conversations.UpdateAnswerStatus(jobContext, job.ConversationID, "ranking", "Ranking sources with query and document embeddings."); err != nil {
		logger.Error("updating answer status failed", "error", err)
	}
	service.publishPipeline(jobContext, job.ConversationID, "ranking", "Ranking sources with query and document embeddings.")

	queryEmbed := <-queryEmbedCh
	if queryEmbed.err != nil {
		service.failPipeline(jobContext, logger, job.ConversationID, fmt.Errorf("query embedding failed: %w", queryEmbed.err))
		return
	}
	queryEmbedding := queryEmbed.vector

	rankedSources, err := service.rankSources(jobContext, logger, job.ConversationID, queryEmbedding)
	if err != nil {
		service.failPipeline(jobContext, logger, job.ConversationID, err)
		return
	}

	if len(rankedSources) == 0 {
		service.failPipeline(jobContext, logger, job.ConversationID, fmt.Errorf("no extracted sources were eligible for ranking"))
		return
	}

	readyCount := service.urlLimit
	if readyCount <= 0 {
		readyCount = 3
	}
	if len(rankedSources) < readyCount {
		readyCount = len(rankedSources)
	}

	// Publish similarity scores for all ranked cards.
	for _, src := range rankedSources {
		service.events.Publish(job.ConversationID, "card", CardEvent{
			URL:             src.URL,
			SimilarityScore: src.SimilarityScore,
		})
	}

	// Publish the final ranked order so the frontend can reorder cards in one
	// deterministic step rather than shuffling them as individual scores arrive.
	orderedURLs := make([]string, 0, len(rankedSources))
	for _, src := range rankedSources {
		orderedURLs = append(orderedURLs, src.URL)
	}
	service.events.Publish(job.ConversationID, "reorder", ReorderEvent{URLs: orderedURLs})

	detail := fmt.Sprintf("Ready to stream an answer from the top %d ranked sources.", readyCount)
	if err := service.conversations.UpdateAnswerStatus(jobContext, job.ConversationID, "ready", detail); err != nil {
		logger.Error("updating answer status failed", "error", err)
	}
	service.publishPipeline(jobContext, job.ConversationID, "ready", detail)

	logger.Info("pipeline_job_finished", "ranked_sources", len(rankedSources), "query", query)
}

func (service *SummarizeService) processResults(ctx context.Context, meta RequestMeta, logger *slog.Logger, conversationID int64, results []SearchResult) error {
	if len(results) == 0 {
		return nil
	}

	if err := service.conversations.UpdateAnswerStatus(ctx, conversationID, "extracting", "Extracting source text from the collected results."); err != nil {
		logger.Error("updating answer status failed", "error", err)
	}

	snippets := make(map[string]string, len(results))
	for _, r := range results {
		snippets[r.URL] = r.Snippet
	}

	docCh := service.fetch.FetchAndExtractChan(ctx, meta, results, func(url, status, detail string) {
		service.updateSummaryStatus(ctx, logger, conversationID, url, status, detail)
	})

	// Track which URLs were fetched to detect failures afterwards.
	fetched := make(map[string]bool)

	// Buffer ready-to-embed documents up to embeddingBatchSize then flush them
	// in a single HTTP call. With batch size 1 (the default) this stays
	// strictly one-at-a-time, identical to the previous behaviour.
	batchSize := service.llm.embeddingBatchSize
	if batchSize <= 0 {
		batchSize = 1
	}
	type pendingEmbed struct {
		url  string
		text string
	}
	pending := make([]pendingEmbed, 0, batchSize)

	flush := func() {
		if len(pending) == 0 {
			return
		}
		texts := make([]string, len(pending))
		for i, p := range pending {
			texts[i] = p.text
		}
		vectors, err := service.llm.EmbedTexts(ctx, meta, texts)
		if err != nil {
			logger.Error("batch embedding failed", "batch_size", len(pending), "error", err)
			for _, p := range pending {
				service.updateSummaryStatus(ctx, logger, conversationID, p.url, "error", err.Error())
			}
			pending = pending[:0]
			return
		}
		for i, p := range pending {
			// EmbedTexts may return a partial result (nil entry) when it fell
			// back to per-item embedding and that one input still failed. Skip
			// those rather than storing a "null" embedding that would poison ranking.
			if i >= len(vectors) || vectors[i] == nil {
				service.updateSummaryStatus(ctx, logger, conversationID, p.url, "error", "Embedding failed for this source.")
				continue
			}
			embeddingJSON, err := json.Marshal(vectors[i])
			if err != nil {
				logger.Error("embedding serialization failed", "url", p.url, "error", err)
				service.updateSummaryStatus(ctx, logger, conversationID, p.url, "error", err.Error())
				continue
			}
			if err := service.conversations.UpdateDocumentEmbedding(ctx, conversationID, p.url, string(embeddingJSON)); err != nil {
				logger.Error("storing document embedding failed", "url", p.url, "error", err)
				service.updateSummaryStatus(ctx, logger, conversationID, p.url, "error", err.Error())
				continue
			}
		}
		pending = pending[:0]
	}

	// Process each document as soon as it arrives: store source text immediately
	// (making hover-card content visible) then queue the embedding for batched
	// computation. The batch is flushed when full OR when the fetch channel
	// closes (handled by the defer-style call after the loop).
	for document := range docCh {
		fetched[document.URL] = true

		text := strings.TrimSpace(document.Text)
		if text == "" {
			// Trafilatura returned nothing — fall back to the search snippet.
			text = strings.TrimSpace(snippets[document.URL])
			if text == "" {
				service.updateSummaryStatus(ctx, logger, conversationID, document.URL, "skipped", "Ignored because extracted content was empty.")
				continue
			}
			service.updateSummaryStatus(ctx, logger, conversationID, document.URL, "extracting", "Using search snippet as fallback (trafilatura returned no content).")
		}
		document.Text = text

		if len([]rune(text)) < minSummarySourceChars {
			service.updateSummaryStatus(ctx, logger, conversationID, document.URL, "skipped", fmt.Sprintf("Ignored because the extracted text was too short (%d characters).", len([]rune(text))))
			continue
		}

		// Store source text immediately so the hover card can show it while
		// we wait for the embedding to be computed.
		if err := service.conversations.StoreSourceText(ctx, conversationID, document.URL, document.Text); err != nil {
			logger.Error("storing source text failed", "url", document.URL, "error", err)
			service.updateSummaryStatus(ctx, logger, conversationID, document.URL, "error", err.Error())
			continue
		}
		// Push source text to connected clients immediately.
		service.events.Publish(conversationID, "card", CardEvent{URL: document.URL, SourceText: document.Text})

		service.updateSummaryStatus(ctx, logger, conversationID, document.URL, "embedding", "Generating document embeddings.")
		pending = append(pending, pendingEmbed{url: document.URL, text: document.Text})
		if len(pending) >= batchSize {
			flush()
		}
	}
	// Flush any partial batch left over once the fetch channel closes.
	flush()

	for _, result := range results {
		if !fetched[result.URL] {
			snippet := strings.TrimSpace(result.Snippet)
			if snippet == "" {
				service.updateSummaryStatus(ctx, logger, conversationID, result.URL, "error", "Failed to extract source text.")
				continue
			}
			// Fetch failed entirely but we have a snippet — use it as fallback.
			service.updateSummaryStatus(ctx, logger, conversationID, result.URL, "extracting", "Using search snippet as fallback (fetch failed).")
			if err := service.conversations.StoreSourceText(ctx, conversationID, result.URL, snippet); err != nil {
				logger.Error("storing fallback snippet failed", "url", result.URL, "error", err)
				service.updateSummaryStatus(ctx, logger, conversationID, result.URL, "error", err.Error())
				continue
			}
			service.updateSummaryStatus(ctx, logger, conversationID, result.URL, "embedding", "Generating document embeddings.")
			embedding, err := service.llm.EmbedText(ctx, meta, snippet)
			if err != nil {
				logger.Error("snippet embedding failed", "url", result.URL, "error", err)
				service.updateSummaryStatus(ctx, logger, conversationID, result.URL, "error", err.Error())
				continue
			}
			embeddingJSON, err := json.Marshal(embedding)
			if err != nil {
				service.updateSummaryStatus(ctx, logger, conversationID, result.URL, "error", err.Error())
				continue
			}
			if err := service.conversations.UpdateDocumentEmbedding(ctx, conversationID, result.URL, string(embeddingJSON)); err != nil {
				service.updateSummaryStatus(ctx, logger, conversationID, result.URL, "error", err.Error())
			}
		}
	}

	return nil
}

func (service *SummarizeService) rankSources(ctx context.Context, logger *slog.Logger, conversationID int64, queryEmbedding []float64) ([]RankedSource, error) {
	return service.conversations.RerankAllSources(ctx, logger, conversationID, queryEmbedding)
}

func (service *SummarizeService) failPipeline(ctx context.Context, logger *slog.Logger, conversationID int64, err error) {
	// Distinguish user-initiated cancellation from real failures so the UI can
	// render a calmer "cancelled" state instead of a scary error.
	status := "error"
	detail := err.Error()
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		status = "cancelled"
		detail = "Pipeline cancelled."
	}
	if status == "error" {
		logger.Error("pipeline_failed", "error", err)
	} else {
		logger.Info("pipeline_cancelled")
	}
	// Use a fresh background context for the status write so a cancelled ctx
	// doesn't prevent us from persisting the final state.
	persistCtx := context.Background()
	if statusErr := service.conversations.UpdateAnswerStatus(persistCtx, conversationID, status, detail); statusErr != nil {
		logger.Error("updating failed pipeline status", "error", statusErr)
	}
	service.publishPipeline(persistCtx, conversationID, status, detail)
}

func (service *SummarizeService) updateSummaryStatus(ctx context.Context, logger *slog.Logger, conversationID int64, url, status, detail string) {
	if err := service.conversations.UpdateSummaryStatus(ctx, conversationID, url, status, detail); err != nil {
		logger.Error("updating summary status failed", "url", url, "status", status, "error", err)
	}
	service.events.Publish(conversationID, "card", CardEvent{URL: url, Status: status, Detail: detail})

	// Refresh the pipeline progress bar when a card reaches a terminal state.
	// Without this the bar would only move on the four explicit phase changes
	// (searching → extracting → ranking → ready) and feel frozen during the
	// long fetch+embed loop.
	switch status {
	case "ready", "skipped", "error":
		service.publishPipeline(ctx, conversationID, "extracting", detail)
	}
}

// pipelineProgress maps the current phase and the ready/target ratio to a
// 0–100 percentage. The extracting phase covers the longest stretch of work
// (fetch + extract + embed for every URL), so it gets the widest band.
func pipelineProgress(status string, readyCount, target int) int {
	switch status {
	case "searching":
		return 5
	case "extracting":
		if target <= 0 {
			return 15
		}
		ratio := float64(readyCount) / float64(target)
		if ratio > 1 {
			ratio = 1
		}
		return 15 + int(ratio*70)
	case "ranking":
		return 90
	case "ready":
		return 100
	case "cancelled":
		return 100
	case "error":
		return 100
	default:
		return 0
	}
}

func (service *SummarizeService) publishPipeline(ctx context.Context, conversationID int64, status, detail string) {
	readyCount, _ := service.conversations.CountReadySummaries(ctx, conversationID)
	service.events.Publish(conversationID, "pipeline", PipelineEvent{
		Status:     status,
		Detail:     detail,
		ReadyCount: readyCount,
		Target:     service.urlLimit,
		Progress:   pipelineProgress(status, readyCount, service.urlLimit),
	})
}


func containsDocument(documents []PageDocument, url string) bool {
	for _, document := range documents {
		if document.URL == url {
			return true
		}
	}
	return false
}

func cosineSimilarity(left, right []float64) float64 {
	if len(left) == 0 || len(right) == 0 || len(left) != len(right) {
		return 0
	}

	dot := 0.0
	leftNorm := 0.0
	rightNorm := 0.0
	for index := range left {
		dot += left[index] * right[index]
		leftNorm += left[index] * left[index]
		rightNorm += right[index] * right[index]
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / (sqrt(leftNorm) * sqrt(rightNorm))
}

func sqrt(value float64) float64 {
	if value <= 0 {
		return 0
	}
	z := value
	for iteration := 0; iteration < 8; iteration++ {
		z -= (z*z - value) / (2 * z)
	}
	return z
}
