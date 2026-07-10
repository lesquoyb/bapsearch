package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type LLMMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type LLMService struct {
	baseURL           string
	embeddingsURL     string
	// answerModel / embedModel are the model names sent in request payloads.
	// llama.cpp ignores them (it serves whatever is loaded), so "local" is a
	// fine default; Ollama routes BY this name, so pointing the endpoints at
	// an Ollama instance requires the real model names (BAP_ANSWER_MODEL /
	// BAP_EMBEDDING_MODEL).
	answerModel string
	embedModel  string
	client            *http.Client
	logger            *slog.Logger
	// llmLogger is a separate logger dedicated to LLM/embedding traffic:
	// prompts, responses, request payload size, latencies, errors. It writes
	// to /logs/llm.jsonl by default so prompt content doesn't drown the
	// general backend log.
	llmLogger          *slog.Logger
	maxResponseTokens int
	contextTokens     int
	maxEmbeddingTokens int
	embeddingBatchSize int
	// embedTokenLimit is the largest input the embeddings server actually
	// accepts. It is discovered proactively from the server's /props (its real
	// n_ctx, already capped to the model) and, as a fallback, learned from
	// rejection messages. Every embed is pre-clamped to it.
	embedMu         sync.Mutex
	embedTokenLimit int
	embedCtxAt      time.Time // last /props probe
	embedCtxProbed  bool
	enableThinking    bool
	reasoningBudget   int
	temperature       float64
	topP              float64
	topK              int
	// profileSampling overrides per call-type. Nil/missing keys fall back to
	// the global temperature/top_p/top_k above. Populated by
	// App.loadSettingsFromDB from rows like "rewrite_temperature".
	profileSampling map[CallProfile]SamplingParams
	Prompts           LLMPrompts
}

// llm returns the dedicated LLM trace logger, falling back to the general
// logger when none has been configured (e.g. tests).
func (service *LLMService) llm() *slog.Logger {
	if service.llmLogger != nil {
		return service.llmLogger
	}
	return service.logger
}

const (
	approxCharsPerToken    = 4
	promptSafetyMargin     = 160
	minTrimmableMessageLen = 256
)

const (
	DefaultPromptGroundedAnswer = `You are bap-search, a grounded web answer engine.
Answer only from the provided extracted source texts.
Treat the extracted source texts as the primary evidence, not the short summaries.
Every factual claim must cite at least one source using bracket citations like [1] or [2].
Return concise markdown with:
- one short direct answer paragraph
- a few factual bullet points with citations
- one short line starting with "Sources:" listing each cited source number with its site name, like: Sources: [1] Wikipedia, [2] Stack Overflow, [3] MDN`

	DefaultPromptChat = `You are bap-search, a conversational search engine.
Answer using the provided summaries, extracted source text, and conversation history.
Treat extracted source text as stronger evidence than the short summaries.
Prefer clear, compact answers suitable for follow-up chat.
Cite your sources using bracket citations like [1] or [2] matching the source numbers in the search context.
End your answer with a short "Sources:" line listing each cited source number with its site name, e.g.: Sources: [1] Wikipedia, [2] MDN`

	// SearchToolInstructions is ALWAYS injected into chat and grounded answer
	// system prompts. It is not user-customizable and not stored in the DB.
	// This ensures the LLM always knows how to request a new search.
	SearchToolInstructions = `You have one special action available: request web searches by outputting one or more lines in this exact format, with nothing else before or after them:
<<NEED_MORE_SEARCH: first search query>>
<<NEED_MORE_SEARCH: second search query>>
Use this action when the provided context does not contain enough information to answer the user. You may request multiple searches at once when different aspects of the question need separate queries — they will run in parallel. Do not answer the question at the same time — the search lines replace your response entirely.
Do not suggest the user visit websites or search manually. If the context is insufficient, use the search action rather than refusing.`

	ForceAnswerInstruction = `You MUST answer using ONLY the information already provided in the search context. Do NOT use the <<NEED_MORE_SEARCH>> tag. Synthesize the best possible answer from the available sources, even if the coverage is incomplete. Acknowledge any gaps briefly, then give the most helpful answer you can.`

	DefaultPromptMemory = `Update the user memory based on the following conversation. Keep it short, factual, and useful for future prompts.`
)

type LLMPrompts struct {
	mu     sync.RWMutex
	Chat   string
	Memory string
}

func (p *LLMPrompts) get(field *string, fallback string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if strings.TrimSpace(*field) == "" {
		return fallback
	}
	return *field
}

func (p *LLMPrompts) Set(chat, memory string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Chat = chat
	p.Memory = memory
}

func (p *LLMPrompts) GetAll() (chat, memory string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.Chat, p.Memory
}

