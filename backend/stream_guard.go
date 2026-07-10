package main

import (
	"strings"
	"unicode"
)

// answerStreamGuard forwards answer tokens to the client while they are being
// generated, instead of holding the whole reply until generation finishes.
//
// The only reason the reply cannot be forwarded verbatim is the model's
// special actions: a reply may be (or contain) a <<NEED_MORE_SEARCH: ...>>
// action marker that must never be shown, and some models leak a literal
// <think>...</think> block into the content channel. The guard therefore
// holds back any suffix that could still turn into one of those markers and
// freezes emission entirely once a marker is confirmed; the handler's final
// cleanup (stripNeedMoreSearch + the done-refresh) keeps the stored message
// clean exactly as before.
type answerStreamGuard struct {
	emit    func(string)
	pending string
	emitted strings.Builder
	frozen  bool
	started bool // set once the first non-whitespace character was seen
}

// guardMinEmitChars batches safe text so the SSE stream isn't flushed for
// every single model token.
const guardMinEmitChars = 24

// guardMarkers are the strings whose (whitespace-insensitive, case-insensitive)
// prefixes must never be emitted. Kept lowercase.
var guardMarkers = []string{"<<need_more_search:", "<think>", "</think>"}

func newAnswerStreamGuard(emit func(string)) *answerStreamGuard {
	return &answerStreamGuard{emit: emit}
}

// OnToken buffers the token and emits every prefix that can no longer be part
// of an action marker.
func (g *answerStreamGuard) OnToken(token string) {
	if g.frozen || token == "" {
		return
	}
	g.pending += token

	// Skip leading whitespace so the emitted text lines up with the
	// TrimSpace'd final reply used by the handlers.
	if !g.started {
		trimmed := strings.TrimLeftFunc(g.pending, unicode.IsSpace)
		if trimmed == "" {
			g.pending = ""
			return
		}
		g.pending = trimmed
		g.started = true
	}

	// A confirmed marker freezes the stream: everything before it is safe,
	// everything after belongs to the action (or chain-of-thought), which the
	// handler deals with after generation completes. Trailing whitespace is
	// trimmed so the emitted text lines up with the TrimSpace'd cleaned reply.
	if at := indexGuardMarker(g.pending); at >= 0 {
		g.flushSafe(strings.TrimRight(g.pending[:at], " \t\r\n"))
		g.pending = ""
		g.frozen = true
		return
	}

	holdback := potentialMarkerHoldback(g.pending)
	safeLen := len(g.pending) - holdback
	if safeLen >= guardMinEmitChars {
		g.flushSafe(g.pending[:safeLen])
		g.pending = g.pending[safeLen:]
	}
}

// Finalize reconciles the streamed prefix with the final reply the handler is
// about to persist/display. It returns true when the client already received
// the full text (so the handler must NOT send it again), false when nothing
// was streamed and the handler should fall back to the previous single-event
// behaviour.
func (g *answerStreamGuard) Finalize(finalReply string) bool {
	if !g.frozen && g.pending != "" {
		// No marker ever appeared; whatever is left is plain answer text.
		holdover := g.pending
		g.pending = ""
		g.flushSafe(holdover)
	}
	emitted := g.emitted.String()
	if emitted == "" {
		return false
	}
	if strings.HasPrefix(finalReply, emitted) {
		if rest := finalReply[len(emitted):]; rest != "" {
			g.emit(rest)
		}
		return true
	}
	// The final reply is usually TrimSpace'd; tolerate trailing whitespace in
	// what was streamed so the tail still gets appended.
	if trimmed := strings.TrimRight(emitted, " \t\r\n"); strings.HasPrefix(finalReply, trimmed) {
		if rest := finalReply[len(trimmed):]; rest != "" {
			g.emit(rest)
		}
		return true
	}
	// The final reply diverged from what was streamed (e.g. a stripped think
	// block). Don't append anything — the done-refresh re-renders the stored
	// message and fixes the transient state.
	return true
}

func (g *answerStreamGuard) flushSafe(text string) {
	if text == "" {
		return
	}
	g.emitted.WriteString(text)
	g.emit(text)
}

// indexGuardMarker returns the byte index where a confirmed marker starts, or
// -1. Comparison is case-insensitive and ignores whitespace inside the
// candidate (so "<< NEED_MORE_SEARCH :" is caught).
func indexGuardMarker(text string) int {
	for i := 0; i < len(text); i++ {
		if text[i] != '<' {
			continue
		}
		if matchesMarkerFully(text[i:]) {
			return i
		}
	}
	return -1
}

// potentialMarkerHoldback returns how many trailing bytes of text could still
// become a marker and must therefore be withheld. Only suffixes starting at a
// '<' can qualify, so the boundary is always rune-safe.
func potentialMarkerHoldback(text string) int {
	// Markers are short; no candidate prefix needs more than ~48 bytes even
	// with generous internal whitespace.
	start := 0
	if len(text) > 48 {
		start = len(text) - 48
	}
	for i := start; i < len(text); i++ {
		if text[i] != '<' {
			continue
		}
		if isMarkerPrefix(text[i:]) {
			return len(text) - i
		}
	}
	return 0
}

// normalizeMarkerCandidate lowercases and strips whitespace for comparison.
func normalizeMarkerCandidate(candidate string) string {
	var b strings.Builder
	for _, r := range candidate {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func isMarkerPrefix(candidate string) bool {
	normalized := normalizeMarkerCandidate(candidate)
	for _, marker := range guardMarkers {
		if strings.HasPrefix(marker, normalized) {
			return true
		}
	}
	return false
}

func matchesMarkerFully(candidate string) bool {
	normalized := normalizeMarkerCandidate(candidate)
	for _, marker := range guardMarkers {
		if strings.HasPrefix(normalized, marker) {
			return true
		}
	}
	return false
}
