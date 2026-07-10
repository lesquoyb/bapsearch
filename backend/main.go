package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

type Config struct {
	Addr                 string
	SearchURL            string
	LlamaURL             string
	EmbeddingsURL        string
	// LLMProvider selects how model status is probed: "llamacpp" (default,
	// the bundled containers) or "ollama" (an external Ollama instance the
	// OpenAI-compatible endpoints point at).
	LLMProvider          string
	AnswerModelName      string
	EmbedModelName       string
	DBPath               string
	SchemaPath           string
	TemplateGlob         string
	StaticDir            string
	ModelsDir            string
	CurrentModelPath     string
	LogsPath             string
	LLMLogsPath          string
	TrafilaturaPath      string
	TrafilaturaURL       string
	FetchRescueURL       string
	SummarizeURLLimit    int
	MaxExtractChars      int
	MaxEmbeddingTokens    int
	EmbeddingBatchSize   int
	FetchTimeoutSeconds  int
	FetchWorkers         int
	SearchWorkers        int
	SummaryWorkers       int
	SummaryQueueSize     int
	ContextDocCount      int
	ChatContextChars     int
	MaxChatMessages      int
	ResultsDisplayLimit  int
	LLMMaxResponseTokens int
	LLMContextTokens     int
	AllowAnonymous       bool
	SessionSecret        string
	QueryReformulations  int
}

type App struct {
	cfg           Config
	logger        *slog.Logger
	db            *sql.DB
	templates     *template.Template
	search        *SearchService
	fetch         *FetchService
	llm           *LLMService
	conversations *ConversationService
	memory        *MemoryService
	summarize     *SummarizeService
	searchJobs    chan SummaryJob
	processJobs   chan ProcessJob
	events        *EventBroker
	cancellations *Cancellations

	modelDownloadMu sync.Mutex
	modelDownload   ModelDownloadStatus
}

type PageData struct {
	AppName        string
	UserID         string
	Conversations  []ConversationListItem
	Conversation   *ConversationView
	Query          string
	Models         []ModelInfo
	CurrentModel   string
	EmbeddingModel string
	Error          string
	Status         string
	Prompts        map[string]string
	Settings       map[string]string // <-- expose all settings for the template
	UserMemory     string
	StarterPresets []StarterPreset
}

