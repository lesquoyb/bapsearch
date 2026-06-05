package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

type PageDocument struct {
	Rank  int
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
	logger            *slog.Logger
	trafilaturaPath   string
	trafilaturaURL    string
	workerCount       int
	maxExtractChars   int
	client            *http.Client
	trafilaturaClient *http.Client
	serviceWarnOnce   sync.Once
	cache             *PageCache
}

func NewFetchService(logger *slog.Logger, trafilaturaPath, trafilaturaURL string, workerCount, maxExtractChars int) *FetchService {
	return &FetchService{
		logger:          logger,
		trafilaturaPath: trafilaturaPath,
		trafilaturaURL:  strings.TrimSpace(trafilaturaURL),
		workerCount:     workerCount,
		maxExtractChars: maxExtractChars,
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
		trafilaturaClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		cache: &PageCache{items: map[string]cachedPage{}},
	}
}

// FetchAndExtractChan fetches and extracts pages concurrently, yielding each
// document as soon as it is ready. The returned channel is closed when all
// workers finish. Callers must drain the channel fully.
func (service *FetchService) FetchAndExtractChan(ctx context.Context, meta RequestMeta, results []SearchResult, onStatus func(url, status, detail string)) <-chan PageDocument {
	jobs := make(chan SearchResult)
	output := make(chan PageDocument, len(results))
	var workers sync.WaitGroup

	for index := 0; index < service.workerCount; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for result := range jobs {
				document, err := service.fetchDocument(ctx, meta, result, onStatus)
				if err != nil {
					if onStatus != nil {
						onStatus(result.URL, "error", err.Error())
					}
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

	return output
}

func (service *FetchService) FetchAndExtract(ctx context.Context, meta RequestMeta, results []SearchResult, onStatus func(url, status, detail string)) []PageDocument {
	ch := service.FetchAndExtractChan(ctx, meta, results, onStatus)
	documents := make([]PageDocument, 0, len(results))
	for document := range ch {
		documents = append(documents, document)
	}
	sort.Slice(documents, func(i, j int) bool {
		return documents[i].Rank < documents[j].Rank
	})
	return documents
}

func (service *FetchService) fetchDocument(ctx context.Context, meta RequestMeta, result SearchResult, onStatus func(url, status, detail string)) (PageDocument, error) {
	if cached, ok := service.cacheGet(result.URL); ok {
		if onStatus != nil {
			onStatus(result.URL, "cleaning", "Using cached extracted content.")
		}
		return PageDocument{Rank: result.Rank, URL: result.URL, Title: result.Title, Text: cached}, nil
	}

	if onStatus != nil {
		onStatus(result.URL, "fetching", "Downloading source content.")
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
	if onStatus != nil {
		onStatus(result.URL, "cleaning", "Cleaning and extracting page content.")
	}

	text, err := service.extractText(ctx, rawBody)
	if err != nil {
		return PageDocument{}, err
	}

	loggerWithMeta(ctx, service.logger, meta.ConversationID).Info("text_extracted",
		"url", result.URL,
		"text_bytes", len(text),
	)

	service.cachePut(result.URL, text)
	return PageDocument{Rank: result.Rank, URL: result.URL, Title: result.Title, Text: text}, nil
}

// extractText turns raw HTML into clean main text. It prefers the long-lived
// trafilatura HTTP service (no per-page Python startup) and falls back to the
// trafilatura CLI subprocess if that service is unavailable.
func (service *FetchService) extractText(ctx context.Context, rawHTML []byte) (string, error) {
	if service.trafilaturaURL != "" {
		text, err := service.extractViaService(ctx, rawHTML)
		if err == nil {
			return service.capExtract(text), nil
		}
		service.serviceWarnOnce.Do(func() {
			service.logger.Warn("trafilatura service unavailable; falling back to the CLI per page",
				"url", service.trafilaturaURL, "error", err)
		})
	}
	text, err := service.extractViaCLI(ctx, rawHTML)
	if err != nil {
		return "", err
	}
	return service.capExtract(text), nil
}

// capExtract trims and bounds extracted text to maxExtractChars, cutting on rune
// boundaries so the last character is never split.
func (service *FetchService) capExtract(text string) string {
	text = strings.TrimSpace(text)
	if service.maxExtractChars <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) > service.maxExtractChars {
		return string(runes[:service.maxExtractChars])
	}
	return text
}

func (service *FetchService) extractViaService(ctx context.Context, rawHTML []byte) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, service.trafilaturaURL, bytes.NewReader(rawHTML))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/html; charset=utf-8")
	resp, err := service.trafilaturaClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("trafilatura service returned status %d", resp.StatusCode)
	}
	return string(body), nil
}

func (service *FetchService) extractViaCLI(ctx context.Context, rawHTML []byte) (string, error) {
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(tctx, service.trafilaturaPath)
	command.Stdin = bytes.NewReader(rawHTML)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		return "", fmt.Errorf("trafilatura failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
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

func (service *FetchService) Invalidate(urls []string) {
	service.cache.mu.Lock()
	defer service.cache.mu.Unlock()

	for _, url := range urls {
		delete(service.cache.items, url)
	}
}
