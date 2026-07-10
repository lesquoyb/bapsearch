package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLooksBlocked(t *testing.T) {
	bigPage := []byte("<html>" + strings.Repeat("real content ", 400) + "</html>")
	cases := []struct {
		name   string
		status int
		body   []byte
		want   bool
	}{
		{"forbidden", 403, bigPage, true},
		{"rate limited", 429, bigPage, true},
		{"tiny stub", 200, []byte("<html>consent wall</html>"), true},
		{"js challenge", 200, append([]byte("<html><title>Just a moment...</title>"), bigPage...), true},
		{"enable js wall", 200, append([]byte("<html>Please enable JavaScript to continue"), bigPage...), true},
		{"normal page", 200, bigPage, false},
		{"plain 404 is not a block", 404, bigPage, false},
	}
	for _, tc := range cases {
		if got := looksBlocked(tc.status, tc.body); got != tc.want {
			t.Errorf("%s: looksBlocked(%d, %dB) = %v, want %v", tc.name, tc.status, len(tc.body), got, tc.want)
		}
	}
}

// fetchFixture wires a FetchService to a fake origin, a fake trafilatura
// extractor (echoes the HTML body text), and optionally a fake rescue sidecar.
func fetchFixture(t *testing.T, origin http.HandlerFunc, withRescue bool) (*FetchService, *httptest.Server, *int) {
	t.Helper()
	originSrv := httptest.NewServer(origin)
	t.Cleanup(originSrv.Close)

	extractor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// crude "extraction": strip tags is irrelevant, just echo the payload
		fmt.Fprint(w, string(body))
	}))
	t.Cleanup(extractor.Close)

	rescueCalls := 0
	rescueURL := ""
	if withRescue {
		rescue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rescueCalls++
			fmt.Fprint(w, "<html><body>"+strings.Repeat("rescued article text ", 300)+"</body></html>")
		}))
		t.Cleanup(rescue.Close)
		rescueURL = rescue.URL
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := NewFetchService(logger, "trafilatura", extractor.URL, rescueURL, 1, 100000, 5)
	return service, originSrv, &rescueCalls
}

func TestBlockedFetchIsRescued(t *testing.T) {
	service, origin, rescueCalls := fetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}, true)

	results := []SearchResult{{URL: origin.URL + "/blocked", Title: "t", Rank: 1}}
	docs := service.FetchAndExtract(context.Background(), RequestMeta{}, results, nil)

	if *rescueCalls != 1 {
		t.Fatalf("rescue sidecar called %d times, want 1", *rescueCalls)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1 (rescued)", len(docs))
	}
	if !strings.Contains(docs[0].Text, "rescued article text") {
		t.Fatalf("document text does not come from the rescued HTML: %q", docs[0].Text[:80])
	}
}

func TestJSWallStubIsRescued(t *testing.T) {
	service, origin, rescueCalls := fetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><title>Just a moment...</title><body>"+strings.Repeat("challenge ", 400)+"</body></html>")
	}, true)

	results := []SearchResult{{URL: origin.URL + "/wall", Title: "t", Rank: 1}}
	docs := service.FetchAndExtract(context.Background(), RequestMeta{}, results, nil)

	if *rescueCalls != 1 {
		t.Fatalf("rescue sidecar called %d times, want 1", *rescueCalls)
	}
	if len(docs) != 1 || !strings.Contains(docs[0].Text, "rescued article text") {
		t.Fatal("JS-wall stub should have been replaced by the rescued HTML")
	}
}

func TestBlockedFetchWithoutRescueKeepsOldBehaviour(t *testing.T) {
	service, origin, _ := fetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}, false)

	results := []SearchResult{{URL: origin.URL + "/blocked", Title: "t", Rank: 1}}
	docs := service.FetchAndExtract(context.Background(), RequestMeta{}, results, nil)

	if len(docs) != 0 {
		t.Fatalf("a 403 without rescue must fail like before, got %d documents", len(docs))
	}
}

func TestHealthyFetchNeverCallsRescue(t *testing.T) {
	page := "<html><body>" + strings.Repeat("healthy content ", 300) + "</body></html>"
	service, origin, rescueCalls := fetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page)
	}, true)

	results := []SearchResult{{URL: origin.URL + "/fine", Title: "t", Rank: 1}}
	docs := service.FetchAndExtract(context.Background(), RequestMeta{}, results, nil)

	if *rescueCalls != 0 {
		t.Fatalf("rescue sidecar called %d times for a healthy page, want 0", *rescueCalls)
	}
	if len(docs) != 1 || !strings.Contains(docs[0].Text, "healthy content") {
		t.Fatal("healthy page should extract normally")
	}
}