func main() {
	cfg := loadConfig()

	logger, closeLogger, err := newJSONLogger(cfg.LogsPath)
	if err != nil {
		panic(err)
	}
	defer closeLogger()

	llmLogger, closeLLMLogger, err := newFileJSONLogger(cfg.LLMLogsPath)
	if err != nil {
		// Non-fatal: fall back to the general logger so we still capture
		// prompts/responses in /logs/backend.jsonl.
		logger.Warn("failed to open LLM log file, falling back to general logger", "path", cfg.LLMLogsPath, "error", err)
		llmLogger = logger
	} else {
		defer closeLLMLogger()
	}

	if cfg.SessionSecret == "" {
		cfg.SessionSecret = generateSessionSecret()
		logger.Warn("no BAP_SESSION_SECRET set – generated a random one (sessions will not survive restarts)")
	}

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		logger.Error("failed to create database directory", "error", err)
		os.Exit(1)
	}

	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		logger.Error("failed to open sqlite database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	db.SetConnMaxLifetime(0)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := applySchema(db, cfg.SchemaPath); err != nil {
		logger.Error("failed to apply schema", "error", err)
		os.Exit(1)
	}

	templates, err := template.New("root").Funcs(template.FuncMap{
		"formatBytes": func(value int64) string {
			if value < 0 {
				return "?"
			}

			units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
			v := float64(value)
			idx := 0
			for v >= 1000.0 && idx < len(units)-1 {
				v /= 1000.0
				idx++
			}
			if idx == 0 {
				return fmt.Sprintf("%d B", value)
			}
			// One decimal when < 10 for readability, otherwise whole.
			if v < 10 {
				return fmt.Sprintf("%.1f %s", v, units[idx])
			}
			return fmt.Sprintf("%.0f %s", v, units[idx])
		},
		"markdown": func(value string) template.HTML {
			return renderMarkdown(value)
		},
		"markdownWithSources": func(value string, conversation ConversationView) template.HTML {
			return renderMarkdownWithSources(value, conversation)
		},
		"siteName": func(rawURL string) string {
			parsed, err := url.Parse(strings.TrimSpace(rawURL))
			if err != nil {
				return "Unknown source"
			}

			host := strings.TrimPrefix(parsed.Hostname(), "www.")
			if host == "" {
				return "Unknown source"
			}

			if label := friendlySiteName(host); label != "" {
				return label
			}

			return host
		},
		"faviconURL": func(rawURL string) string {
			trimmed := strings.TrimSpace(rawURL)
			if trimmed == "" {
				return ""
			}
			return "https://www.google.com/s2/favicons?sz=64&domain_url=" + url.QueryEscape(trimmed)
		},
		"formatTime": func(value time.Time) string {
			if value.IsZero() {
				return ""
			}
			return value.Format("2006-01-02 15:04")
		},
		"truncate": func(value string, limit int) string {
			value = strings.TrimSpace(value)
			if len(value) <= limit {
				return value
			}
			return value[:limit] + "..."
		},
		"engineStatusLabel": func(status SearchEngineStatus) string {
			switch status.Status {
			case "ok":
				if status.ResultCount > 0 {
					return fmt.Sprintf("%s %d", status.Engine, status.ResultCount)
				}
				return status.Engine
			case "timeout":
				return status.Engine + " timeout"
			default:
				return status.Engine + " error"
			}
		},
		"isSearchQueryMsg": func(msg MessageRecord) bool {
			return msg.Role == "system" && strings.HasPrefix(msg.Content, "search_query:")
		},
		"searchQueryText": func(msg MessageRecord) string {
			return strings.TrimPrefix(msg.Content, "search_query:")
		},
		"uniqueQueryTexts": func(results []SearchResult) []string {
			seen := make(map[string]bool)
			var out []string
			for _, r := range results {
				if r.QueryText != "" && !seen[r.QueryText] {
					seen[r.QueryText] = true
					out = append(out, r.QueryText)
				}
			}
			return out
		},
		"countByQueryText": func(results []SearchResult, qt string) int {
			n := 0
			for _, r := range results {
				if r.QueryText == qt {
					n++
				}
			}
			return n
		},
		"slice": func(items ...string) []string {
			if len(items) == 0 {
				return []string{}
			}
			return items
		},
		"append": func(slice []string, item string) []string {
			return append(slice, item)
		},
	}).ParseGlob(cfg.TemplateGlob)
	if err != nil {
		logger.Error("failed to parse templates", "error", err, "glob", cfg.TemplateGlob)
		os.Exit(1)
	}

	conversations := &ConversationService{db: db, logger: logger, summaryTarget: cfg.SummarizeURLLimit}
	llm := &LLMService{
		baseURL:           cfg.LlamaURL,
		embeddingsURL:     cfg.EmbeddingsURL,
		answerModel:       cfg.AnswerModelName,
		embedModel:        cfg.EmbedModelName,
		client:            &http.Client{Timeout: 10 * time.Minute},
		logger:            logger,
		llmLogger:         llmLogger,
		maxResponseTokens: cfg.LLMMaxResponseTokens,
		contextTokens:     cfg.LLMContextTokens,
		maxEmbeddingTokens: cfg.MaxEmbeddingTokens,
		embeddingBatchSize: cfg.EmbeddingBatchSize,
		enableThinking:    false,
		reasoningBudget:   2048,
		temperature:       0.2,
		topP:              1.0,
		topK:              40,
	}
	fetchService := NewFetchService(logger, cfg.TrafilaturaPath, cfg.TrafilaturaURL, cfg.FetchRescueURL, cfg.FetchWorkers, cfg.MaxExtractChars, cfg.FetchTimeoutSeconds)
	memoryService := &MemoryService{db: db, llm: llm, conversations: conversations, logger: logger}
	eventBroker := NewEventBroker()
	cancellations := NewCancellations()

	summarizeService := &SummarizeService{
		conversations:       conversations,
		search:              &SearchService{baseURL: cfg.SearchURL, client: &http.Client{Timeout: 20 * time.Second}},
		fetch:               fetchService,
		llm:                 llm,
		memory:              memoryService,
		logger:              logger,
		urlLimit:            cfg.SummarizeURLLimit,
		queryReformulations: cfg.QueryReformulations,
		events:              eventBroker,
		cancellations:       cancellations,
	}

	app := &App{
		cfg:           cfg,
		logger:        logger,
		db:            db,
		templates:     templates,
		search:        summarizeService.search,
		fetch:         fetchService,
		llm:           llm,
		conversations: conversations,
		memory:        memoryService,
		summarize:     summarizeService,
		searchJobs:    make(chan SummaryJob, cfg.SummaryQueueSize),
		processJobs:   make(chan ProcessJob, cfg.SummaryQueueSize),
		events:        eventBroker,
		cancellations: cancellations,
	}

	app.summarize.StartWorkers(app.searchJobs, app.processJobs, cfg.SearchWorkers, cfg.SummaryWorkers)
	app.loadPromptsFromDB()
	app.loadSettingsFromDB()

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      15 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.Info("backend listening", "addr", cfg.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-sigCtx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

func (app *App) loadPromptsFromDB() {
	ctx := context.Background()
	c := app.conversations.GetSetting(ctx, "prompt_chat", "")
	m := app.conversations.GetSetting(ctx, "prompt_memory", "")
	app.llm.Prompts.Set(c, m)
}

func (app *App) loadSettingsFromDB() {
	ctx := context.Background()

	if v := app.conversations.GetSetting(ctx, "enable_thinking", ""); v != "" {
		app.llm.enableThinking = v == "true"
	}
	if v := app.conversations.GetSetting(ctx, "reasoning_budget", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			app.llm.reasoningBudget = n
		}
	}
	if v := app.conversations.GetSetting(ctx, "temperature", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			app.llm.temperature = f
		}
	}
	if v := app.conversations.GetSetting(ctx, "top_p", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			app.llm.topP = f
		}
	}
	if v := app.conversations.GetSetting(ctx, "top_k", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			app.llm.topK = n
		}
	}
	if v := app.conversations.GetSetting(ctx, "max_tokens", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			app.llm.maxResponseTokens = n
		}
	}
	if v := app.conversations.GetSetting(ctx, "context_doc_count", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			app.cfg.ContextDocCount = n
		}
	}
	if v := app.conversations.GetSetting(ctx, "chat_context_chars", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			app.cfg.ChatContextChars = n
		}
	}
	if v := app.conversations.GetSetting(ctx, "max_chat_messages", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			app.cfg.MaxChatMessages = n
		}
	}
	if v := app.conversations.GetSetting(ctx, "results_display_limit", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			app.cfg.ResultsDisplayLimit = n
		}
	}
	if v := app.conversations.GetSetting(ctx, "summarize_url_limit", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			app.cfg.SummarizeURLLimit = n
			app.summarize.urlLimit = n
			app.conversations.summaryTarget = n
		}
	}
	if v := app.conversations.GetSetting(ctx, "max_extract_chars", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			app.cfg.MaxExtractChars = n
			app.fetch.maxExtractChars = n
		}
	}
	if v := app.conversations.GetSetting(ctx, "fetch_workers", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			app.cfg.FetchWorkers = n
			app.fetch.workerCount = n
		}
	}
	if v := app.conversations.GetSetting(ctx, "max_embedding_tokens", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			app.cfg.MaxEmbeddingTokens = n
			app.llm.maxEmbeddingTokens = n
		}
	}
	if v := app.conversations.GetSetting(ctx, "embedding_batch_size", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			app.llm.embeddingBatchSize = n
		}
	}
	if v := app.conversations.GetSetting(ctx, "query_reformulations", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			app.cfg.QueryReformulations = n
			app.summarize.queryReformulations = n
		}
	} else {
		// Cleared from the settings page: fall back to the env default.
		fallback := envOrDefaultInt("BAP_QUERY_REFORMULATIONS", 0)
		app.cfg.QueryReformulations = fallback
		app.summarize.queryReformulations = fallback
	}

	// Per-call-type sampling overrides. Each profile pulls three optional
	// keys; zero/empty leaves the global temperature/top_p/top_k in effect.
	profiles := []CallProfile{ProfileAnswer, ProfileRewrite, ProfileUtility}
	overrides := map[CallProfile]SamplingParams{}
	for _, p := range profiles {
		var override SamplingParams
		if v := app.conversations.GetSetting(ctx, string(p)+"_temperature", ""); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				override.Temperature = f
			}
		}
		if v := app.conversations.GetSetting(ctx, string(p)+"_top_p", ""); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				override.TopP = f
			}
		}
		if v := app.conversations.GetSetting(ctx, string(p)+"_top_k", ""); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				override.TopK = n
			}
		}
		if override.Temperature != 0 || override.TopP != 0 || override.TopK != 0 {
			overrides[p] = override
		}
	}
	if len(overrides) > 0 {
		app.llm.profileSampling = overrides
	} else {
		app.llm.profileSampling = nil
	}
}

