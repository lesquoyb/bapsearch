package main

import (
	"strings"
	"testing"
)

// feedTokens pushes text through the guard in chunks of the given size, the
// way llama.cpp deltas arrive.
func feedTokens(guard *answerStreamGuard, text string, chunk int) {
	for start := 0; start < len(text); start += chunk {
		end := start + chunk
		if end > len(text) {
			end = len(text)
		}
		guard.OnToken(text[start:end])
	}
}

func TestGuardStreamsPlainAnswer(t *testing.T) {
	var emitted strings.Builder
	guard := newAnswerStreamGuard(func(s string) { emitted.WriteString(s) })

	reply := "The capital of France is Paris [1]. It has been the capital since 987 [2].\n\nSources: [1] Wikipedia, [2] Britannica"
	feedTokens(guard, reply, 3)

	if emitted.Len() == 0 {
		t.Fatal("expected tokens to be streamed before Finalize")
	}
	if !guard.Finalize(reply) {
		t.Fatal("Finalize should report the reply as already streamed")
	}
	if emitted.String() != reply {
		t.Fatalf("streamed text = %q, want %q", emitted.String(), reply)
	}
}

func TestGuardNeverEmitsNeedMoreSearchMarker(t *testing.T) {
	for _, chunk := range []int{1, 2, 5, 50} {
		var emitted strings.Builder
		guard := newAnswerStreamGuard(func(s string) { emitted.WriteString(s) })

		reply := "<<NEED_MORE_SEARCH: recette carottes simple>>"
		feedTokens(guard, reply, chunk)
		guard.Finalize(stripNeedMoreSearch(reply))

		if got := emitted.String(); got != "" {
			t.Fatalf("chunk=%d: marker leaked to the client: %q", chunk, got)
		}
	}
}

func TestGuardEmitsTextBeforeMidReplyMarker(t *testing.T) {
	var emitted strings.Builder
	guard := newAnswerStreamGuard(func(s string) { emitted.WriteString(s) })

	prefix := "I found partial information about this topic in the sources provided here."
	reply := prefix + "\n<<NEED_MORE_SEARCH: more details>>"
	feedTokens(guard, reply, 4)

	cleaned := stripNeedMoreSearch(reply)
	if !guard.Finalize(cleaned) {
		t.Fatal("Finalize should report the cleaned reply as handled")
	}
	if got := emitted.String(); strings.Contains(got, "NEED_MORE_SEARCH") {
		t.Fatalf("marker leaked: %q", got)
	}
	if got := emitted.String(); got != cleaned {
		t.Fatalf("streamed %q, want cleaned reply %q", got, cleaned)
	}
}

func TestGuardMarkerSplitAcrossTokensWithSpaces(t *testing.T) {
	var emitted strings.Builder
	guard := newAnswerStreamGuard(func(s string) { emitted.WriteString(s) })

	// Sloppy small-model output: spaces inside the marker, no closing >>.
	guard.OnToken("<< ")
	guard.OnToken("NEED_MORE")
	guard.OnToken("_SEARCH")
	guard.OnToken(" : easy recipe")
	guard.Finalize("")

	if got := emitted.String(); got != "" {
		t.Fatalf("split marker leaked: %q", got)
	}
}

func TestGuardFreezesOnInlineThinkBlock(t *testing.T) {
	var emitted strings.Builder
	guard := newAnswerStreamGuard(func(s string) { emitted.WriteString(s) })

	reply := "<think>secret chain of thought</think>The answer is 42."
	feedTokens(guard, reply, 6)

	if got := emitted.String(); strings.Contains(got, "secret") {
		t.Fatalf("chain of thought leaked: %q", got)
	}
	// The final reply diverges from the (empty) streamed prefix; Finalize must
	// still claim it handled things IF something was emitted, or return false
	// so the caller sends the clean reply itself.
	final := "The answer is 42."
	if guard.Finalize(final) {
		// Nothing was emitted, so Finalize must have returned false.
		t.Fatal("Finalize should return false when nothing was streamed")
	}
}

func TestGuardHTMLIsNotHeldForever(t *testing.T) {
	var emitted strings.Builder
	guard := newAnswerStreamGuard(func(s string) { emitted.WriteString(s) })

	reply := "Use the <code>fmt.Println</code> function to print <b>bold</b> text in Go programs."
	feedTokens(guard, reply, 5)
	if !guard.Finalize(reply) {
		t.Fatal("Finalize should report the reply as already streamed")
	}
	if emitted.String() != reply {
		t.Fatalf("streamed %q, want %q", emitted.String(), reply)
	}
}

func TestGuardSkipsLeadingWhitespace(t *testing.T) {
	var emitted strings.Builder
	guard := newAnswerStreamGuard(func(s string) { emitted.WriteString(s) })

	guard.OnToken("\n\n")
	guard.OnToken("  ")
	raw := "Answer text that is definitely long enough to flush the internal buffer."
	feedTokens(guard, raw, 8)

	final := strings.TrimSpace("\n\n  " + raw)
	if !guard.Finalize(final) {
		t.Fatal("Finalize should report the reply as already streamed")
	}
	if emitted.String() != final {
		t.Fatalf("streamed %q, want %q", emitted.String(), final)
	}
}

func TestGuardFinalizeEmitsHeldTail(t *testing.T) {
	var emitted strings.Builder
	guard := newAnswerStreamGuard(func(s string) { emitted.WriteString(s) })

	// Short reply, never reaches the batch threshold: everything must arrive
	// at Finalize time.
	reply := "Short answer."
	feedTokens(guard, reply, 2)
	if !guard.Finalize(reply) {
		t.Fatal("Finalize should flush the held text and report handled")
	}
	if emitted.String() != reply {
		t.Fatalf("streamed %q, want %q", emitted.String(), reply)
	}
}

func TestGuardUTF8SafeEmission(t *testing.T) {
	var chunks []string
	guard := newAnswerStreamGuard(func(s string) { chunks = append(chunks, s) })

	reply := "Voici une réponse détaillée — après vérification, l'élément clé est le café ☕ et les crêpes."
	feedTokens(guard, reply, 7) // 7 bytes can split runes at token level; guard must not re-split worse
	guard.Finalize(reply)

	if got := strings.Join(chunks, ""); got != reply {
		t.Fatalf("reassembled %q, want %q", got, reply)
	}
}
