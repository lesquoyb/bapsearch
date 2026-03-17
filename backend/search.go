package main

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "net"
    "net/http"
    "net/url"
    "strings"
    "time"
)

type SearchResult struct {
    URL     string
    Title   string
    Snippet string
    Rank    int
}

type SearchService struct {
    baseURL string
    client  *http.Client
}

type searxResponse struct {
    Results []struct {
        URL     string `json:"url"`
        Title   string `json:"title"`
        Content string `json:"content"`
    } `json:"results"`
}

func (service *SearchService) Search(ctx context.Context, query string) ([]SearchResult, error) {
    var lastErr error
    for attempt := 0; attempt < 4; attempt++ {
        results, err := service.searchOnce(ctx, query)
        if err == nil {
            return results, nil
        }

        lastErr = err
        if !isTransientSearchError(err) || attempt == 3 {
            break
        }

        timer := time.NewTimer(time.Duration(attempt+1) * 400 * time.Millisecond)
        select {
        case <-ctx.Done():
            timer.Stop()
            return nil, ctx.Err()
        case <-timer.C:
        }
    }

    return nil, lastErr
}

func (service *SearchService) searchOnce(ctx context.Context, query string) ([]SearchResult, error) {
    endpoint, err := url.Parse(service.baseURL)
    if err != nil {
        return nil, err
    }

    params := endpoint.Query()
    params.Set("q", query)
    params.Set("format", "json")
    endpoint.RawQuery = params.Encode()

    req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
    if err != nil {
        return nil, err
    }
    req.Header.Set("Accept", "application/json")
    req.Header.Set("User-Agent", "bap-search/0.1")

    response, err := service.client.Do(req)
    if err != nil {
        return nil, err
    }
    defer response.Body.Close()

    if response.StatusCode >= http.StatusBadRequest {
        return nil, fmt.Errorf("searxng returned status %d", response.StatusCode)
    }

    payload := searxResponse{}
    if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
        return nil, err
    }

    results := make([]SearchResult, 0, len(payload.Results))
    for index, result := range payload.Results {
        if strings.TrimSpace(result.URL) == "" {
            continue
        }
        results = append(results, SearchResult{
            URL:     strings.TrimSpace(result.URL),
            Title:   strings.TrimSpace(result.Title),
            Snippet: strings.TrimSpace(result.Content),
            Rank:    index + 1,
        })
    }

    return results, nil
}

func isTransientSearchError(err error) bool {
    var netErr net.Error
    if errors.As(err, &netErr) {
        return true
    }

    text := strings.ToLower(err.Error())
    return strings.Contains(text, "connection refused") ||
        strings.Contains(text, "timeout") ||
        strings.Contains(text, "temporary failure") ||
        strings.Contains(text, "status 502") ||
        strings.Contains(text, "status 503") ||
        strings.Contains(text, "status 504")
}