func friendlySiteName(host string) string {
	specialHosts := map[string]string{
		"arxiv.org":            "arXiv",
		"github.com":           "GitHub",
		"docs.github.com":      "GitHub Docs",
		"medium.com":           "Medium",
		"news.ycombinator.com": "Hacker News",
		"stackoverflow.com":    "Stack Overflow",
		"wikipedia.org":        "Wikipedia",
	}

	if label, ok := specialHosts[host]; ok {
		return label
	}

	parts := strings.Split(host, ".")
	if len(parts) == 0 {
		return ""
	}

	labelIndex := len(parts) - 2
	if len(parts) >= 3 && len(parts[len(parts)-1]) == 2 && len(parts[len(parts)-2]) <= 3 {
		labelIndex = len(parts) - 3
	}
	if labelIndex < 0 || labelIndex >= len(parts) {
		labelIndex = 0
	}

	candidate := parts[labelIndex]
	if candidate == "" {
		return ""
	}

	if candidate == "wikipedia" {
		return "Wikipedia"
	}

	words := strings.FieldsFunc(candidate, func(value rune) bool {
		return value == '-' || value == '_'
	})
	if len(words) == 0 {
		words = []string{candidate}
	}

	for index, word := range words {
		words[index] = capitalizeWord(word)
	}

	return strings.Join(words, " ")
}

