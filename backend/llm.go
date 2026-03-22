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
	rewriteURL        string
	embeddingsURL     string
	client            *http.Client
	logger            *slog.Logger
	maxResponseTokens int
	contextTokens     int
	maxEmbeddingChars int
	enableThinking    bool
	reasoningBudget   int
	temperature       float64
	topP              float64
	topK              int
	Prompts           LLMPrompts
}

const (
	approxCharsPerToken    = 4
	promptSafetyMargin     = 160
	minTrimmableMessageLen = 256
)

const (
	DefaultPromptSummarize = `You are bap-search, a search assistant running on a small self-hosted machine.
Produce a concise factual summary of the extracted page.
Focus on facts useful for answering the user's query.
Return plain text with 3 short bullet points and one short concluding sentence.`

	DefaultPromptRewriteSearch = `You rewrite a user query into a stronger web search query.
Return only one short search string.
Prefer precise nouns, product names, standards, dates, error names, and discriminating keywords.
Do not add explanations, quotes, bullets, or prefixes.`

	DefaultPromptSynthesize = `You are bap-search, a conversational search engine.
You receive article summaries that were already generated from individual search results.
Answer the user's query as directly as the source summaries allow.
If the summaries do not fully support a conclusion, say what is missing.
Return plain text markdown with:
- a short direct answer or synthesis paragraph
- 3 to 5 concise bullet points grounded in the summaries
- one short line starting with "Limits:"`


	DefaultPromptGroundedAnswer = `You are bap-search, a grounded web answer engine.
Answer only from the provided extracted source texts.
Treat the extracted source texts as the primary evidence, not the short summaries.
Every factual claim must cite at least one source using bracket citations like [1] or [2].
Return concise markdown with:
- one short direct answer paragraph
- 3 to 6 factual bullet points with citations
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
	SearchToolInstructions = `MANDATORY RULES — follow these without exception:
- You have a tool: to request a new web search, output ONLY a line exactly like:
  <<NEED_MORE_SEARCH: your search query in plain text>>
  When you output this line, do NOT answer the question — output ONLY the NEED_MORE_SEARCH line.
- You MUST use this tool when the provided context does not contain what the user asks for.
- You MUST use this tool when the user asks for a different example, another item, more details, or something specific that is missing from the context.
- NEVER suggest the user visit a website, click a link, or search manually.
- NEVER recommend external sites by name or URL.
- NEVER say "I suggest you consult", "you can find it at", "try searching for", or anything similar.
- If you cannot answer from the provided context, use <<NEED_MORE_SEARCH: ...>> instead of refusing or deflecting.`

	ForceAnswerInstruction = `You MUST answer using ONLY the information already provided in the search context. Do NOT use the <<NEED_MORE_SEARCH>> tag. Synthesize the best possible answer from the available sources, even if the coverage is incomplete. Acknowledge any gaps briefly, then give the most helpful answer you can.`

	DefaultPromptMemory = `Update the user memory based on the following conversation. Keep it short, factual, and useful for future prompts.`
)

type LLMPrompts struct {
	mu         sync.RWMutex
	Summarize  string
	Synthesize string
	Chat       string
	Memory     string
}

func (p *LLMPrompts) get(field *string, fallback string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if strings.TrimSpace(*field) == "" {
		return fallback
	}
	return *field
}

func (p *LLMPrompts) Set(summarize, synthesize, chat, memory string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Summarize = summarize
	p.Synthesize = synthesize
	p.Chat = chat
	p.Memory = memory
}

func (p *LLMPrompts) GetAll() (summarize, synthesize, chat, memory string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.Summarize, p.Synthesize, p.Chat, p.Memory
}

type llamaChatRequest struct {
	Model              string         `json:"model,omitempty"`
	Messages           []LLMMessage   `json:"messages"`
	Temperature        float64        `json:"temperature,omitempty"`
	MaxTokens          int            `json:"max_tokens,omitempty"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	ReasoningBudget    *int           `json:"reasoning_budget,omitempty"`
	Stream             bool           `json:"stream"`
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
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Embedding []float64 `json:"embedding"`
}

