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
	"syscall"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

type Config struct {
	Addr                 string
	SearchURL            string
	LlamaURL             string
	RewriteLLMURL        string
	EmbeddingsURL        string
	DBPath               string
	SchemaPath           string
	TemplateGlob         string
	StaticDir            string
	ModelsDir            string
	CurrentModelPath     string
	LogsPath             string
	TrafilaturaPath      string
	SummarizeURLLimit    int
	MaxExtractChars      int
	MaxEmbeddingChars    int
	FetchWorkers         int
	SummaryWorkers       int
	SummaryQueueSize     int
	ContextDocCount      int
	ChatContextChars     int
	MaxChatMessages      int
	LLMMaxResponseTokens int
	LLMContextTokens     int
	AllowAnonymous       bool
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
	summaryJobs   chan SummaryJob
}

type PageData struct {
	AppName        string
	UserID         string
	Conversations  []ConversationListItem
	Conversation   *ConversationView
	Query          string
	Models         []ModelInfo
	CurrentModel   string
	RewriteModel   string
	EmbeddingModel string
	Error          string
	Status         string
	Prompts        map[string]string
	Settings       map[string]string // <-- expose all settings for the template
	UserMemory     string
}

func main() {
	cfg := loadConfig()

	logger, closeLogger, err := newJSONLogger(cfg.LogsPath)
	if err != nil {
		panic(err)
	}
	defer closeLogger()

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
				if status.Detail != "" {
					return status.Engine + " error"
				}
				return status.Engine + " error"
			}
		},
		"summaryStatusLabel": func(status string) string {
			switch strings.TrimSpace(status) {
			case "fetching":
				return "Fetching content"
			case "cleaning":
				return "Extracting content"
			case "embedding":
				return "Embedding content"
			case "ranking":
				return "Ranking result"
			case "skipped":
				return "Skipped"
			case "error":
				return "Error"
			case "ready":
				return "Ready"
			default:
				return "Unknown status"
			}
		},
		"rewriteStatusLabel": func(status string) string {
			switch strings.TrimSpace(status) {
			case "running":
				return "Rewriting query"
			case "succeeded":
				return "Rewrite succeeded"
			case "failed":
				return "Using original query"
			default:
				return "Rewrite pending"
			}
		},
	}).ParseGlob(cfg.TemplateGlob)
	if err != nil {
		logger.Error("failed to parse templates", "error", err, "glob", cfg.TemplateGlob)
		os.Exit(1)
	}

	conversations := &ConversationService{db: db, logger: logger, summaryTarget: cfg.SummarizeURLLimit}
	llm := &LLMService{
		baseURL:           cfg.LlamaURL,
		rewriteURL:        cfg.RewriteLLMURL,
		embeddingsURL:     cfg.EmbeddingsURL,
		client:            &http.Client{Timeout: 90 * time.Second},
		logger:            logger,
		maxResponseTokens: cfg.LLMMaxResponseTokens,
		contextTokens:     cfg.LLMContextTokens,
		maxEmbeddingChars: cfg.MaxEmbeddingChars,
		enableThinking:    true,
		reasoningBudget:   2048,
		temperature:       0.2,
		topP:              1.0,
		topK:              40,
	}
	fetchService := NewFetchService(logger, cfg.TrafilaturaPath, cfg.FetchWorkers, cfg.MaxExtractChars)
	memoryService := &MemoryService{db: db, llm: llm, conversations: conversations, logger: logger}
	summarizeService := &SummarizeService{
		conversations: conversations,
		search:        &SearchService{baseURL: cfg.SearchURL, client: &http.Client{Timeout: 20 * time.Second}},
		fetch:         fetchService,
		llm:           llm,
		memory:        memoryService,
		logger:        logger,
		urlLimit:      cfg.SummarizeURLLimit,
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
		summaryJobs:   make(chan SummaryJob, cfg.SummaryQueueSize),
	}

	app.summarize.StartWorkers(app.summaryJobs, cfg.SummaryWorkers)
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
	s, sy, c, m := app.conversations.GetSetting(ctx, "prompt_summarize", ""),
		app.conversations.GetSetting(ctx, "prompt_synthesize", ""),
		app.conversations.GetSetting(ctx, "prompt_chat", ""),
		app.conversations.GetSetting(ctx, "prompt_memory", "")
	app.llm.Prompts.Set(s, sy, c, m)
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
	// Route for the unified settings page.
	mux.HandleFunc("/settings", app.handleSettingsPage)
	mux.HandleFunc("/settings/save", app.handleSettingsSave)
	mux.HandleFunc("/settings/download", app.handleModelDownload)
	mux.HandleFunc("/memory", app.handleMemoryPage)
	mux.HandleFunc("/llama-status", app.handleLlamaStatus)

	return withMiddlewares(mux, app.logger, app.cfg.AllowAnonymous)
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

	parsed, err := url.Parse(app.llamaURLForRole(role))
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"role": role, "status": "error"})
		return
	}
	baseURL := parsed.Scheme + "://" + parsed.Host

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"role": role, "status": "error"})
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"role": role, "status": "error"})
		return
	}
	defer resp.Body.Close()

	var body struct {
		Status string `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&body)

	status := "error"
	switch {
	case body.Status == "ok":
		status = "loaded"
	case strings.Contains(body.Status, "load") || resp.StatusCode == http.StatusServiceUnavailable:
		status = "loading"
	}

	json.NewEncoder(w).Encode(map[string]string{"role": role, "status": status})
}

func (app *App) llamaURLForRole(role string) string {
	switch normalizeModelRole(role) {
	case modelRoleRewrite:
		return app.cfg.RewriteLLMURL
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
		RewriteLLMURL:        envOrDefault("LLAMA_CPP_REWRITE_URL", answerURL),
		EmbeddingsURL:        envOrDefault("LLAMA_CPP_EMBEDDINGS_URL", "http://llama:8080/v1/embeddings"),
		DBPath:               envOrDefault("BAP_DB_PATH", "/database/bap-search.db"),
		SchemaPath:           envOrDefault("BAP_SCHEMA_PATH", "/app/database/schema.sql"),
		TemplateGlob:         envOrDefault("BAP_TEMPLATE_GLOB", "/app/ui/templates/*.html"),
		StaticDir:            envOrDefault("BAP_STATIC_DIR", "/app/ui/static"),
		ModelsDir:            envOrDefault("BAP_MODELS_DIR", "/models"),
		CurrentModelPath:     envOrDefault("BAP_CURRENT_MODEL_PATH", "/models/current-model.txt"),
		LogsPath:             envOrDefault("BAP_LOG_PATH", "/logs/backend.jsonl"),
		TrafilaturaPath:      envOrDefault("TRAFILATURA_BIN", "trafilatura"),
		SummarizeURLLimit:    envOrDefaultInt("BAP_SUMMARIZE_URL_LIMIT", 3),
		MaxExtractChars:      envOrDefaultInt("BAP_MAX_EXTRACT_CHARS", 12000),
		MaxEmbeddingChars:    envOrDefaultInt("BAP_MAX_EMBEDDING_CHARS", 1800),
		FetchWorkers:         envOrDefaultInt("BAP_FETCH_WORKERS", 3),
		SummaryWorkers:       envOrDefaultInt("BAP_SUMMARY_WORKERS", 1),
		SummaryQueueSize:     envOrDefaultInt("BAP_SUMMARY_QUEUE", 32),
		ContextDocCount:      envOrDefaultInt("BAP_CONTEXT_DOC_COUNT", 5),
		ChatContextChars:     envOrDefaultInt("BAP_CHAT_CONTEXT_CHARS", 4200),
		MaxChatMessages:      envOrDefaultInt("BAP_MAX_CHAT_MESSAGES", 8),
		LLMMaxResponseTokens: envOrDefaultInt("BAP_LLM_MAX_TOKENS", 700),
		LLMContextTokens:     envOrDefaultInt("BAP_LLM_CONTEXT_TOKENS", 8192),
		AllowAnonymous:       envOrDefault("BAP_ALLOW_ANONYMOUS", "true") == "true",
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

	migrations := []struct {
		table      string
		column     string
		definition string
	}{
		{table: "conversations", column: "rewritten_query", definition: "TEXT NOT NULL DEFAULT ''"},
		{table: "conversations", column: "rewrite_status", definition: "TEXT NOT NULL DEFAULT 'pending'"},
		{table: "conversations", column: "rewrite_detail", definition: "TEXT NOT NULL DEFAULT ''"},
		{table: "conversations", column: "answer_status", definition: "TEXT NOT NULL DEFAULT 'pending'"},
		{table: "conversations", column: "answer_detail", definition: "TEXT NOT NULL DEFAULT ''"},
		{table: "search_results", column: "query_variant", definition: "TEXT NOT NULL DEFAULT 'original'"},
		{table: "summaries", column: "embedding_json", definition: "TEXT NOT NULL DEFAULT ''"},
		{table: "summaries", column: "similarity_score", definition: "REAL NOT NULL DEFAULT 0"},
		{table: "summaries", column: "rerank_position", definition: "INTEGER NOT NULL DEFAULT 0"},
		{table: "messages", column: "reasoning", definition: "TEXT NOT NULL DEFAULT ''"},
	}

	for _, migration := range migrations {
		if err := ensureColumn(db, migration.table, migration.column, migration.definition); err != nil {
			return err
		}
	}

	return nil
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name string
		var dataType string
		var notNull int
		var defaultValue sql.NullString
		var primaryKey int
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if strings.EqualFold(name, column) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition)
	return err
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
