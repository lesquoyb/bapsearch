package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Ollama routes requests by the payload model name (llama.cpp ignores it), so
// the configured names must reach every request type.

func TestChatRequestUsesConfiguredModelName(t *testing.T) {
	service := &LLMService{answerModel: "qwen3.5:9b"}
	req := service.newLlamaChatRequestForProfile([]LLMMessage{{Role: "user", Content: "hi"}}, 64, false, false, ProfileAnswer)
	if req.Model != "qwen3.5:9b" {
		t.Fatalf("chat request model = %q, want %q", req.Model, "qwen3.5:9b")
	}

	service = &LLMService{}
	req = service.newLlamaChatRequestForProfile([]LLMMessage{{Role: "user", Content: "hi"}}, 64, false, false, ProfileAnswer)
	if req.Model != "local" {
		t.Fatalf("default chat request model = %q, want %q (llama.cpp compatibility)", req.Model, "local")
	}
}

func TestEmbeddingRequestsUseConfiguredModelName(t *testing.T) {
	var gotModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Model string `json:"model"`
			Input any    `json:"input"`
		}
		_ = json.Unmarshal(body, &payload)
		gotModels = append(gotModels, payload.Model)

		items := 1
		if arr, ok := payload.Input.([]any); ok {
			items = len(arr)
		}
		out := struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}{}
		for i := 0; i < items; i++ {
			out.Data = append(out.Data, struct {
				Index     int       `json:"index"`
				Embedding []float64 `json:"embedding"`
			}{Index: i, Embedding: []float64{0.1, 0.2}})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	service := &LLMService{
		embeddingsURL:      srv.URL + "/v1/embeddings",
		embedModel:         "nomic-embed-text",
		client:             srv.Client(),
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		maxEmbeddingTokens: 128,
	}

	if _, err := service.EmbedText(t.Context(), RequestMeta{}, "single input"); err != nil {
		t.Fatalf("EmbedText() failed: %v", err)
	}
	if _, err := service.EmbedTexts(t.Context(), RequestMeta{}, []string{"a", "b", "c"}); err != nil {
		t.Fatalf("EmbedTexts() failed: %v", err)
	}

	if len(gotModels) == 0 {
		t.Fatal("no embedding request captured")
	}
	for i, m := range gotModels {
		if m != "nomic-embed-text" {
			t.Fatalf("embedding request %d model = %q, want %q (all: %v)", i, m, "nomic-embed-text", gotModels)
		}
	}
}
