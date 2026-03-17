package main

import (
    "context"
    "database/sql"
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
    Addr                string
    SearchURL           string
    LlamaURL            string
    DBPath              string
    SchemaPath          string
    TemplateGlob        string
    StaticDir           string
    ModelsDir           string
    CurrentModelPath    string
    LogsPath            string
    TrafilaturaPath     string
    SummarizeURLLimit   int
    MaxExtractChars     int
    FetchWorkers        int
    SummaryWorkers      int
    SummaryQueueSize    int
    ChatContextChars    int
    MaxChatMessages     int
    LLMMaxResponseTokens int
    LLMContextTokens    int
    AllowAnonymous      bool
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
    AppName       string
    UserID        string
    Conversations []ConversationListItem
    Conversation  *ConversationView
    Query         string
    Models        []ModelInfo
    CurrentModel  string
    Error         string
    Status        string
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
    }).ParseGlob(cfg.TemplateGlob)
    if err != nil {
        logger.Error("failed to parse templates", "error", err, "glob", cfg.TemplateGlob)
        os.Exit(1)
    }

    conversations := &ConversationService{db: db, logger: logger}
    llm := &LLMService{
        baseURL:           cfg.LlamaURL,
        client:            &http.Client{Timeout: 90 * time.Second},
        logger:            logger,
        maxResponseTokens: cfg.LLMMaxResponseTokens,
        contextTokens:     cfg.LLMContextTokens,
    }
    fetchService := NewFetchService(logger, cfg.TrafilaturaPath, cfg.FetchWorkers, cfg.MaxExtractChars)
    memoryService := &MemoryService{db: db, llm: llm, conversations: conversations, logger: logger}
    summarizeService := &SummarizeService{
        conversations: conversations,
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
        search:        &SearchService{baseURL: cfg.SearchURL, client: &http.Client{Timeout: 20 * time.Second}},
        fetch:         fetchService,
        llm:           llm,
        conversations: conversations,
        memory:        memoryService,
        summarize:     summarizeService,
        summaryJobs:   make(chan SummaryJob, cfg.SummaryQueueSize),
    }

    app.summarize.StartWorkers(app.summaryJobs, cfg.SummaryWorkers)

    server := &http.Server{
        Addr:              cfg.Addr,
        Handler:           app.routes(),
        ReadHeaderTimeout: 10 * time.Second,
        ReadTimeout:       30 * time.Second,
        WriteTimeout:      120 * time.Second,
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

func friendlySiteName(host string) string {
    specialHosts := map[string]string{
        "arxiv.org":         "arXiv",
        "github.com":        "GitHub",
        "docs.github.com":   "GitHub Docs",
        "medium.com":        "Medium",
        "news.ycombinator.com": "Hacker News",
        "stackoverflow.com": "Stack Overflow",
        "wikipedia.org":     "Wikipedia",
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
    mux.HandleFunc("/models", app.handleModelsPage)
    mux.HandleFunc("/models/select", app.handleModelSelect)
    mux.HandleFunc("/models/download", app.handleModelDownload)

    return withMiddlewares(mux, app.logger, app.cfg.AllowAnonymous)
}

func (app *App) render(w http.ResponseWriter, name string, data PageData) {
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    if err := app.templates.ExecuteTemplate(w, name, data); err != nil {
        app.logger.Error("template rendering failed", "error", err, "template", name)
        http.Error(w, "template rendering failed", http.StatusInternalServerError)
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
    return Config{
        Addr:                 envOrDefault("BAP_ADDR", ":8081"),
        SearchURL:            envOrDefault("SEARXNG_SEARCH_URL", "http://searxng:8080/search"),
        LlamaURL:             envOrDefault("LLAMA_CPP_URL", "http://llama:8080/v1/chat/completions"),
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
        FetchWorkers:         envOrDefaultInt("BAP_FETCH_WORKERS", 3),
        SummaryWorkers:       envOrDefaultInt("BAP_SUMMARY_WORKERS", 1),
        SummaryQueueSize:     envOrDefaultInt("BAP_SUMMARY_QUEUE", 32),
        ChatContextChars:     envOrDefaultInt("BAP_CHAT_CONTEXT_CHARS", 4200),
        MaxChatMessages:      envOrDefaultInt("BAP_MAX_CHAT_MESSAGES", 8),
        LLMMaxResponseTokens: envOrDefaultInt("BAP_LLM_MAX_TOKENS", 700),
        LLMContextTokens:     envOrDefaultInt("BAP_LLM_CONTEXT_TOKENS", 2048),
        AllowAnonymous:       envOrDefault("BAP_ALLOW_ANONYMOUS", "true") == "true",
    }
}

func applySchema(db *sql.DB, schemaPath string) error {
    schema, err := os.ReadFile(schemaPath)
    if err != nil {
        return err
    }
    _, err = db.Exec(string(schema))
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
