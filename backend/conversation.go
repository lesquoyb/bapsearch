package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type ConversationService struct {
	db            *sql.DB
	logger        *slog.Logger
	summaryTarget int
}

type ConversationListItem struct {
	ID        int64
	Title     string
	UpdatedAt time.Time
}

type MessageRecord struct {
	ID        int64
	Role      string
	Content   string
	Timestamp time.Time
}

type SummaryRecord struct {
	URL        string
	Summary    string
	SourceText string
	Status     string
	Detail     string
}

type ConversationView struct {
	ID                int64
	UserID            string
	Title             string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Messages          []MessageRecord
	SearchResults     []SearchResult
	EngineStatus      []SearchEngineStatus
	Summaries         []SummaryRecord
	SummaryLookup     map[string]SummaryRecord
	ReadySummaryCount int
	SummaryTarget     int
	OverviewSummary   string
}

func (app *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	meta := requestMetaFromContext(r.Context())
	if err := app.conversations.EnsureUser(r.Context(), meta.UserID); err != nil {
		http.Error(w, "failed to initialize user", http.StatusInternalServerError)
		return
	}

	conversations, err := app.conversations.ListConversations(r.Context(), meta.UserID)
	if err != nil {
		http.Error(w, "failed to load conversations", http.StatusInternalServerError)
		return
	}

	app.render(w, "index", PageData{
		AppName:       "bap-search",
		UserID:        meta.UserID,
		Conversations: conversations,
		Status:        r.URL.Query().Get("status"),
	})
}

func (app *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := strings.TrimSpace(r.FormValue("q"))
	if query == "" {
		http.Redirect(w, r, "/?status=missing+query", http.StatusSeeOther)
		return
	}

	meta := requestMetaFromContext(r.Context())
	if err := app.conversations.EnsureUser(r.Context(), meta.UserID); err != nil {
		http.Error(w, "failed to initialize user", http.StatusInternalServerError)
		return
	}

	conversationID, err := app.conversations.CreateConversation(r.Context(), meta.UserID, query)
	if err != nil {
		http.Error(w, "failed to create conversation", http.StatusInternalServerError)
		return
	}

	loggerWithMeta(r.Context(), app.logger, conversationID).Info("search_query", "query", query)

	searchResponse, err := app.search.Search(r.Context(), query)
	if err != nil {
		loggerWithMeta(r.Context(), app.logger, conversationID).Error("search failed", "error", err)
		http.Redirect(w, r, "/?status=search+failed", http.StatusSeeOther)
		return
	}

	if err := app.conversations.StoreSearchResults(r.Context(), conversationID, searchResponse.Results, searchResponse.EngineStatus); err != nil {
		loggerWithMeta(r.Context(), app.logger, conversationID).Error("storing search results failed", "error", err)
		http.Error(w, "failed to store search results", http.StatusInternalServerError)
		return
	}

	app.summaryJobs <- SummaryJob{
		ConversationID: conversationID,
		UserID:         meta.UserID,
		Query:          query,
		Results:        searchResponse.Results,
	}

	http.Redirect(w, r, fmt.Sprintf("/conversations/%d", conversationID), http.StatusSeeOther)
}

func (app *App) handleConversationRoutes(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/conversations/"), "/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	parts := strings.Split(path, "/")
	conversationID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		app.handleConversationView(w, r, conversationID)
	case len(parts) == 2 && parts[1] == "results" && r.Method == http.MethodGet:
		app.handleConversationResults(w, r, conversationID)
	case len(parts) == 2 && parts[1] == "summaries" && r.Method == http.MethodGet:
		app.handleConversationSummaries(w, r, conversationID)
	case len(parts) == 2 && parts[1] == "messages" && r.Method == http.MethodPost:
		app.handleConversationMessage(w, r, conversationID)
	default:
		http.NotFound(w, r)
	}
}

func (app *App) handleConversationView(w http.ResponseWriter, r *http.Request, conversationID int64) {
	meta := requestMetaFromContext(r.Context())
	conversation, conversations, err := app.loadConversationPage(r.Context(), meta.UserID, conversationID)
	if err != nil {
		status := http.StatusInternalServerError
		if err == sql.ErrNoRows {
			status = http.StatusNotFound
		}
		http.Error(w, "failed to load conversation", status)
		return
	}

	app.render(w, "conversation", PageData{
		AppName:       "bap-search",
		UserID:        meta.UserID,
		Conversations: conversations,
		Conversation:  &conversation,
		CurrentModel:  app.currentModelName(),
	})
}

