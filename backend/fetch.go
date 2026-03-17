package main

import (
    "bytes"
    "context"
    "fmt"
    "io"
    "log/slog"
    "net/http"
    "os/exec"
    "strings"
    "sync"
    "time"
)

type PageDocument struct {
    URL   string
    Title string
    Text  string
}

type cachedPage struct {
    text      string
    expiresAt time.Time
}

type PageCache struct {
    mu    sync.RWMutex
    items map[string]cachedPage
}

type FetchService struct {
    logger          *slog.Logger
    trafilaturaPath string
    workerCount     int
    maxExtractChars int
    client          *http.Client
    cache           *PageCache
}

func NewFetchService(logger *slog.Logger, trafilaturaPath string, workerCount, maxExtractChars int) *FetchService {
    return &FetchService{
        logger:          logger,
        trafilaturaPath: trafilaturaPath,
        workerCount:     workerCount,
        maxExtractChars: maxExtractChars,
        client: &http.Client{
            Timeout: 20 * time.Second,
        },
        cache: &PageCache{items: map[string]cachedPage{}},
    }
}

func (service *FetchService) FetchAndExtract(ctx context.Context, meta RequestMeta, results []SearchResult) []PageDocument {
    jobs := make(chan SearchResult)
    output := make(chan PageDocument, len(results))
    var workers sync.WaitGroup

    for index := 0; index < service.workerCount; index++ {
        workers.Add(1)
        go func() {
            defer workers.Done()
            for result := range jobs {
                document, err := service.fetchDocument(ctx, meta, result)
                if err != nil {
                    loggerWithMeta(ctx, service.logger, meta.ConversationID).Error("page processing failed", "url", result.URL, "error", err)
                    continue
                }
                output <- document
            }
        }()
    }

    go func() {
        for _, result := range results {
            jobs <- result
        }
        close(jobs)
        workers.Wait()
        close(output)
    }()

    documents := make([]PageDocument, 0, len(results))
    for document := range output {
        documents = append(documents, document)
    }
    return documents
}

func (service *FetchService) fetchDocument(ctx context.Context, meta RequestMeta, result SearchResult) (PageDocument, error) {
    if cached, ok := service.cacheGet(result.URL); ok {
        return PageDocument{URL: result.URL, Title: result.Title, Text: cached}, nil
    }

    req, err := http.NewRequestWithContext(ctx, http.MethodGet, result.URL, nil)
    if err != nil {
        return PageDocument{}, err
    }
    req.Header.Set("User-Agent", "bap-search/0.1")

    response, err := service.client.Do(req)
    if err != nil {
        return PageDocument{}, err
    }
    defer response.Body.Close()

    if response.StatusCode >= http.StatusBadRequest {
        return PageDocument{}, fmt.Errorf("status %d", response.StatusCode)
    }

    rawBody, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
    if err != nil {
        return PageDocument{}, err
    }

    loggerWithMeta(ctx, service.logger, meta.ConversationID).Info("url_fetched",
        "url", result.URL,
        "html_bytes", len(rawBody),
    )

    text, err := service.extractText(ctx, rawBody)
    if err != nil {
        return PageDocument{}, err
    }

    loggerWithMeta(ctx, service.logger, meta.ConversationID).Info("text_extracted",
        "url", result.URL,
        "text_bytes", len(text),
    )

    service.cachePut(result.URL, text)
    return PageDocument{URL: result.URL, Title: result.Title, Text: text}, nil
}

func (service *FetchService) extractText(ctx context.Context, rawHTML []byte) (string, error) {
    command := exec.CommandContext(ctx, service.trafilaturaPath)
    command.Stdin = bytes.NewReader(rawHTML)

    var stdout bytes.Buffer
    var stderr bytes.Buffer
    command.Stdout = &stdout
    command.Stderr = &stderr

    if err := command.Run(); err != nil {
        return "", fmt.Errorf("trafilatura failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
    }

    text := strings.TrimSpace(stdout.String())
    if len(text) > service.maxExtractChars {
        text = text[:service.maxExtractChars]
    }
    return text, nil
}

func (service *FetchService) cacheGet(url string) (string, bool) {
    service.cache.mu.RLock()
    defer service.cache.mu.RUnlock()

    item, ok := service.cache.items[url]
    if !ok || time.Now().After(item.expiresAt) {
        return "", false
    }
    return item.text, true
}

func (service *FetchService) cachePut(url, text string) {
    service.cache.mu.Lock()
    defer service.cache.mu.Unlock()
    service.cache.items[url] = cachedPage{text: text, expiresAt: time.Now().Add(30 * time.Minute)}
}
