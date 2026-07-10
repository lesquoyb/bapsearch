package main

import (
	"bytes"
	"context"
	"encoding/json"
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

	// rescueURL points at the optional stealth-fetch sidecar. When a page
	// fails or looks blocked (403, JS-challenge stub…), the URL is retried
	// there once; empty disables the fallback. rescueSem caps concurrent
	// rescues — each one holds a headless browser for seconds.
	rescueURL      string
	rescueClient   *http.Client
	rescueSem      chan struct{}
	rescueWarnOnce sync.Once
}

func NewFetchService(logger *slog.Logger, trafilaturaPath, trafilaturaURL, rescueURL string, workerCount, maxExtractChars, fetchTimeoutSeconds int) *FetchService {
	// One slow site used to hold the whole ranking phase for its full 20s
	// timeout; a shorter, configurable budget keeps the pipeline responsive
	// (the search snippet remains as fallback content for the slow page).
	if fetchTimeoutSeconds <= 0 {
		fetchTimeoutSeconds = 10
	}
	return &FetchService{
		logger:          logger,
		trafilaturaPath: trafilaturaPath,
		trafilaturaURL:  strings.TrimSpace(trafilaturaURL),
		workerCount:     workerCount,
		maxExtractChars: maxExtractChars,
		client: &http.Client{
			Timeout: time.Duration(fetchTimeoutSeconds) * time.Second,
		},
		trafilaturaClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		rescueURL: strings.TrimSpace(rescueURL),
		// The sidecar resolves a JS challenge in a real browser: allow well
		// beyond the normal fetch budget, but bound it.
		rescueClient: &http.Client{Timeout: 60 * time.Second},
		rescueSem:    make(chan struct{}, 2),
		cache:        &PageCache{items: map[string]cachedPage{}},
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

	rawBody, status, fetchErr := service.fetchHTML(ctx, result.URL)

	// A failed or blocked-looking fetch gets ONE retry through the optional
	// stealth sidecar, which resolves JS/anti-bot walls in a real browser.
	if fetchErr != nil || looksBlocked(status, rawBody) {
		if rescued, rescueErr := service.rescueFetch(ctx, meta, result.URL, onStatus); rescueErr == nil {
			rawBody = rescued
		} else if fetchErr != nil {
			return PageDocument{}, fetchErr
		} else if status >= http.StatusBadRequest {
			return PageDocument{}, fmt.Errorf("status %d", status)
		}
		// Otherwise keep the stub HTML we got: extraction may still salvage
		// something, and processResults falls back to the snippet if not.
	} else if status >= http.StatusBadRequest {
		return PageDocument{}, fmt.Errorf("status %d", status)
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

// fetchHTML downloads a page with the plain HTTP client. A non-2xx status is
// returned WITHOUT an error so the caller can decide between erroring out and
// escalating to the stealth sidecar.
func (service *FetchService) fetchHTML(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "bap-search/0.1")

	response, err := service.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()

	rawBody, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if err != nil {
		return nil, response.StatusCode, err
	}
	return rawBody, response.StatusCode, nil
}

// blockedStatusCodes are the statuses anti-bot layers answer with. A plain 404
// or 500 is NOT here on purpose: a real browser won't resurrect those (the
// bench confirmed it), so they aren't worth a rescue.
var blockedStatusCodes = map[int]bool{
	http.StatusUnauthorized:      true,
	http.StatusForbidden:         true,
	http.StatusProxyAuthRequired: true,
	http.StatusTooManyRequests:   true,
	http.StatusServiceUnavailable: true,
}

// blockedMarkers appear in JS-challenge / consent-wall stub pages. Kept
// conservative: e.g. the bare word "captcha" also shows up in legitimate
// pages' JS bundles (seen on marmiton in the bench), so it's not listed.
var blockedMarkers = []string{
	"enable javascript",
	"please turn on javascript",
	"just a moment",
	"attention required",
	"verify you are a human",
	"pardon our interruption",
	"are you a robot",
	"access denied",
	"unusual traffic",
}

// looksBlocked reports whether a "successful" fetch actually returned an
// anti-bot wall instead of the page.
func looksBlocked(status int, body []byte) bool {
	if blockedStatusCodes[status] {
		return true
	}
	if len(body) < 2048 {
		return true
	}
	head := strings.ToLower(string(body[:min(len(body), 20*1024)]))
	for _, marker := range blockedMarkers {
		if strings.Contains(head, marker) {
			return true
		}
	}
	return false
}

// rescueFetch retries a blocked URL through the stealth-fetch sidecar. It
// degrades silently when the sidecar isn't configured/running or when the
// concurrency budget is spent (a rescue holds a headless browser for
// seconds, so a burst of blocked pages must not fork N browsers).
func (service *FetchService) rescueFetch(ctx context.Context, meta RequestMeta, url string, onStatus func(url, status, detail string)) ([]byte, error) {
	if service.rescueURL == "" {
		return nil, fmt.Errorf("fetch-rescue disabled")
	}
	select {
	case service.rescueSem <- struct{}{}:
	default:
		return nil, fmt.Errorf("fetch-rescue budget exhausted")
	}
	defer func() { <-service.rescueSem }()

	if onStatus != nil {
		onStatus(url, "fetching", "Page looked blocked; retrying with the stealth fetcher.")
	}
	started := time.Now()

	payload, err := json.Marshal(map[string]string{"url": url})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, service.rescueURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := service.rescueClient.Do(req)
	if err != nil {
		// Most common cause: the fetch-rescue compose profile isn't running.
		service.rescueWarnOnce.Do(func() {
			service.logger.Warn("fetch-rescue sidecar unreachable; blocked pages keep the snippet fallback",
				"url", service.rescueURL, "error", err)
		})
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest || len(body) == 0 {
		return nil, fmt.Errorf("fetch-rescue returned status %d", resp.StatusCode)
	}
	loggerWithMeta(ctx, service.logger, meta.ConversationID).Info("url_rescued",
		"url", url,
		"html_bytes", len(body),
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return body, nil
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
