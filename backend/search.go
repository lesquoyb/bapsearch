package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type SearchResult struct {
	URL     string
	Title   string
	Snippet string
	Rank    int
	QueryVariant string
}

type SearchEngineStatus struct {
	Engine      string
	Status      string
	Detail      string
	ResultCount int
}

type SearchResponse struct {
	Results      []SearchResult
	EngineStatus []SearchEngineStatus
}

type SearchService struct {
	baseURL string
	client  *http.Client
}

type searxResponse struct {
	Results []struct {
		URL     string   `json:"url"`
		Title   string   `json:"title"`
		Content string   `json:"content"`
		Engine  string   `json:"engine"`
		Engines []string `json:"engines"`
	} `json:"results"`
	UnresponsiveEngines []any `json:"unresponsive_engines"`
}

func (service *SearchService) Search(ctx context.Context, query string) (SearchResponse, error) {
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
			return SearchResponse{}, ctx.Err()
		case <-timer.C:
		}
	}

	return SearchResponse{}, lastErr
}

func (service *SearchService) searchOnce(ctx context.Context, query string) (SearchResponse, error) {
	endpoint, err := url.Parse(service.baseURL)
	if err != nil {
		return SearchResponse{}, err
	}

	params := endpoint.Query()
	params.Set("q", query)
	params.Set("format", "json")
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return SearchResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "bap-search/0.1")

	response, err := service.client.Do(req)
	if err != nil {
		return SearchResponse{}, err
	}
	defer response.Body.Close()

	if response.StatusCode >= http.StatusBadRequest {
		return SearchResponse{}, fmt.Errorf("searxng returned status %d", response.StatusCode)
	}

	payload := searxResponse{}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return SearchResponse{}, err
	}

	results := make([]SearchResult, 0, len(payload.Results))
	engineCounts := map[string]int{}
	for index, result := range payload.Results {
		if strings.TrimSpace(result.URL) == "" {
			continue
		}

		engines := result.Engines
		if len(engines) == 0 && strings.TrimSpace(result.Engine) != "" {
			engines = []string{result.Engine}
		}
		for _, engine := range engines {
			engine = normalizeEngineName(engine)
			if engine == "" {
				continue
			}
			engineCounts[engine]++
		}

		results = append(results, SearchResult{
			URL:     strings.TrimSpace(result.URL),
			Title:   strings.TrimSpace(result.Title),
			Snippet: strings.TrimSpace(result.Content),
			Rank:    index + 1,
		})
	}

	statuses := buildEngineStatuses(engineCounts, payload.UnresponsiveEngines)

	return SearchResponse{
		Results:      results,
		EngineStatus: statuses,
	}, nil
}

func buildEngineStatuses(engineCounts map[string]int, unresponsive []any) []SearchEngineStatus {
	statusByEngine := map[string]SearchEngineStatus{}
	for engine, count := range engineCounts {
		statusByEngine[engine] = SearchEngineStatus{
			Engine:      engine,
			Status:      "ok",
			ResultCount: count,
		}
	}

	for _, raw := range unresponsive {
		engine, detail := parseUnresponsiveEngine(raw)
		engine = normalizeEngineName(engine)
		if engine == "" {
			continue
		}

		status := classifyEngineStatus(detail)
		current := statusByEngine[engine]
		current.Engine = engine
		current.Status = status
		current.Detail = detail
		current.ResultCount = engineCounts[engine]
		statusByEngine[engine] = current
	}

	statuses := make([]SearchEngineStatus, 0, len(statusByEngine))
	for _, status := range statusByEngine {
		statuses = append(statuses, status)
	}

	slices.SortFunc(statuses, func(left, right SearchEngineStatus) int {
		if left.Status != right.Status {
			return strings.Compare(left.Status, right.Status)
		}
		return strings.Compare(left.Engine, right.Engine)
	})

	return statuses
}

func parseUnresponsiveEngine(raw any) (string, string) {
	switch value := raw.(type) {
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			parts = append(parts, fmt.Sprint(item))
		}
		if len(parts) == 0 {
			return "", ""
		}
		if len(parts) == 1 {
			return parts[0], "error"
		}
		return parts[0], strings.Join(parts[1:], " ")
	case map[string]any:
		engine := fmt.Sprint(value["engine"])
		detail := fmt.Sprint(value["error"])
		if detail == "<nil>" || detail == "" {
			detail = fmt.Sprint(value["detail"])
		}
		return engine, detail
	default:
		return fmt.Sprint(value), "error"
	}
}

func classifyEngineStatus(detail string) string {
	text := strings.ToLower(strings.TrimSpace(detail))
	switch {
	case text == "":
		return "error"
	case strings.Contains(text, "timeout"):
		return "timeout"
	case strings.Contains(text, "403") || strings.Contains(text, "denied") || strings.Contains(text, "forbidden"):
		return "error"
	default:
		return "error"
	}
}

func normalizeEngineName(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	return value
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