func (service *LLMService) Chat(ctx context.Context, meta RequestMeta, messages []LLMMessage, maxTokens int) (string, error) {
	return service.chatWithURL(ctx, service.baseURL, meta, messages, maxTokens)
}

func (service *LLMService) chatWithURL(ctx context.Context, endpoint string, meta RequestMeta, messages []LLMMessage, maxTokens int) (string, error) {
	unlimited := maxTokens < 0
	if maxTokens <= 0 {
		maxTokens = service.maxResponseTokens
	}

	messages = service.fitMessagesToContext(messages, maxTokens)
	requestMaxTokens := maxTokens
	if unlimited {
		requestMaxTokens = 0
	}
	payload := service.newLlamaChatRequest(messages, requestMaxTokens, false, false)

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	promptPreview := make([]string, 0, len(messages))
	for _, message := range messages {
		promptPreview = append(promptPreview, fmt.Sprintf("[%s] %s", message.Role, strings.TrimSpace(message.Content)))
	}

	service.logger.Info("llm_prompt",
		"timestamp", time.Now().UTC().Format(time.RFC3339),
		"request_id", meta.RequestID,
		"user_id", meta.UserID,
		"conversation_id", meta.ConversationID,
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
	service.logger.Info("llm_response",
		"timestamp", time.Now().UTC().Format(time.RFC3339),
		"request_id", meta.RequestID,
		"user_id", meta.UserID,
		"conversation_id", meta.ConversationID,
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
	service.logger.Info("llm_stream_response",
		"request_id", meta.RequestID,
		"user_id", meta.UserID,
		"conversation_id", meta.ConversationID,
		"chars", len(result),
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

	intent, err := service.chatWithURL(ctx, service.rewriteURL, meta, messages, 128)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(intent), nil
}

func (service *LLMService) RewriteSearchQuery(ctx context.Context, meta RequestMeta, query string) (string, error) {
	messages := []LLMMessage{
		buildSystemMessage(DefaultPromptRewriteSearch),
		{Role: "user", Content: query},
	}

	rewritten, err := service.chatWithURL(ctx, service.rewriteURL, meta, messages, 64)
	if err != nil {
		return "", err
	}

	cleaned := sanitizeSearchQuery(rewritten)
	if cleaned == "" {
		return "", fmt.Errorf("rewrite model returned an empty query")
	}
	return cleaned, nil
}

func (service *LLMService) EmbedText(ctx context.Context, meta RequestMeta, text string) ([]float64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("cannot embed empty text")
	}

	limit := service.maxEmbeddingChars
	if limit <= 0 {
		limit = 1800
	}
	text = service.truncateForPrompt(text, limit)

	payload := llamaEmbeddingRequest{Input: text, Model: "local"}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	endpoint := strings.TrimSpace(service.embeddingsURL)
	if endpoint == "" {
		return nil, fmt.Errorf("embeddings endpoint is not configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := service.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("embedding endpoint returned status %d: %s", resp.StatusCode, string(responseBody))
	}

	var payloadResponse llamaEmbeddingResponse
	if err := json.Unmarshal(responseBody, &payloadResponse); err != nil {
		return nil, err
	}

	if len(payloadResponse.Data) > 0 && len(payloadResponse.Data[0].Embedding) > 0 {
		return payloadResponse.Data[0].Embedding, nil
	}
	if len(payloadResponse.Embedding) > 0 {
		return payloadResponse.Embedding, nil
	}

	return nil, fmt.Errorf("embedding endpoint returned no vector")
}

func (service *LLMService) GenerateGroundedSearchAnswerStream(ctx context.Context, meta RequestMeta, originalQuery, rewrittenQuery string, sources []RankedSource, onReasoning func(string)) (string, error) {
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

	return service.chatStreamWithURL(ctx, service.baseURL, meta, messages, -1, service.enableThinking, onReasoning, func(string) {})
}

func (service *LLMService) SummarizePage(ctx context.Context, meta RequestMeta, query, memory, url, text string) (string, error) {
	text = service.truncateForPrompt(text, 3600)

	prompt := service.Prompts.get(&service.Prompts.Summarize, DefaultPromptSummarize)
	messages := []LLMMessage{
		buildSystemMessage(
			strings.TrimSpace(prompt),
			optionalSystemSection("User memory", memory),
		),
		LLMMessage{Role: "user", Content: fmt.Sprintf("Original query: %s\nSource URL: %s\n\nExtracted page text:\n%s", query, url, text)},
	}

	return service.Chat(ctx, meta, messages, 320)
}

func (service *LLMService) SynthesizeSearchAnswer(ctx context.Context, meta RequestMeta, query string, summaries []SummaryRecord) (string, error) {
	blocks := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		trimmed := strings.TrimSpace(summary.Summary)
		if trimmed == "" {
			continue
		}
		blocks = append(blocks, fmt.Sprintf("Source: %s\n%s", summary.URL, compactContextText(trimmed, 1400)))
	}

	if len(blocks) == 0 {
		return "", fmt.Errorf("no summaries available for synthesis")
	}

	messages := []LLMMessage{
		buildSystemMessage(strings.TrimSpace(service.Prompts.get(&service.Prompts.Synthesize, DefaultPromptSynthesize))),
		{
			Role:    "user",
			Content: fmt.Sprintf("User query: %s\n\nArticle summaries:\n\n%s", query, service.truncateForPrompt(strings.Join(blocks, "\n\n"), 5600)),
		},
	}

	return service.Chat(ctx, meta, messages, 420)
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

func (service *LLMService) GenerateConversationReplyStream(ctx context.Context, meta RequestMeta, userMemory, searchContext string, history []LLMMessage, onReasoning func(string)) (string, error) {
	messages := []LLMMessage{
		buildSystemMessage(
			strings.TrimSpace(service.Prompts.get(&service.Prompts.Chat, DefaultPromptChat)),
			SearchToolInstructions,
			optionalSystemSection("Persistent user memory", userMemory),
			optionalSystemSection("Search context", searchContext),
		),
	}

	messages = append(messages, history...)
	return service.chatStreamWithURL(ctx, service.baseURL, meta, messages, -1, service.enableThinking, onReasoning, func(string) {})
}

func (service *LLMService) GenerateConversationForceReplyStream(ctx context.Context, meta RequestMeta, userMemory, searchContext string, history []LLMMessage, onReasoning func(string)) (string, error) {
	messages := []LLMMessage{
		buildSystemMessage(
			strings.TrimSpace(service.Prompts.get(&service.Prompts.Chat, DefaultPromptChat)),
			ForceAnswerInstruction,
			optionalSystemSection("Persistent user memory", userMemory),
			optionalSystemSection("Search context", searchContext),
		),
	}

	messages = append(messages, history...)
	return service.chatStreamWithURL(ctx, service.baseURL, meta, messages, -1, service.enableThinking, onReasoning, func(string) {})
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
	req := llamaChatRequest{
		Model:       "local",
		Messages:    messages,
		Temperature: service.temperature,
		MaxTokens:   maxTokens,
		Stream:      stream,
	}
	if enableThinking {
		budget := service.reasoningBudget
		req.ChatTemplateKwargs = map[string]any{"enable_thinking": true}
		req.ReasoningBudget = &budget
	} else {
		budget := 0
		req.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
		req.ReasoningBudget = &budget
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
		if index == 0 && message.Role == "system" {
			continue
		}

		length := len([]rune(strings.TrimSpace(message.Content)))
		if length > longestLength && length > minTrimmableMessageLen {
			longestIndex = index
			longestLength = length
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