func capitalizeWord(value string) string {
	if value == "" {
		return value
	}

	lower := strings.ToLower(value)
	runes := []rune(lower)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func (app *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(app.cfg.StaticDir))))
	mux.HandleFunc("/", app.handleIndex)
	mux.HandleFunc("/healthz", app.handleHealth)
	mux.HandleFunc("/search", app.handleSearch)
	mux.HandleFunc("/conversations/", app.handleConversationRoutes)
	mux.HandleFunc("/settings", app.handleSettingsPage)
	mux.HandleFunc("/settings/preset", app.handleSettingsPreset)
	mux.HandleFunc("/settings/download", app.handleModelDownload)
	mux.HandleFunc("/settings/download-status", app.handleModelDownloadStatus)
	mux.HandleFunc("/memory", app.handleMemoryPage)
	mux.HandleFunc("/llama-status", app.handleLlamaStatus)
	mux.HandleFunc("/login", app.handleLoginPage)
	mux.HandleFunc("/register", app.handleRegisterPage)
	mux.HandleFunc("/logout", app.handleLogout)

	return withMiddlewares(mux, app.logger, app.cfg.AllowAnonymous, app.cfg.SessionSecret)
}

func (app *App) render(w http.ResponseWriter, name string, data PageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := app.templates.ExecuteTemplate(w, name, data); err != nil {
		app.logger.Error("template rendering failed", "error", err, "template", name)
		http.Error(w, "template rendering failed", http.StatusInternalServerError)
	}
}

