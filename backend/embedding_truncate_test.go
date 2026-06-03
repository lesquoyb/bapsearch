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
	got := truncateForEmbedding(long, 100) // 100 tokens * 4 chars = 400
	if n := len([]rune(got)); n != 400 {
		t.Fatalf("len(result) = %d, want 400", n)
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
	got := truncateForEmbedding(long, 0) // floor of 256 tokens * 4 = 1024 chars
	if n := len([]rune(got)); n != 1024 {
		t.Fatalf("len(result) = %d, want 1024 (256-token floor)", n)
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
