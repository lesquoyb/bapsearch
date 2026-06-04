package main

import (
	"strings"
	"testing"
)

// truncateForEmbedding must be a hard, deterministic char cap — the whole point
// of the rewrite is that it can never silently pass full-size text through to a
// context-limited embeddings server.
func TestTruncateForEmbeddingCapsByChars(t *testing.T) {
	long := strings.Repeat("a", 4000)
	got := truncateForEmbedding(long, 100) // 100 tokens * charsPerEmbeddingToken
	if want := 100 * charsPerEmbeddingToken; len([]rune(got)) != want {
		t.Fatalf("len(result) = %d, want %d", len([]rune(got)), want)
	}
}

func TestTruncateForEmbeddingShortTextUnchanged(t *testing.T) {
	in := "a short passage that already fits"
	if got := truncateForEmbedding(in, 100); got != in {
		t.Fatalf("result = %q, want it returned unchanged", got)
	}
}

func TestTruncateForEmbeddingZeroMaxTokensUsesFloor(t *testing.T) {
	long := strings.Repeat("b", 5000)
	got := truncateForEmbedding(long, 0) // floor of 256 tokens
	if want := 256 * charsPerEmbeddingToken; len([]rune(got)) != want {
		t.Fatalf("len(result) = %d, want %d (256-token floor)", len([]rune(got)), want)
	}
}

// A pathological maxTokens must still produce a bounded result, never the input.
func TestTruncateForEmbeddingNeverExceedsBudget(t *testing.T) {
	long := strings.Repeat("word ", 100000) // 500k chars
	got := truncateForEmbedding(long, 500)
	if n := len([]rune(got)); n > 500*charsPerEmbeddingToken {
		t.Fatalf("len(result) = %d, want <= %d", n, 500*charsPerEmbeddingToken)
	}
}

// The adaptive retry depends on parsing llama.cpp's exact rejection message.
func TestParseEmbedTooLarge(t *testing.T) {
	body := `{"error":{"code":500,"message":"input (558 tokens) is too large to process. increase the physical batch size (current batch size: 512)","type":"server_error"}}`
	tokens, batch := parseEmbedTooLarge(500, body)
	if tokens != 558 || batch != 512 {
		t.Fatalf("parseEmbedTooLarge = (%d, %d), want (558, 512)", tokens, batch)
	}

	// Non-size errors and success must not match.
	if tk, b := parseEmbedTooLarge(500, `{"error":{"message":"out of memory"}}`); tk != 0 || b != 0 {
		t.Fatalf("unrelated error matched: (%d, %d)", tk, b)
	}
	if tk, b := parseEmbedTooLarge(200, body); tk != 0 || b != 0 {
		t.Fatalf("2xx status matched: (%d, %d)", tk, b)
	}
}