func (app *App) handleLlamaStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	role := normalizeModelRole(r.URL.Query().Get("role"))
	expectedModel := app.currentModelNameForRole(role)

	type responsePayload struct {
		Role          string `json:"role"`
		Status        string `json:"status"`
		ExpectedModel string `json:"expected_model,omitempty"`
		LoadedModel   string `json:"loaded_model,omitempty"`
		Detail        string `json:"detail,omitempty"`
	}

	// An external Ollama instance speaks its own management API (/api/*) and
	// routes by payload model name, so probe it accordingly.
	if app.cfg.LLMProvider == "ollama" {
		expected := app.cfg.AnswerModelName
		if role == modelRoleEmbeddings {
			expected = app.cfg.EmbedModelName
		}
		status, loaded, detail := app.ollamaStatus(r.Context(), role, expected)
		json.NewEncoder(w).Encode(responsePayload{Role: role, Status: status, ExpectedModel: expected, LoadedModel: loaded, Detail: detail})
		return
	}

	parsed, err := url.Parse(app.llamaURLForRole(role))
	if err != nil {
		json.NewEncoder(w).Encode(responsePayload{Role: role, Status: "error", ExpectedModel: expectedModel, Detail: "invalid llama url"})
		return
	}
	baseURL := parsed.Scheme + "://" + parsed.Host

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		json.NewEncoder(w).Encode(responsePayload{Role: role, Status: "error", ExpectedModel: expectedModel, Detail: "failed to build health request"})
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		json.NewEncoder(w).Encode(responsePayload{Role: role, Status: "error", ExpectedModel: expectedModel, Detail: err.Error()})
		return
	}
	defer resp.Body.Close()

	var body struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)

	status := "error"
	detail := strings.TrimSpace(body.Status)
	switch {
	case body.Status == "ok":
		status = "loaded"
	case strings.Contains(body.Status, "load") || resp.StatusCode == http.StatusServiceUnavailable:
		status = "loading"
	}

	loadedModel := ""
	if status != "error" {
		modelsCtx, modelsCancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer modelsCancel()
		mReq, err := http.NewRequestWithContext(modelsCtx, http.MethodGet, baseURL+"/v1/models", nil)
		if err == nil {
			mResp, err := http.DefaultClient.Do(mReq)
			if err == nil {
				defer mResp.Body.Close()
				var payload struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				if err := json.NewDecoder(mResp.Body).Decode(&payload); err == nil {
					if len(payload.Data) > 0 {
						loadedModel = strings.TrimSpace(payload.Data[0].ID)
					}
				}
			}
		}
	}

	if status == "error" {
		if detail == "" {
			detail = "unhealthy"
		}
		if resp.StatusCode >= http.StatusBadRequest {
			detail = fmt.Sprintf("%s (HTTP %d)", detail, resp.StatusCode)
		}
	}

	// If the expected model (file written by the UI) doesn't match what
	// llama.cpp currently has loaded, surface that explicitly so the UI can
	// render "Reloading…" instead of pretending the new model is already
	// serving requests.
	if status == "loaded" && expectedModel != "" && loadedModel != "" {
		expBase := strings.ToLower(strings.TrimSpace(expectedModel))
		loadBase := strings.ToLower(strings.TrimSpace(loadedModel))
		// Compare the basename without extension to tolerate path differences
		// between the file content (basename) and llama's /v1/models id.
		if expBase != loadBase &&
			!strings.HasSuffix(expBase, loadBase) &&
			!strings.HasSuffix(loadBase, expBase) {
			status = "reloading"
			if detail == "" {
				detail = "llama.cpp is still serving the previous model"
			}
		}
	}

	json.NewEncoder(w).Encode(responsePayload{Role: role, Status: status, ExpectedModel: expectedModel, LoadedModel: loadedModel, Detail: detail})
}