func (app *App) handleConversationSummaries(w http.ResponseWriter, r *http.Request, conversationID int64) {
	meta := requestMetaFromContext(r.Context())
	conversation, err := app.conversations.GetConversationView(r.Context(), meta.UserID, conversationID)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to load summaries", http.StatusInternalServerError)
		return
	}
	app.renderTemplate(w, "summaries", conversation)
}

func (app *App) handleConversationResults(w http.ResponseWriter, r *http.Request, conversationID int64) {
	meta := requestMetaFromContext(r.Context())
	conversation, err := app.conversations.GetConversationView(r.Context(), meta.UserID, conversationID)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to load results", http.StatusInternalServerError)
		return
	}
	app.renderTemplate(w, "search_results", conversation)
}

func (app *App) handleConversationMessage(w http.ResponseWriter, r *http.Request, conversationID int64) {
	meta := requestMetaFromContext(r.Context())
	message := strings.TrimSpace(r.FormValue("message"))
	if message == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}

	if _, err := app.conversations.GetConversationView(r.Context(), meta.UserID, conversationID); err != nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	if err := app.conversations.AddMessage(r.Context(), conversationID, "user", message); err != nil {
		http.Error(w, "failed to store message", http.StatusInternalServerError)
		return
	}

	memorySummary, err := app.memory.GetUserMemory(r.Context(), meta.UserID)
	if err != nil {
		http.Error(w, "failed to load user memory", http.StatusInternalServerError)
		return
	}

	history, err := app.conversations.GetMessageHistory(r.Context(), meta.UserID, conversationID, app.cfg.MaxChatMessages)
	if err != nil {
		http.Error(w, "failed to build prompt", http.StatusInternalServerError)
		return
	}

	contextText, err := app.conversations.BuildSearchContext(r.Context(), meta.UserID, conversationID, app.cfg.ChatContextChars)
	if err != nil {
		http.Error(w, "failed to build search context", http.StatusInternalServerError)
		return
	}

	reply, err := app.llm.GenerateConversationReply(r.Context(), RequestMeta{
		RequestID:      meta.RequestID,
		UserID:         meta.UserID,
		ConversationID: conversationID,
	}, memorySummary, contextText, history)
	if err != nil {
		loggerWithMeta(r.Context(), app.logger, conversationID).Error("chat generation failed", "error", err)

		failureMessage := chatFailureMessage(err)
		if storeErr := app.conversations.AddMessage(r.Context(), conversationID, "system", failureMessage); storeErr != nil {
			loggerWithMeta(r.Context(), app.logger, conversationID).Error("failed to store chat failure message", "error", storeErr)
			http.Error(w, "failed to generate reply", http.StatusBadGateway)
			return
		}

		updatedConversation, loadErr := app.conversations.GetConversationView(r.Context(), meta.UserID, conversationID)
		if loadErr != nil {
			http.Error(w, "failed to reload messages", http.StatusInternalServerError)
			return
		}

		if r.Header.Get("HX-Request") == "true" {
			app.renderTemplate(w, "chat_messages", updatedConversation)
			return
		}

		http.Redirect(w, r, fmt.Sprintf("/conversations/%d#chat", conversationID), http.StatusSeeOther)
		return
	}

	if err := app.conversations.AddMessage(r.Context(), conversationID, "assistant", reply); err != nil {
		http.Error(w, "failed to store assistant reply", http.StatusInternalServerError)
		return
	}

	go app.memory.MaybeRefreshUserMemory(RequestMeta{
		RequestID:      newRequestID(),
		UserID:         meta.UserID,
		ConversationID: conversationID,
	}, meta.UserID, conversationID)

	updatedConversation, err := app.conversations.GetConversationView(r.Context(), meta.UserID, conversationID)
	if err != nil {
		http.Error(w, "failed to reload messages", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		app.renderTemplate(w, "chat_messages", updatedConversation)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/conversations/%d#chat", conversationID), http.StatusSeeOther)
}

func (app *App) loadConversationPage(ctx context.Context, userID string, conversationID int64) (ConversationView, []ConversationListItem, error) {
	conversation, err := app.conversations.GetConversationView(ctx, userID, conversationID)
	if err != nil {
		return ConversationView{}, nil, err
	}

	conversations, err := app.conversations.ListConversations(ctx, userID)
	if err != nil {
		return ConversationView{}, nil, err
	}

	return conversation, conversations, nil
}

func (app *App) renderTemplate(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := app.templates.ExecuteTemplate(w, name, data); err != nil {
		app.logger.Error("template rendering failed", "error", err, "template", name)
		http.Error(w, "template rendering failed", http.StatusInternalServerError)
	}
}

func (service *ConversationService) EnsureUser(ctx context.Context, userID string) error {
	_, err := service.db.ExecContext(ctx, `
        INSERT INTO users (id) VALUES (?)
        ON CONFLICT(id) DO UPDATE SET last_seen_at = CURRENT_TIMESTAMP
    `, userID)
	return err
}

func (service *ConversationService) CreateConversation(ctx context.Context, userID, query string) (int64, error) {
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	title := query
	if len(title) > 96 {
		title = title[:96]
	}

	result, err := tx.ExecContext(ctx, `
        INSERT INTO conversations (user_id, title) VALUES (?, ?)
    `, userID, title)
	if err != nil {
		return 0, err
	}

	conversationID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `
        INSERT INTO messages (conversation_id, role, content) VALUES (?, 'user', ?)
    `, conversationID, query); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return conversationID, nil
}

