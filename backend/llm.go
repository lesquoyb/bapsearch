package main

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log/slog"
    "net/http"
    "strings"
    "time"
)

type LLMMessage struct {
    Role    string `json:"role"`
    Content string `json:"content"`
}

type LLMService struct {
    baseURL           string
    client            *http.Client
    logger            *slog.Logger
    maxResponseTokens int
    contextTokens     int
}

const (
    approxCharsPerToken    = 4
    promptSafetyMargin     = 160
    minTrimmableMessageLen = 256
)

type llamaChatRequest struct {
    Model       string       `json:"model,omitempty"`
    Messages    []LLMMessage `json:"messages"`
    Temperature float64      `json:"temperature,omitempty"`
    MaxTokens   int          `json:"max_tokens,omitempty"`
    Stream      bool         `json:"stream"`
}

type llamaChatResponse struct {
    Choices []struct {
        Message struct {
            Content string `json:"content"`
        } `json:"message"`
    } `json:"choices"`
}

func (service *LLMService) Chat(ctx context.Context, meta RequestMeta, messages []LLMMessage, maxTokens int) (string, error) {
    if maxTokens <= 0 {
        maxTokens = service.maxResponseTokens
    }

    messages = service.fitMessagesToContext(messages, maxTokens)

    payload := llamaChatRequest{
        Model:       "local",
        Messages:    messages,
        Temperature: 0.2,
        MaxTokens:   maxTokens,
        Stream:      false,
    }

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

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, service.baseURL, bytes.NewReader(body))
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
    service.logger.Info("llm_response",
        "timestamp", time.Now().UTC().Format(time.RFC3339),
        "request_id", meta.RequestID,
        "user_id", meta.UserID,
        "conversation_id", meta.ConversationID,
        "response", content,
    )

    return content, nil
}

func (service *LLMService) SummarizePage(ctx context.Context, meta RequestMeta, query, memory, url, text string) (string, error) {
    text = service.truncateForPrompt(text, 3600)

    messages := []LLMMessage{
        {
            Role: "system",
            Content: strings.TrimSpace(`You are bap-search, a search assistant running on a small self-hosted machine.
Produce a concise factual summary of the extracted page.
Focus on facts useful for answering the user's query.
Return plain text with 3 short bullet points and one short concluding sentence.`),
        },
    }

    if memory != "" {
        messages = append(messages, LLMMessage{Role: "system", Content: "User memory: " + memory})
    }

    messages = append(messages,
        LLMMessage{Role: "user", Content: fmt.Sprintf("Original query: %s\nSource URL: %s\n\nExtracted page text:\n%s", query, url, text)},
    )

    return service.Chat(ctx, meta, messages, 320)
}

func (service *LLMService) GenerateConversationReply(ctx context.Context, meta RequestMeta, userMemory, searchContext string, history []LLMMessage) (string, error) {
    messages := []LLMMessage{
        {
            Role: "system",
            Content: strings.TrimSpace(`You are bap-search, a conversational search engine.
Answer using the provided summaries, extracted source text, and conversation history.
If the context does not support a claim, say that the source material is insufficient.
Prefer clear, compact answers suitable for follow-up chat.`),
        },
    }

    if userMemory != "" {
        messages = append(messages, LLMMessage{Role: "system", Content: "Persistent user memory: " + userMemory})
    }
    if searchContext != "" {
        messages = append(messages, LLMMessage{Role: "system", Content: "Search context:\n" + searchContext})
    }

    messages = append(messages, history...)
    return service.Chat(ctx, meta, messages, service.maxResponseTokens)
}

func (service *LLMService) UpdateUserMemory(ctx context.Context, meta RequestMeta, currentMemory, transcript string) (string, error) {
    messages := []LLMMessage{
        {
            Role: "system",
            Content: "Update the user memory based on the following conversation. Keep it short, factual, and useful for future prompts.",
        },
    }

    if currentMemory != "" {
        messages = append(messages, LLMMessage{Role: "system", Content: "Current user memory: " + currentMemory})
    }

    messages = append(messages, LLMMessage{Role: "user", Content: transcript})
    return service.Chat(ctx, meta, messages, 220)
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