type llamaChatRequest struct {
	Model              string         `json:"model,omitempty"`
	Messages           []LLMMessage   `json:"messages"`
	Temperature        float64        `json:"temperature,omitempty"`
	TopP               float64        `json:"top_p,omitempty"`
	TopK               int            `json:"top_k,omitempty"`
	MaxTokens          int            `json:"max_tokens,omitempty"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	ReasoningBudget    *int           `json:"reasoning_budget,omitempty"`
	Stream             bool           `json:"stream"`
	// CachePrompt asks llama.cpp to keep the processed prompt in its KV cache
	// and reuse the common prefix on the next call. Grounded answers and their
	// follow-ups share a long system+sources prefix, so this skips most of the
	// prompt-processing cost on every call after the first.
	CachePrompt bool `json:"cache_prompt"`
}

// CallProfile names a sampling profile used to pick per-call-type temperature,
// top_p, top_k overrides. Profiles are persisted as DB settings (e.g.
// "rewrite_temperature") and fall back to the global temperature/top_p/top_k
// when no override is configured.
type CallProfile string

const (
	ProfileAnswer  CallProfile = "answer"
	ProfileRewrite CallProfile = "rewrite"
	ProfileUtility CallProfile = "utility"
)

// SamplingParams bundles the three sampler knobs for a single call.
type SamplingParams struct {
	Temperature float64
	TopP        float64
	TopK        int
}

// samplingForProfile resolves the active sampling parameters for a given call
// profile. It reads the profile-specific override from the in-memory map (set
// by loadSettingsFromDB) and falls back to the global temperature/top_p/top_k.
func (service *LLMService) samplingForProfile(profile CallProfile) SamplingParams {
	params := SamplingParams{
		Temperature: service.temperature,
		TopP:        service.topP,
		TopK:        service.topK,
	}
	if service.profileSampling == nil {
		return params
	}
	if override, ok := service.profileSampling[profile]; ok {
		if override.Temperature > 0 {
			params.Temperature = override.Temperature
		}
		if override.TopP > 0 {
			params.TopP = override.TopP
		}
		if override.TopK > 0 {
			params.TopK = override.TopK
		}
	}
	return params
}

type llamaChatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
}

type llamaStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

type llamaEmbeddingRequest struct {
	Input any    `json:"input"`
	Model string `json:"model,omitempty"`
}

type llamaEmbeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Embedding []float64 `json:"embedding"`
}

func (service *LLMService) Chat(ctx context.Context, meta RequestMeta, messages []LLMMessage, maxTokens int) (string, error) {
	return service.chatWithURL(ctx, service.baseURL, meta, messages, maxTokens, ProfileAnswer)
}

func (service *LLMService) chatWithURL(ctx context.Context, endpoint string, meta RequestMeta, messages []LLMMessage, maxTokens int, profile CallProfile) (string, error) {
	unlimited := maxTokens < 0
	if maxTokens <= 0 {
		maxTokens = service.maxResponseTokens
	}

	messages = service.fitMessagesToContext(messages, maxTokens)
	requestMaxTokens := maxTokens
	if unlimited {
		requestMaxTokens = 0
	}
	payload := service.newLlamaChatRequestForProfile(messages, requestMaxTokens, false, false, profile)

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	promptPreview := make([]string, 0, len(messages))
	for _, message := range messages {
		promptPreview = append(promptPreview, fmt.Sprintf("[%s] %s", message.Role, strings.TrimSpace(message.Content)))
	}

	service.llm().Info("llm_prompt",
		"timestamp", time.Now().UTC().Format(time.RFC3339),
		"request_id", meta.RequestID,
		"user_id", meta.UserID,
		"conversation_id", meta.ConversationID,
		"endpoint", endpoint,
		"call", "chat",
		"streaming", false,
		"max_tokens", maxTokens,
		"message_count", len(messages),
		"prompt", strings.Join(promptPreview, "\n\n"),
	)

	if strings.TrimSpace(endpoint) == "" {
		endpoint = service.baseURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	response, err := service.client.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}

	if response.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("llama.cpp returned status %d: %s", response.StatusCode, string(responseBody))
	}

	payloadResponse := llamaChatResponse{}
	if err := json.Unmarshal(responseBody, &payloadResponse); err != nil {
		return "", err
	}
	if len(payloadResponse.Choices) == 0 {
		return "", fmt.Errorf("llama.cpp returned no choices")
	}

	content := strings.TrimSpace(payloadResponse.Choices[0].Message.Content)
	if content == "" && strings.TrimSpace(payloadResponse.Choices[0].Message.ReasoningContent) != "" {
		content = extractAnswerFromReasoning(payloadResponse.Choices[0].Message.ReasoningContent)
		if content == "" {
			return "", fmt.Errorf("llama.cpp returned reasoning without a final answer")
		}
	}
	service.llm().Info("llm_response",
		"timestamp", time.Now().UTC().Format(time.RFC3339),
		"request_id", meta.RequestID,
		"user_id", meta.UserID,
		"conversation_id", meta.ConversationID,
		"endpoint", endpoint,
		"call", "chat",
		"status_code", response.StatusCode,
		"response_chars", len(content),
		"reasoning_chars", len(strings.TrimSpace(payloadResponse.Choices[0].Message.ReasoningContent)),
		"response", content,
	)

	return content, nil
}

// ChatStream sends a streaming request to llama.cpp and calls onToken for each
// content delta. It returns the full accumulated response when done.
func (service *LLMService) ChatStream(ctx context.Context, meta RequestMeta, messages []LLMMessage, maxTokens int, onToken func(string)) (string, error) {
	return service.chatStreamWithURL(ctx, service.baseURL, meta, messages, maxTokens, false, nil, onToken)
}

func (service *LLMService) chatStreamWithURL(ctx context.Context, endpoint string, meta RequestMeta, messages []LLMMessage, maxTokens int, enableThinking bool, onReasoning func(string), onToken func(string)) (string, error) {
	unlimited := maxTokens < 0
	if maxTokens <= 0 {
		maxTokens = service.maxResponseTokens
	}

	messages = service.fitMessagesToContext(messages, maxTokens)
	requestMaxTokens := maxTokens
	if unlimited {
		requestMaxTokens = 0
	}
	payload := service.newLlamaChatRequest(messages, requestMaxTokens, true, enableThinking)

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	streamPromptPreview := make([]string, 0, len(messages))
	for _, message := range messages {
		streamPromptPreview = append(streamPromptPreview, fmt.Sprintf("[%s] %s", message.Role, strings.TrimSpace(message.Content)))
	}
	service.llm().Info("llm_prompt",
		"timestamp", time.Now().UTC().Format(time.RFC3339),
		"request_id", meta.RequestID,
		"user_id", meta.UserID,
		"conversation_id", meta.ConversationID,
		"endpoint", endpoint,
		"call", "chat_stream",
		"streaming", true,
		"max_tokens", maxTokens,
		"reasoning", enableThinking,
		"message_count", len(messages),
		"prompt", strings.Join(streamPromptPreview, "\n\n"),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, service.baseURL, bytes.NewReader(body))
	if endpoint != "" {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	}
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := service.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		errBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("llama.cpp returned status %d: %s", resp.StatusCode, string(errBody))
	}

	var builder strings.Builder
	var reasoningBuilder strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk llamaStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		token := chunk.Choices[0].Delta.Content
		reasoningToken := chunk.Choices[0].Delta.ReasoningContent
		if reasoningToken != "" {
			reasoningBuilder.WriteString(reasoningToken)
			if onReasoning != nil {
				onReasoning(reasoningToken)
			}
		}
		if token == "" {
			continue
		}
		builder.WriteString(token)
		onToken(token)
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return builder.String(), err
	}

	result := strings.TrimSpace(builder.String())
	if result == "" && strings.TrimSpace(reasoningBuilder.String()) != "" {
		result = extractAnswerFromReasoning(reasoningBuilder.String())
		if result == "" {
			return "", fmt.Errorf("llama.cpp returned reasoning without a final answer")
		}
	}
	service.llm().Info("llm_stream_response",
		"timestamp", time.Now().UTC().Format(time.RFC3339),
		"request_id", meta.RequestID,
		"user_id", meta.UserID,
		"conversation_id", meta.ConversationID,
		"call", "chat_stream",
		"chars", len(result),
		"response", result,
	)
	return result, nil
}

// GenerateSearchIntent asks the LLM to produce a short paragraph describing what
// an ideal document would look like to answer the user's query. The resulting
// text is designed to embed well for cosine-similarity matching against document
// embeddings, producing better re-ranking than embedding the raw query alone.
func (service *LLMService) GenerateSearchIntent(ctx context.Context, meta RequestMeta, query string, history []LLMMessage) (string, error) {
	prompt := "Given the user's search query and conversation, write a single short paragraph (2-4 sentences) " +
		"describing the ideal document that would answer the query. Focus on the key concepts, facts, and " +
		"topics the user needs. Do not ask questions. Only output the paragraph, nothing else."

	messages := []LLMMessage{buildSystemMessage(prompt)}
	for _, msg := range history {
		messages = append(messages, msg)
	}
	messages = append(messages, LLMMessage{Role: "user", Content: "Search query: " + query})

	intent, err := service.chatWithURL(ctx, service.baseURL, meta, messages, 128, ProfileUtility)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(intent), nil
}


// GenerateQueryReformulations asks the LLM to produce n alternative phrasings of
// query. It returns up to n sanitized queries; the slice may be shorter if the
// model returns fewer usable lines.
func (service *LLMService) GenerateQueryReformulations(ctx context.Context, meta RequestMeta, query string, n int) ([]string, error) {
	prompt := fmt.Sprintf(
		"Generate %d alternative search queries for the following query. "+
			"Each reformulation should rephrase or approach the topic differently to maximize search result coverage. "+
			"Return ONLY the %d queries, one per line, with no numbering, bullets, explanations, or extra text.",
		n, n,
	)
	messages := []LLMMessage{
		buildSystemMessage(prompt),
		{Role: "user", Content: query},
	}

	maxTokens := n * 24
	if maxTokens < 64 {
		maxTokens = 64
	}

	raw, err := service.chatWithURL(ctx, service.baseURL, meta, messages, maxTokens, ProfileRewrite)
	if err != nil {
		return nil, err
	}

	var results []string
	for _, line := range strings.Split(raw, "\n") {
		if cleaned := sanitizeSearchQuery(strings.TrimSpace(line)); cleaned != "" {
			results = append(results, cleaned)
			if len(results) >= n {
				break
			}
		}
	}
	return results, nil
}

// charsPerEmbeddingToken bounds text by characters before it reaches the
// embeddings server. It is deliberately LOWER than the real ~3.6 chars/token of
// typical web text so a maxTokens budget stays safely under the server's
// physical batch (e.g. a 500-token budget → 1500 chars ≈ 420 tokens, under a
// common ubatch of 512). Inputs that are still too dense are caught by the
// adaptive retry in EmbedText, which fits to the size the server reports.
const charsPerEmbeddingToken = 3

// truncateForEmbedding bounds text to roughly maxTokens, by characters, with no
// network call. The previous implementation asked the llama.cpp /tokenize
// endpoint for an exact count and — critically — returned the FULL, untruncated
// text on any error (bad URL, unreachable endpoint, unexpected JSON, non-2xx).
// When that happened a whole extracted page (up to max_extract_chars) was POSTed
// to a context-limited embeddings server, which rejected it, so embeddings
// failed on most pages "no matter the setting". A deterministic char cap can
// never silently no-op, and the exact token count is irrelevant for ranking.
func truncateForEmbedding(text string, maxTokens int) string {
	if maxTokens <= 0 {
		maxTokens = 256
	}
	maxChars := maxTokens * charsPerEmbeddingToken
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	return strings.TrimSpace(string(runes[:maxChars]))
}

// llama.cpp rejects oversized embedding inputs two ways, depending on whether
// the input exceeds the physical batch (--ubatch-size) or the model's context
// (n_ctx, which can be far below the configured ctx for a small model):
//   500 "input (N tokens) is too large to process ... batch size: M"
//   400 "input (N tokens) is larger than the max context size (M tokens)"
// The 400 form also carries n_prompt_tokens / n_ctx as JSON fields.
var (
	embedBatchRe = regexp.MustCompile(`input \((\d+) tokens\).*?batch size:?\s*(\d+)`)
	embedCtxRe   = regexp.MustCompile(`input \((\d+) tokens\).*?context size \((\d+)`)
)

type embedErrorEnvelope struct {
	Error struct {
		Message       string `json:"message"`
		NPromptTokens int    `json:"n_prompt_tokens"`
		NCtx          int    `json:"n_ctx"`
		NBatch        int    `json:"n_batch"`
	} `json:"error"`
}

// parseEmbedTooLarge returns the (inputTokens, limit) the server reported when
// it refused an input for being too large, across both rejection formats. limit
// is the binding constraint (context or physical batch). Returns (0,0) for any
// other error.
func parseEmbedTooLarge(status int, body string) (tokens, limit int) {
	if status < http.StatusBadRequest || body == "" {
		return 0, 0
	}
	// Prefer the structured fields when present (the 400 context error).
	var env embedErrorEnvelope
	if json.Unmarshal([]byte(body), &env) == nil && env.Error.NPromptTokens > 0 {
		lim := env.Error.NCtx
		if env.Error.NBatch > 0 && (lim == 0 || env.Error.NBatch < lim) {
			lim = env.Error.NBatch
		}
		if lim > 0 {
			return env.Error.NPromptTokens, lim
		}
	}
	// Fall back to scraping the message text (the 500 batch error has no fields).
	for _, re := range []*regexp.Regexp{embedCtxRe, embedBatchRe} {
		if m := re.FindStringSubmatch(body); len(m) == 3 {
			t, _ := strconv.Atoi(m[1])
			l, _ := strconv.Atoi(m[2])
			if t > 0 && l > 0 {
				return t, l
			}
		}
	}
	return 0, 0
}

// embedBudget is the token cap to apply before sending: the configured
// max_embedding_tokens, clamped to what the embeddings server actually accepts
// (its real n_ctx, probed from /props; or a limit learned from a rejection).
func (service *LLMService) embedBudget(ctx context.Context, meta RequestMeta) int {
	service.ensureEmbedContextLimit(ctx, meta)
	n := service.maxEmbeddingTokens
	service.embedMu.Lock()
	limit := service.embedTokenLimit
	service.embedMu.Unlock()
	if limit > 0 && limit < n {
		return limit
	}
	return n
}

// noteEmbedTokenLimit records (the smallest) server-reported acceptance limit,
// with a safety margin, so subsequent embeds are pre-clamped and don't retry.
func (service *LLMService) noteEmbedTokenLimit(limit int) {
	if limit <= 1 {
		return
	}
	safe := limit * 9 / 10
	if safe < 1 {
		safe = limit
	}
	service.embedMu.Lock()
	if service.embedTokenLimit == 0 || safe < service.embedTokenLimit {
		service.embedTokenLimit = safe
	}
	service.embedMu.Unlock()
}

// embedServerBaseURL derives the llama.cpp server root from the embeddings
// endpoint, e.g. http://host:8080/v1/embeddings -> http://host:8080.
func embedServerBaseURL(embeddingsURL string) string {
	base := strings.TrimSpace(embeddingsURL)
	if base == "" {
		return ""
	}
	if idx := strings.Index(base, "/v1/"); idx != -1 {
		base = base[:idx]
	} else if strings.HasSuffix(base, "/embeddings") {
		base = strings.TrimSuffix(base, "/embeddings")
	}
	return strings.TrimRight(base, "/")
}

// ensureEmbedContextLimit proactively asks the embeddings server for its real
// context size (GET /props) and uses it as the send ceiling, so we never POST
// more tokens than the model can accept. Refreshed periodically so it follows
// model reloads. Best-effort: if /props is unavailable we silently fall back to
// the reactive limit learned from rejection messages.
func (service *LLMService) ensureEmbedContextLimit(ctx context.Context, meta RequestMeta) {
	service.embedMu.Lock()
	if service.embedCtxProbed && time.Since(service.embedCtxAt) < 2*time.Minute {
		service.embedMu.Unlock()
		return
	}
	// Claim the probe window now so concurrent embeds don't all hit /props.
	service.embedCtxAt = time.Now()
	service.embedCtxProbed = true
	service.embedMu.Unlock()

	nCtx := service.fetchEmbedContextTokens(ctx)
	if nCtx <= 0 {
		return
	}
	safe := nCtx * 9 / 10
	service.embedMu.Lock()
	prev := service.embedTokenLimit
	// /props is authoritative for the current model: set it (can raise if the
	// model was swapped for a larger one).
	service.embedTokenLimit = safe
	service.embedMu.Unlock()
	if prev != safe {
		service.llm().Info("embedding_context_limit",
			"request_id", meta.RequestID,
			"n_ctx", nCtx,
			"send_budget_tokens", safe,
		)
	}
}

// fetchEmbedContextTokens reads the server's n_ctx from /props. Returns 0 on any
// failure or if the route/shape isn't recognised.
func (service *LLMService) fetchEmbedContextTokens(ctx context.Context) int {
	base := embedServerBaseURL(service.embeddingsURL)
	if base == "" {
		return 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/props", nil)
	if err != nil {
		return 0
	}
	resp, err := service.client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return 0
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0
	}
	var props struct {
		NCtx                      int `json:"n_ctx"`
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if json.Unmarshal(body, &props) != nil {
		return 0
	}
	if props.DefaultGenerationSettings.NCtx > 0 {
		return props.DefaultGenerationSettings.NCtx
	}
	return props.NCtx
}

func (service *LLMService) EmbedText(ctx context.Context, meta RequestMeta, text string) ([]float64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("cannot embed empty text")
	}
	endpoint := strings.TrimSpace(service.embeddingsURL)
	if endpoint == "" {
		return nil, fmt.Errorf("embeddings endpoint is not configured")
	}

	text = truncateForEmbedding(text, service.embedBudget(ctx, meta))

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		vec, status, respBody, err := service.embedOnce(ctx, meta, endpoint, text)
		if err == nil {
			return vec, nil
		}
		lastErr = err
		// If the server rejected the input for exceeding its context or physical
		// batch, learn the limit and shrink to fit using the server's OWN token
		// count (exact chars/token ratio), then retry. This makes embeddings
		// succeed whatever the embeddings model's real context is.
		if tokens, limit := parseEmbedTooLarge(status, respBody); limit > 0 && tokens > limit {
			service.noteEmbedTokenLimit(limit)
			runes := []rune(text)
			target := int(float64(len(runes)) * float64(limit) / float64(tokens) * 0.9)
			if target > 0 && target < len(runes) {
				service.llm().Warn("embedding_input_too_large_retry",
					"request_id", meta.RequestID,
					"conversation_id", meta.ConversationID,
					"reported_tokens", tokens,
					"server_limit", limit,
					"from_chars", len(runes),
					"to_chars", target,
				)
				text = strings.TrimSpace(string(runes[:target]))
				continue
			}
		}
		break
	}
	return nil, lastErr
}

// embedOnce performs a single embeddings request. It returns the HTTP status
// code and response body alongside any error so the caller can adapt (e.g. on a
// "too large for the physical batch" rejection).
func (service *LLMService) embedOnce(ctx context.Context, meta RequestMeta, endpoint, text string) ([]float64, int, string, error) {
	payload := llamaEmbeddingRequest{Input: text, Model: service.embedModelName()}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, "", err
	}

	started := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := service.client.Do(req)
	if err != nil {
		service.llm().Error("embedding_request_failed",
			"timestamp", time.Now().UTC().Format(time.RFC3339),
			"request_id", meta.RequestID,
			"user_id", meta.UserID,
			"conversation_id", meta.ConversationID,
			"endpoint", endpoint,
			"duration_ms", time.Since(started).Milliseconds(),
			"error", err.Error(),
		)
		return nil, 0, "", err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, "", err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		service.llm().Error("embedding_request_status",
			"timestamp", time.Now().UTC().Format(time.RFC3339),
			"request_id", meta.RequestID,
			"user_id", meta.UserID,
			"conversation_id", meta.ConversationID,
			"endpoint", endpoint,
			"status_code", resp.StatusCode,
			"body", string(responseBody),
		)
		return nil, resp.StatusCode, string(responseBody), fmt.Errorf("embedding endpoint returned status %d: %s", resp.StatusCode, string(responseBody))
	}

	var payloadResponse llamaEmbeddingResponse
	if err := json.Unmarshal(responseBody, &payloadResponse); err != nil {
		return nil, resp.StatusCode, string(responseBody), err
	}

	var vector []float64
	switch {
	case len(payloadResponse.Data) > 0 && len(payloadResponse.Data[0].Embedding) > 0:
		vector = payloadResponse.Data[0].Embedding
	case len(payloadResponse.Embedding) > 0:
		vector = payloadResponse.Embedding
	}
	if vector == nil {
		return nil, resp.StatusCode, string(responseBody), fmt.Errorf("embedding endpoint returned no vector")
	}
	service.llm().Info("embedding_response",
		"timestamp", time.Now().UTC().Format(time.RFC3339),
		"request_id", meta.RequestID,
		"user_id", meta.UserID,
		"conversation_id", meta.ConversationID,
		"endpoint", endpoint,
		"input_chars", len(text),
		"vector_dim", len(vector),
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return vector, resp.StatusCode, string(responseBody), nil
}

// EmbedTexts embeds several inputs in a single HTTP request. Returns one
// vector per input in the same order. Each text is truncated to
// maxEmbeddingTokens, exactly like EmbedText. Empty inputs are not allowed —
// the caller is expected to filter beforehand.
func (service *LLMService) EmbedTexts(ctx context.Context, meta RequestMeta, texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) == 1 {
		v, err := service.EmbedText(ctx, meta, texts[0])
		if err != nil {
			return nil, err
		}
		return [][]float64{v}, nil
	}

	inputs := make([]string, 0, len(texts))
	for _, t := range texts {
		trimmed := strings.TrimSpace(t)
		if trimmed == "" {
			return nil, fmt.Errorf("cannot embed empty text in batch")
		}
		inputs = append(inputs, truncateForEmbedding(trimmed, service.embedBudget(ctx, meta)))
	}

	vectors, err := service.embedBatch(ctx, meta, inputs)
	if err == nil {
		return vectors, nil
	}

	// Graceful degradation: a batch failure is often a single oversized or
	// otherwise-bad input. Embed one-by-one so the good sources still rank;
	// failed entries are left nil and skipped by the caller.
	service.llm().Warn("embedding_batch_fallback_per_item",
		"request_id", meta.RequestID,
		"conversation_id", meta.ConversationID,
		"batch_size", len(inputs),
		"error", err.Error(),
	)
	out := make([][]float64, len(texts))
	succeeded := 0
	var firstErr error
	for i := range texts {
		v, e := service.EmbedText(ctx, meta, texts[i])
		if e != nil {
			if firstErr == nil {
				firstErr = e
			}
			continue
		}
		out[i] = v
		succeeded++
	}
	if succeeded == 0 {
		return nil, firstErr
	}
	return out, nil
}

// embedBatch sends every input to the embeddings endpoint in a single request,
// returning one vector per input in order. Inputs must already be trimmed and
// truncated. Any failure is returned so EmbedTexts can fall back to per-item.
func (service *LLMService) embedBatch(ctx context.Context, meta RequestMeta, inputs []string) ([][]float64, error) {
	payload := llamaEmbeddingRequest{Input: inputs, Model: service.embedModelName()}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	endpoint := strings.TrimSpace(service.embeddingsURL)
	if endpoint == "" {
		return nil, fmt.Errorf("embeddings endpoint is not configured")
	}

	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := service.client.Do(req)
	if err != nil {
		service.llm().Error("embedding_batch_request_failed",
			"timestamp", time.Now().UTC().Format(time.RFC3339),
			"request_id", meta.RequestID,
			"user_id", meta.UserID,
			"conversation_id", meta.ConversationID,
			"endpoint", endpoint,
			"batch_size", len(inputs),
			"duration_ms", time.Since(started).Milliseconds(),
			"error", err.Error(),
		)
		return nil, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		service.llm().Error("embedding_batch_request_status",
			"timestamp", time.Now().UTC().Format(time.RFC3339),
			"request_id", meta.RequestID,
			"user_id", meta.UserID,
			"conversation_id", meta.ConversationID,
			"endpoint", endpoint,
			"batch_size", len(inputs),
			"status_code", resp.StatusCode,
			"body", string(responseBody),
		)
		return nil, fmt.Errorf("embedding endpoint returned status %d: %s", resp.StatusCode, string(responseBody))
	}

	var payloadResponse llamaEmbeddingResponse
	if err := json.Unmarshal(responseBody, &payloadResponse); err != nil {
		return nil, err
	}
	if len(payloadResponse.Data) == 0 {
		return nil, fmt.Errorf("embedding endpoint returned no vectors for batch of %d", len(inputs))
	}

	// llama.cpp returns data sorted but the spec says we should honour the
	// "index" field. Reorder defensively so callers can map by position.
	vectors := make([][]float64, len(inputs))
	for _, item := range payloadResponse.Data {
		idx := item.Index
		if idx < 0 || idx >= len(inputs) {
			continue
		}
		vectors[idx] = item.Embedding
	}
	totalDim := 0
	for i, v := range vectors {
		if len(v) == 0 {
			return nil, fmt.Errorf("embedding endpoint returned no vector for input %d", i)
		}
		totalDim = len(v)
	}

	service.llm().Info("embedding_batch_response",
		"timestamp", time.Now().UTC().Format(time.RFC3339),
		"request_id", meta.RequestID,
		"user_id", meta.UserID,
		"conversation_id", meta.ConversationID,
		"endpoint", endpoint,
		"batch_size", len(inputs),
		"vector_dim", totalDim,
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return vectors, nil
}

func (service *LLMService) GenerateGroundedSearchAnswerStream(ctx context.Context, meta RequestMeta, originalQuery, rewrittenQuery string, sources []RankedSource, onReasoning, onToken func(string)) (string, error) {
	blocks := make([]string, 0, len(sources))
	for index, source := range sources {
		if strings.TrimSpace(source.SourceText) == "" {
			continue
		}
		block := fmt.Sprintf("[%d] %s\nURL: %s\nSimilarity: %.4f\n\nExtracted text:\n%s", index+1, compactContextText(source.Title, 180), source.URL, source.SimilarityScore, service.truncateForPrompt(source.SourceText, 3000))
		blocks = append(blocks, block)
	}

	if len(blocks) == 0 {
		return "", fmt.Errorf("no ranked sources available for answer generation")
	}

	targetQuery := strings.TrimSpace(rewrittenQuery)
	if targetQuery == "" {
		targetQuery = strings.TrimSpace(originalQuery)
	}

	messages := []LLMMessage{
		buildSystemMessage(DefaultPromptGroundedAnswer, SearchToolInstructions),
		{Role: "user", Content: fmt.Sprintf("Original user query: %s\nOptimized search query: %s\n\nTop ranked sources:\n\n%s", originalQuery, targetQuery, strings.Join(blocks, "\n\n"))},
	}

	return service.chatStreamWithURL(ctx, service.baseURL, meta, messages, -1, service.enableThinking, onReasoning, streamTokenCallback(onToken))
}

// streamTokenCallback normalizes a possibly-nil token callback so generators
// can be called with or without live token forwarding.
func streamTokenCallback(onToken func(string)) func(string) {
	if onToken == nil {
		return func(string) {}
	}
	return onToken
}

func (service *LLMService) GenerateConversationReply(ctx context.Context, meta RequestMeta, userMemory, searchContext string, history []LLMMessage) (string, string, error) {
	messages := []LLMMessage{
		buildSystemMessage(
			strings.TrimSpace(service.Prompts.get(&service.Prompts.Chat, DefaultPromptChat)),
			SearchToolInstructions,
			optionalSystemSection("Persistent user memory", userMemory),
			optionalSystemSection("Search context", searchContext),
		),
	}

	messages = append(messages, history...)
	var reasoningBuf strings.Builder
	reply, err := service.chatStreamWithURL(ctx, service.baseURL, meta, messages, -1, service.enableThinking, func(token string) {
		reasoningBuf.WriteString(token)
	}, func(string) {})
	return reply, reasoningBuf.String(), err
}

func (service *LLMService) GenerateConversationReplyStream(ctx context.Context, meta RequestMeta, userMemory, searchContext string, history []LLMMessage, onReasoning, onToken func(string)) (string, error) {
	messages := []LLMMessage{
		buildSystemMessage(
			strings.TrimSpace(service.Prompts.get(&service.Prompts.Chat, DefaultPromptChat)),
			SearchToolInstructions,
			optionalSystemSection("Persistent user memory", userMemory),
			optionalSystemSection("Search context", searchContext),
		),
	}

	messages = append(messages, history...)
	return service.chatStreamWithURL(ctx, service.baseURL, meta, messages, -1, service.enableThinking, onReasoning, streamTokenCallback(onToken))
}

func (service *LLMService) GenerateConversationForceReplyStream(ctx context.Context, meta RequestMeta, userMemory, searchContext string, history []LLMMessage, onReasoning, onToken func(string)) (string, error) {
	messages := []LLMMessage{
		buildSystemMessage(
			strings.TrimSpace(service.Prompts.get(&service.Prompts.Chat, DefaultPromptChat)),
			ForceAnswerInstruction,
			optionalSystemSection("Persistent user memory", userMemory),
			optionalSystemSection("Search context", searchContext),
		),
	}

	messages = append(messages, history...)
	return service.chatStreamWithURL(ctx, service.baseURL, meta, messages, -1, service.enableThinking, onReasoning, streamTokenCallback(onToken))
}

func (service *LLMService) UpdateUserMemory(ctx context.Context, meta RequestMeta, currentMemory, transcript string) (string, error) {
	messages := []LLMMessage{
		buildSystemMessage(
			service.Prompts.get(&service.Prompts.Memory, DefaultPromptMemory),
			optionalSystemSection("Current user memory", currentMemory),
		),
	}

	messages = append(messages, LLMMessage{Role: "user", Content: transcript})
	return service.Chat(ctx, meta, messages, 220)
}

func (service *LLMService) newLlamaChatRequest(messages []LLMMessage, maxTokens int, stream bool, enableThinking bool) llamaChatRequest {
	return service.newLlamaChatRequestForProfile(messages, maxTokens, stream, enableThinking, ProfileAnswer)
}

// chatModelName returns the model name to send in chat payloads.
func (service *LLMService) chatModelName() string {
	if strings.TrimSpace(service.answerModel) != "" {
		return service.answerModel
	}
	return "local"
}

// embedModelName returns the model name to send in embedding payloads.
func (service *LLMService) embedModelName() string {
	if strings.TrimSpace(service.embedModel) != "" {
		return service.embedModel
	}
	return "local"
}

func (service *LLMService) newLlamaChatRequestForProfile(messages []LLMMessage, maxTokens int, stream bool, enableThinking bool, profile CallProfile) llamaChatRequest {
	params := service.samplingForProfile(profile)
	req := llamaChatRequest{
		Model:       service.chatModelName(),
		Messages:    messages,
		Temperature: params.Temperature,
		TopP:        params.TopP,
		TopK:        params.TopK,
		MaxTokens:   maxTokens,
		Stream:      stream,
		CachePrompt: true,
	}
	if enableThinking {
		budget := service.reasoningBudget
		req.ChatTemplateKwargs = map[string]any{"enable_thinking": true}
		req.ReasoningBudget = &budget
	} else {
		// Don't set reasoning_budget when thinking is off — llama.cpp logs
		// the field's presence as "reasoning budget activated" even when the
		// value is 0, which made users think their unchecked setting hadn't
		// taken effect. The chat template kwarg alone is enough to disable
		// thinking on models that support it.
		req.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}
	return req
}

// extractAnswerFromReasoning recovers the actual answer from reasoning content
// produced by thinking models (e.g. Qwen3.5). The model wraps its chain-of-thought
// in <think>…</think> and places the real answer after the closing tag. If no tag
// is present, the last non-empty line is returned as a best-effort fallback.
func extractAnswerFromReasoning(reasoning string) string {
	if idx := strings.LastIndex(reasoning, "</think>"); idx >= 0 {
		after := strings.TrimSpace(reasoning[idx+len("</think>"):])
		if after != "" {
			return after
		}
	}
	// Fallback: return the last non-empty line (the model sometimes omits the tag)
	lines := strings.Split(strings.TrimSpace(reasoning), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" && !strings.HasPrefix(line, "<think") && line != "</think>" {
			return line
		}
	}
	return ""
}

func buildSystemMessage(parts ...string) LLMMessage {
	sections := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		sections = append(sections, trimmed)
	}

	return LLMMessage{
		Role:    "system",
		Content: strings.Join(sections, "\n\n"),
	}
}

func optionalSystemSection(title, body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if strings.Contains(body, "\n") {
		return title + ":\n" + body
	}
	return title + ": " + body
}

func (service *LLMService) fitMessagesToContext(messages []LLMMessage, maxTokens int) []LLMMessage {
	if service.contextTokens <= 0 {
		return messages
	}

	fitted := append([]LLMMessage(nil), messages...)
	promptBudget := service.contextTokens - maxTokens - promptSafetyMargin
	if promptBudget < 256 {
		promptBudget = 256
	}

	for attempt := 0; attempt < 32 && estimateMessagesTokens(fitted) > promptBudget; attempt++ {
		index := longestTrimmableMessage(fitted)
		if index == -1 {
			if !dropOldestConversationMessage(&fitted) {
				break
			}
			continue
		}

		excessTokens := estimateMessagesTokens(fitted) - promptBudget
		reductionChars := (excessTokens * approxCharsPerToken) + 128
		currentLength := len([]rune(strings.TrimSpace(fitted[index].Content)))
		nextLength := currentLength - reductionChars
		if nextLength < minTrimmableMessageLen {
			nextLength = minTrimmableMessageLen
		}
		if nextLength >= currentLength {
			nextLength = currentLength - 64
		}
		if nextLength <= 0 {
			if !dropOldestConversationMessage(&fitted) {
				break
			}
			continue
		}

		fitted[index].Content = service.truncateForPrompt(fitted[index].Content, nextLength)
	}

	return fitted
}

func longestTrimmableMessage(messages []LLMMessage) int {
	longestIndex := -1
	longestLength := 0
	for index, message := range messages {
		// Prefer trimming non-system messages first; include system only as last resort.
		if index == 0 && message.Role == "system" {
			continue
		}

		length := len([]rune(strings.TrimSpace(message.Content)))
		if length > longestLength && length > minTrimmableMessageLen {
			longestIndex = index
			longestLength = length
		}
	}

	// If no non-system message is long enough, fall back to trimming the system message.
	if longestIndex == -1 && len(messages) > 0 && messages[0].Role == "system" {
		length := len([]rune(strings.TrimSpace(messages[0].Content)))
		if length > minTrimmableMessageLen {
			longestIndex = 0
		}
	}

	return longestIndex
}

func dropOldestConversationMessage(messages *[]LLMMessage) bool {
	firstNonSystem := -1
	lastNonSystem := -1
	for index, message := range *messages {
		if message.Role == "system" {
			continue
		}
		if firstNonSystem == -1 {
			firstNonSystem = index
		}
		lastNonSystem = index
	}

	if firstNonSystem == -1 || firstNonSystem == lastNonSystem {
		return false
	}

	trimmed := make([]LLMMessage, 0, len(*messages)-1)
	removed := false
	for index, message := range *messages {
		if !removed && index == firstNonSystem {
			removed = true
			continue
		}
		trimmed = append(trimmed, message)
	}

	*messages = trimmed
	return true
}

func estimateMessagesTokens(messages []LLMMessage) int {
	total := 0
	for _, message := range messages {
		total += estimateTokens(message.Content) + 6
	}
	return total
}

func estimateTokens(value string) int {
	runes := len([]rune(strings.TrimSpace(value)))
	if runes == 0 {
		return 0
	}
	return (runes + approxCharsPerToken - 1) / approxCharsPerToken
}

func (service *LLMService) truncateForPrompt(value string, maxChars int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if maxChars <= 0 || len(runes) <= maxChars {
		return value
	}

	suffix := "\n\n[truncated]"
	suffixLen := len([]rune(suffix))
	if maxChars <= suffixLen+32 {
		return string(runes[:maxChars])
	}

	return strings.TrimSpace(string(runes[:maxChars-suffixLen])) + suffix
}

func sanitizeSearchQuery(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "\"'`")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.TrimPrefix(value, "query:")
	value = strings.TrimPrefix(value, "Query:")
	value = strings.Join(strings.Fields(value), " ")
	// Reject any answer containing <think>, </think>, or other tags, or that looks like reasoning
	if strings.Contains(value, "<think>") || strings.Contains(value, "</think>") || strings.Contains(value, "Reasoning:") || strings.Contains(value, "Answer:") || strings.Contains(value, "Bullet") || strings.Contains(value, "explanation") || strings.Contains(value, "justification") {
		return ""
	}
	// Reject if it contains any angle brackets (likely hallucinated tags)
	if strings.ContainsAny(value, "<>") {
		return ""
	}
	if len([]rune(value)) > 180 {
		value = string([]rune(value)[:180])
	}
	value = strings.TrimSpace(strings.Trim(value, "|/\\.,:;_-"))
	if !isUsableSearchQuery(value) {
		return ""
	}
	return value
}

func isUsableSearchQuery(value string) bool {
	value = strings.TrimSpace(value)
	if len([]rune(value)) < 3 {
		return false
	}

	words := strings.Fields(strings.ToLower(value))
	if len(words) > 0 {
		uniqueWords := map[string]int{}
		maxRepeatedWordRun := 1
		currentRepeatedWordRun := 1
		for index, word := range words {
			uniqueWords[word]++
			if index > 0 && word == words[index-1] {
				currentRepeatedWordRun++
				if currentRepeatedWordRun > maxRepeatedWordRun {
					maxRepeatedWordRun = currentRepeatedWordRun
				}
			} else {
				currentRepeatedWordRun = 1
			}
		}

		if len(words) >= 4 && len(uniqueWords) == 1 {
			return false
		}
		if maxRepeatedWordRun >= 3 {
			return false
		}
	}

	alnumCount := 0
	punctuationCount := 0
	maxRepeatedPunctuation := 0
	currentRepeatedPunctuation := 0
	var lastPunctuation rune

	for _, char := range value {
		switch {
		case unicode.IsLetter(char) || unicode.IsDigit(char):
			alnumCount++
			currentRepeatedPunctuation = 0
			lastPunctuation = 0
		case unicode.IsSpace(char):
			currentRepeatedPunctuation = 0
			lastPunctuation = 0
		default:
			punctuationCount++
			if char == lastPunctuation {
				currentRepeatedPunctuation++
			} else {
				currentRepeatedPunctuation = 1
				lastPunctuation = char
			}
			if currentRepeatedPunctuation > maxRepeatedPunctuation {
				maxRepeatedPunctuation = currentRepeatedPunctuation
			}
		}
	}

	if alnumCount < 3 {
		return false
	}
	if punctuationCount > alnumCount {
		return false
	}
	if maxRepeatedPunctuation >= 4 {
		return false
	}

	return true
}