func (service *ConversationService) AddMessage(ctx context.Context, conversationID int64, role, content string) error {
	if _, err := service.db.ExecContext(ctx, `
        INSERT INTO messages (conversation_id, role, content) VALUES (?, ?, ?)
    `, conversationID, role, content); err != nil {
		return err
	}
	_, err := service.db.ExecContext(ctx, `
        UPDATE conversations SET updated_at = CURRENT_TIMESTAMP WHERE id = ?
    `, conversationID)
	return err
}

func (service *ConversationService) ListConversations(ctx context.Context, userID string) ([]ConversationListItem, error) {
	rows, err := service.db.QueryContext(ctx, `
        SELECT id, title, updated_at
        FROM conversations
        WHERE user_id = ?
        ORDER BY updated_at DESC
    `, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []ConversationListItem{}
	for rows.Next() {
		var item ConversationListItem
		if err := rows.Scan(&item.ID, &item.Title, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (service *ConversationService) StoreSearchResults(ctx context.Context, conversationID int64, results []SearchResult, engineStatus []SearchEngineStatus) error {
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM search_results WHERE conversation_id = ?`, conversationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM search_engine_statuses WHERE conversation_id = ?`, conversationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM summaries WHERE conversation_id = ?`, conversationID); err != nil {
		return err
	}

	for _, result := range results {
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO search_results (conversation_id, url, title, snippet, rank)
            VALUES (?, ?, ?, ?, ?)
        `, conversationID, result.URL, result.Title, result.Snippet, result.Rank); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
            INSERT INTO summaries (conversation_id, url, summary, source_text, status, status_detail)
            VALUES (?, ?, '', '', 'pending', '')
        `, conversationID, result.URL); err != nil {
			return err
		}
	}

	for _, status := range engineStatus {
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO search_engine_statuses (conversation_id, engine, status, detail, result_count)
            VALUES (?, ?, ?, ?, ?)
        `, conversationID, status.Engine, status.Status, status.Detail, status.ResultCount); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, conversationID); err != nil {
		return err
	}

	return tx.Commit()
}

func (service *ConversationService) StoreSummary(ctx context.Context, conversationID int64, url, summary, sourceText string) error {
	_, err := service.db.ExecContext(ctx, `
        INSERT INTO summaries (conversation_id, url, summary, source_text, status, status_detail)
        VALUES (?, ?, ?, ?, 'ready', '')
        ON CONFLICT(conversation_id, url) DO UPDATE SET
            summary = excluded.summary,
            source_text = excluded.source_text,
            status = 'ready',
            status_detail = '',
            updated_at = CURRENT_TIMESTAMP
    `, conversationID, url, summary, sourceText)
	if err != nil {
		return err
	}

	_, err = service.db.ExecContext(ctx, `UPDATE conversations SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, conversationID)
	return err
}

func (service *ConversationService) UpdateSummaryStatus(ctx context.Context, conversationID int64, url, status, detail string) error {
	_, err := service.db.ExecContext(ctx, `
        INSERT INTO summaries (conversation_id, url, summary, source_text, status, status_detail)
        VALUES (?, ?, '', '', ?, ?)
        ON CONFLICT(conversation_id, url) DO UPDATE SET
            status = excluded.status,
            status_detail = excluded.status_detail,
            updated_at = CURRENT_TIMESTAMP
    `, conversationID, url, status, detail)
	if err != nil {
		return err
	}

	_, err = service.db.ExecContext(ctx, `UPDATE conversations SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, conversationID)
	return err
}

func (service *ConversationService) UpdateSummarySource(ctx context.Context, conversationID int64, url, sourceText string) error {
	_, err := service.db.ExecContext(ctx, `
        INSERT INTO summaries (conversation_id, url, summary, source_text, status, status_detail)
        VALUES (?, ?, '', ?, 'pending', '')
        ON CONFLICT(conversation_id, url) DO UPDATE SET
            source_text = excluded.source_text,
            updated_at = CURRENT_TIMESTAMP
    `, conversationID, url, sourceText)
	if err != nil {
		return err
	}

	_, err = service.db.ExecContext(ctx, `UPDATE conversations SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, conversationID)
	return err
}

func (service *ConversationService) GetConversationView(ctx context.Context, userID string, conversationID int64) (ConversationView, error) {
	conversation := ConversationView{}
	row := service.db.QueryRowContext(ctx, `
        SELECT id, user_id, title, created_at, updated_at
        FROM conversations
        WHERE id = ? AND user_id = ?
    `, conversationID, userID)
	if err := row.Scan(&conversation.ID, &conversation.UserID, &conversation.Title, &conversation.CreatedAt, &conversation.UpdatedAt); err != nil {
		return ConversationView{}, err
	}

	messages, err := service.getMessages(ctx, conversationID)
	if err != nil {
		return ConversationView{}, err
	}
	conversation.Messages = messages

	results, err := service.GetSearchResults(ctx, conversationID)
	if err != nil {
		return ConversationView{}, err
	}
	conversation.SearchResults = results

	engineStatus, err := service.GetSearchEngineStatus(ctx, conversationID)
	if err != nil {
		return ConversationView{}, err
	}
	conversation.EngineStatus = engineStatus

	summaries, err := service.GetSummaries(ctx, conversationID)
	if err != nil {
		return ConversationView{}, err
	}
	conversation.Summaries = summaries
	conversation.SummaryLookup = buildSummaryLookup(summaries)
	conversation.ReadySummaryCount = countReadySummaries(summaries)
	conversation.SummaryTarget = service.summaryTarget
	if conversation.SummaryTarget <= 0 {
		conversation.SummaryTarget = 3
	}
	conversation.OverviewSummary = findOverviewSummary(messages)

	return conversation, nil
}

func (service *ConversationService) GetSearchResults(ctx context.Context, conversationID int64) ([]SearchResult, error) {
	rows, err := service.db.QueryContext(ctx, `
        SELECT url, title, snippet, rank
        FROM search_results
        WHERE conversation_id = ?
        ORDER BY rank ASC
    `, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := []SearchResult{}
	for rows.Next() {
		var item SearchResult
		if err := rows.Scan(&item.URL, &item.Title, &item.Snippet, &item.Rank); err != nil {
			return nil, err
		}
		results = append(results, item)
	}
	return results, rows.Err()
}

func (service *ConversationService) GetSearchEngineStatus(ctx context.Context, conversationID int64) ([]SearchEngineStatus, error) {
	rows, err := service.db.QueryContext(ctx, `
        SELECT engine, status, detail, result_count
        FROM search_engine_statuses
        WHERE conversation_id = ?
        ORDER BY
            CASE status
                WHEN 'error' THEN 0
                WHEN 'timeout' THEN 1
                ELSE 2
            END,
            engine ASC
    `, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	statuses := []SearchEngineStatus{}
	for rows.Next() {
		var item SearchEngineStatus
		if err := rows.Scan(&item.Engine, &item.Status, &item.Detail, &item.ResultCount); err != nil {
			return nil, err
		}
		statuses = append(statuses, item)
	}
	return statuses, rows.Err()
}

func (service *ConversationService) GetSummaries(ctx context.Context, conversationID int64) ([]SummaryRecord, error) {
	rows, err := service.db.QueryContext(ctx, `
	SELECT url, summary, source_text, status, status_detail
        FROM summaries
        WHERE conversation_id = ?
	ORDER BY created_at ASC, id ASC
    `, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	summaries := []SummaryRecord{}
	for rows.Next() {
		var item SummaryRecord
		if err := rows.Scan(&item.URL, &item.Summary, &item.SourceText, &item.Status, &item.Detail); err != nil {
			return nil, err
		}
		summaries = append(summaries, item)
	}
	return summaries, rows.Err()
}

func (service *ConversationService) getMessages(ctx context.Context, conversationID int64) ([]MessageRecord, error) {
	rows, err := service.db.QueryContext(ctx, `
        SELECT id, role, content, timestamp
        FROM messages
        WHERE conversation_id = ?
        ORDER BY timestamp ASC, id ASC
    `, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	messages := []MessageRecord{}
	for rows.Next() {
		var item MessageRecord
		if err := rows.Scan(&item.ID, &item.Role, &item.Content, &item.Timestamp); err != nil {
			return nil, err
		}
		messages = append(messages, item)
	}
	return messages, rows.Err()
}

func (service *ConversationService) GetMessageHistory(ctx context.Context, userID string, conversationID int64, maxMessages int) ([]LLMMessage, error) {
	conversation, err := service.GetConversationView(ctx, userID, conversationID)
	if err != nil {
		return nil, err
	}

	messages := conversation.Messages
	if len(messages) > maxMessages {
		messages = messages[len(messages)-maxMessages:]
	}

	history := make([]LLMMessage, 0, len(messages))
	for _, message := range messages {
		history = append(history, LLMMessage{Role: message.Role, Content: message.Content})
	}
	return history, nil
}

func (service *ConversationService) BuildSearchContext(ctx context.Context, userID string, conversationID int64, maxChars int) (string, error) {
	conversation, err := service.GetConversationView(ctx, userID, conversationID)
	if err != nil {
		return "", err
	}

	if maxChars <= 0 {
		maxChars = 4200
	}

	builder := strings.Builder{}
	for _, summary := range conversation.Summaries {
		if summary.Status != "ready" {
			continue
		}

		excerpt := compactContextText(summary.SourceText, 700)
		block := fmt.Sprintf("URL: %s\nSummary:\n%s\n", summary.URL, compactContextText(summary.Summary, 1400))
		if excerpt != "" {
			block += fmt.Sprintf("\nExtracted text excerpt:\n%s\n", excerpt)
		}
		block += "\n"
		if builder.Len()+len(block) > maxChars {
			remaining := maxChars - builder.Len()
			if remaining > 0 {
				builder.WriteString(block[:remaining])
			}
			break
		}
		builder.WriteString(block)
	}

	if builder.Len() == 0 {
		for _, result := range conversation.SearchResults {
			block := fmt.Sprintf("URL: %s\nTitle: %s\nSnippet: %s\n\n", result.URL, compactContextText(result.Title, 180), compactContextText(result.Snippet, 500))
			if builder.Len()+len(block) > maxChars {
				remaining := maxChars - builder.Len()
				if remaining > 0 {
					builder.WriteString(block[:remaining])
				}
				break
			}
			builder.WriteString(block)
		}
	}

	return builder.String(), nil
}

func compactContextText(value string, maxChars int) string {
	value = strings.TrimSpace(value)
	if maxChars <= 0 || len(value) <= maxChars {
		return value
	}

	if maxChars <= len("...[truncated]") {
		return value[:maxChars]
	}

	return strings.TrimSpace(value[:maxChars-len("...[truncated]")]) + "...[truncated]"
}

func chatFailureMessage(err error) string {
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "exceeds the available context size") {
		return "The model could not answer because the prompt exceeded the current context window. Future prompts are trimmed more aggressively now, but for this turn try a shorter follow-up or increase LLAMA_CTX_SIZE together with BAP_LLM_CONTEXT_TOKENS."
	}

	return "The model request failed before a reply was generated. Please try again."
}

func buildSummaryLookup(summaries []SummaryRecord) map[string]SummaryRecord {
	lookup := make(map[string]SummaryRecord, len(summaries))
	for _, summary := range summaries {
		lookup[summary.URL] = summary
	}
	return lookup
}

func countReadySummaries(summaries []SummaryRecord) int {
	count := 0
	for _, summary := range summaries {
		if summary.Status == "ready" && strings.TrimSpace(summary.Summary) != "" {
			count++
		}
	}
	return count
}

func findOverviewSummary(messages []MessageRecord) string {
	for _, message := range messages {
		if message.Role != "assistant" {
			continue
		}

		content := strings.TrimSpace(message.Content)
		if content != "" {
			return content
		}
	}

	return ""
}

func (service *ConversationService) BuildTranscript(ctx context.Context, conversationID int64, limit int) (string, error) {
	messages, err := service.getMessages(ctx, conversationID)
	if err != nil {
		return "", err
	}
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}

	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		parts = append(parts, fmt.Sprintf("%s: %s", message.Role, message.Content))
	}
	return strings.Join(parts, "\n\n"), nil
}