// ollamaStatus probes an Ollama server: reachability via /api/version, model
// availability via /api/tags, and in-memory state via /api/ps. A model that
// is installed but not resident is fine — Ollama loads it on the first call.
func (app *App) ollamaStatus(ctx context.Context, role, expected string) (status, loadedModel, detail string) {
	parsed, err := url.Parse(app.llamaURLForRole(role))
	if err != nil {
		return "error", "", "invalid endpoint url"
	}
	base := parsed.Scheme + "://" + parsed.Host

	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	getJSON := func(path string, target any) error {
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, base+path, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= http.StatusBadRequest {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return json.NewDecoder(resp.Body).Decode(target)
	}

	var version struct {
		Version string `json:"version"`
	}
	if err := getJSON("/api/version", &version); err != nil {
		return "error", "", "ollama unreachable: " + err.Error()
	}

	// sameOllamaModel tolerates the implicit ":latest" tag.
	sameOllamaModel := func(a, b string) bool {
		a = strings.ToLower(strings.TrimSpace(a))
		b = strings.ToLower(strings.TrimSpace(b))
		return a == b || a == b+":latest" || a+":latest" == b
	}

	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := getJSON("/api/tags", &tags); err == nil && expected != "" && expected != "local" {
		found := false
		for _, m := range tags.Models {
			if sameOllamaModel(m.Name, expected) {
				found = true
				break
			}
		}
		if !found {
			return "error", "", fmt.Sprintf("model %q is not pulled in Ollama (try: ollama pull %s)", expected, expected)
		}
	}

	var ps struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := getJSON("/api/ps", &ps); err == nil {
		for _, m := range ps.Models {
			if sameOllamaModel(m.Name, expected) {
				return "loaded", m.Name, "ollama " + version.Version
			}
		}
	}
	return "loaded", "", "installed; loads on first call (ollama " + version.Version + ")"
}

func (app *App) llamaURLForRole(role string) string {
	switch normalizeModelRole(role) {
	case modelRoleEmbeddings:
		return app.cfg.EmbeddingsURL
	default:
		return app.cfg.LlamaURL
	}
}

func (app *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := app.db.PingContext(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}

	fmt.Fprint(w, "ok")
}

func loadConfig() Config {
	answerURL := envOrDefault("LLAMA_CPP_URL", "http://llama:8080/v1/chat/completions")
	return Config{
		Addr:                 envOrDefault("BAP_ADDR", ":8081"),
		SearchURL:            envOrDefault("SEARXNG_SEARCH_URL", "http://searxng:8080/search"),
		LlamaURL:             answerURL,
		EmbeddingsURL:        envOrDefault("LLAMA_CPP_EMBEDDINGS_URL", "http://llama:8080/v1/embeddings"),
		LLMProvider:          strings.ToLower(envOrDefault("BAP_LLM_PROVIDER", "llamacpp")),
		// llama.cpp ignores the payload model name; Ollama routes by it.
		AnswerModelName:      envOrDefault("BAP_ANSWER_MODEL", "local"),
		EmbedModelName:       envOrDefault("BAP_EMBEDDING_MODEL", "local"),
		DBPath:               envOrDefault("BAP_DB_PATH", "/database/bap-search.db"),
		SchemaPath:           envOrDefault("BAP_SCHEMA_PATH", "/app/database/schema.sql"),
		TemplateGlob:         envOrDefault("BAP_TEMPLATE_GLOB", "/app/ui/templates/*.html"),
		StaticDir:            envOrDefault("BAP_STATIC_DIR", "/app/ui/static"),
		ModelsDir:            envOrDefault("BAP_MODELS_DIR", "/models"),
		CurrentModelPath:     envOrDefault("BAP_CURRENT_MODEL_PATH", "/models/current-model.txt"),
		LogsPath:             envOrDefault("BAP_LOG_PATH", "/logs/backend.jsonl"),
		LLMLogsPath:          envOrDefault("BAP_LLM_LOG_PATH", "/logs/llm.jsonl"),
		TrafilaturaPath:      envOrDefault("TRAFILATURA_BIN", "trafilatura"),
		TrafilaturaURL:       envOrDefault("BAP_TRAFILATURA_URL", "http://127.0.0.1:8090/extract"),
		// Optional stealth-fetch sidecar (compose profile "fetch-rescue");
		// unreachable = graceful degradation, empty = fully disabled.
		FetchRescueURL:       envOrDefault("BAP_FETCH_RESCUE_URL", ""),
		SummarizeURLLimit:    envOrDefaultInt("BAP_SUMMARIZE_URL_LIMIT", 3),
		MaxExtractChars:      envOrDefaultInt("BAP_MAX_EXTRACT_CHARS", 6000),
		MaxEmbeddingTokens:    envOrDefaultInt("BAP_MAX_EMBEDDING_TOKENS", 500),
		// Batching documents into one embeddings call removes most of the
		// per-call overhead; EmbedTexts falls back to per-item embedding when a
		// batch fails, so a larger default is safe even on small servers.
		EmbeddingBatchSize:   envOrDefaultInt("BAP_EMBEDDING_BATCH_SIZE", 4),
		FetchTimeoutSeconds:  envOrDefaultInt("BAP_FETCH_TIMEOUT_SECONDS", 10),
		FetchWorkers:         envOrDefaultInt("BAP_FETCH_WORKERS", 3),
		SearchWorkers:        envOrDefaultInt("BAP_SEARCH_WORKERS", 4),
		SummaryWorkers:       envOrDefaultInt("BAP_SUMMARY_WORKERS", 1),
		SummaryQueueSize:     envOrDefaultInt("BAP_SUMMARY_QUEUE", 32),
		ContextDocCount:      envOrDefaultInt("BAP_CONTEXT_DOC_COUNT", 5),
		ChatContextChars:     envOrDefaultInt("BAP_CHAT_CONTEXT_CHARS", 4200),
		MaxChatMessages:      envOrDefaultInt("BAP_MAX_CHAT_MESSAGES", 8),
		ResultsDisplayLimit:  envOrDefaultInt("BAP_RESULTS_DISPLAY_LIMIT", 10),
		LLMMaxResponseTokens: envOrDefaultInt("BAP_LLM_MAX_TOKENS", 700),
		LLMContextTokens:     envOrDefaultInt("BAP_LLM_CONTEXT_TOKENS", 8192),
		AllowAnonymous:       envOrDefault("BAP_ALLOW_ANONYMOUS", "true") == "true",
		SessionSecret:        envOrDefault("BAP_SESSION_SECRET", ""),
		QueryReformulations:  envOrDefaultInt("BAP_QUERY_REFORMULATIONS", 0),
	}
}

func applySchema(db *sql.DB, schemaPath string) error {
	schema, err := os.ReadFile(schemaPath)
	if err != nil {
		return err
	}
	if _, err = db.Exec(string(schema)); err != nil {
		return err
	}

	// Additive migrations for columns added after initial schema
	migrations := []string{
		`ALTER TABLE search_results ADD COLUMN query_text TEXT NOT NULL DEFAULT ''`,
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}

	return nil
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envOrDefaultInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}
