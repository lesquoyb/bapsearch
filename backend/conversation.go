package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var needMoreSearchPattern = regexp.MustCompile(`(?is)<<\s*NEED_MORE_SEARCH\s*:(.*?)(?:>>|$)`)

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
	ID            int64
	Role          string
	Content       string
	Reasoning     string
	Timestamp     time.Time
	SkipInDisplay bool
}

type SummaryRecord struct {
	URL             string
	Summary         string
	SourceText      string
	Status          string
	Detail          string
	SimilarityScore float64
	RerankPosition  int
}

type ConversationView struct {
	ID                   int64
	UserID               string
	Title                string
	RewrittenQuery       string
	RewriteStatus        string
	RewriteDetail        string
	AnswerStatus         string
	AnswerDetail         string
	OriginalResultCount  int
	RewrittenResultCount int
	CreatedAt            time.Time
	UpdatedAt            time.Time
	Messages             []MessageRecord
	SearchResults        []SearchResult
	EngineStatus         []SearchEngineStatus
	Summaries            []SummaryRecord
	SummaryLookup        map[string]SummaryRecord
	ReadySummaryCount    int
	SummaryTarget        int
	OverviewSummary      string
	ResultsDisplayLimit  int
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

	if err := app.conversations.UpdateAnswerStatus(r.Context(), conversationID, "searching", "Preparing the search pipeline."); err != nil {
		loggerWithMeta(r.Context(), app.logger, conversationID).Error("initializing answer status failed", "error", err)
	}

	app.summaryJobs <- SummaryJob{
		ConversationID: conversationID,
		UserID:         meta.UserID,
		Query:          query,
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
	case len(parts) == 2 && parts[1] == "delete" && r.Method == http.MethodPost:
		app.handleConversationDelete(w, r, conversationID)
	case len(parts) == 2 && parts[1] == "results" && r.Method == http.MethodGet:
		app.handleConversationResults(w, r, conversationID)
	case len(parts) == 3 && parts[1] == "answer" && parts[2] == "stream" && r.Method == http.MethodGet:
		app.handleConversationAnswerStream(w, r, conversationID)
	case len(parts) == 2 && parts[1] == "summaries" && r.Method == http.MethodGet:
		app.handleConversationSummaries(w, r, conversationID)
	case len(parts) == 3 && parts[1] == "summaries" && parts[2] == "regenerate" && r.Method == http.MethodPost:
		app.handleConversationRegenerateSummaries(w, r, conversationID)
	case len(parts) == 2 && parts[1] == "messages" && r.Method == http.MethodGet:
		app.handleConversationMessagesGet(w, r, conversationID)
	case len(parts) == 2 && parts[1] == "messages" && r.Method == http.MethodPost:
		app.handleConversationMessage(w, r, conversationID)
	case len(parts) == 3 && parts[1] == "messages" && parts[2] == "stream" && r.Method == http.MethodPost:
		app.handleConversationMessageStream(w, r, conversationID)
	case len(parts) == 5 && parts[1] == "messages" && parts[3] == "regenerate" && parts[4] == "stream" && r.Method == http.MethodPost:
		messageID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		app.handleConversationMessageRegenerateStream(w, r, conversationID, messageID)
	case len(parts) == 3 && parts[1] == "search-more" && parts[2] == "stream" && r.Method == http.MethodPost:
		app.handleSearchMoreStream(w, r, conversationID)
	case len(parts) == 3 && parts[1] == "force-answer" && parts[2] == "stream" && r.Method == http.MethodPost:
		app.handleForceAnswerStream(w, r, conversationID)
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
	conversation.ResultsDisplayLimit = app.cfg.ResultsDisplayLimit

	app.render(w, "conversation", PageData{
		AppName:       "bap-search",
		UserID:        meta.UserID,
		Conversations: conversations,
		Conversation:  &conversation,
		CurrentModel:  app.currentModelName(),
		Status:        r.URL.Query().Get("status"),
	})
}

func (app *App) handleConversationDelete(w http.ResponseWriter, r *http.Request, conversationID int64) {
	meta := requestMetaFromContext(r.Context())
	if err := app.conversations.DeleteConversation(r.Context(), meta.UserID, conversationID); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to delete conversation", http.StatusInternalServerError)
		return
	}

	loggerWithMeta(r.Context(), app.logger, conversationID).Info("conversation_deleted")
	http.Redirect(w, r, "/?status=conversation+deleted", http.StatusSeeOther)
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
	conversation.ResultsDisplayLimit = app.cfg.ResultsDisplayLimit
	app.renderTemplate(w, "search_results", conversation)
}

func (app *App) handleConversationMessagesGet(w http.ResponseWriter, r *http.Request, conversationID int64) {
	meta := requestMetaFromContext(r.Context())
	conversation, err := app.conversations.GetConversationView(r.Context(), meta.UserID, conversationID)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to load messages", http.StatusInternalServerError)
		return
	}
	app.renderTemplate(w, "chat_messages", conversation)
}

func (app *App) handleConversationRegenerateSummaries(w http.ResponseWriter, r *http.Request, conversationID int64) {
	meta := requestMetaFromContext(r.Context())

	conversation, err := app.conversations.GetConversationView(r.Context(), meta.UserID, conversationID)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to load conversation", http.StatusInternalServerError)
		return
	}

	if err := app.conversations.ResetSummaries(r.Context(), conversationID); err != nil {
		http.Error(w, "failed to reset summaries", http.StatusInternalServerError)
		return
	}

	loggerWithMeta(r.Context(), app.logger, conversationID).Info("summary_regeneration_requested")

	urls := make([]string, 0, len(conversation.SearchResults))
	for _, result := range conversation.SearchResults {
		urls = append(urls, result.URL)
	}
	app.fetch.Invalidate(urls)

	app.summaryJobs <- SummaryJob{
		ConversationID: conversationID,
		UserID:         meta.UserID,
		Query:          conversation.Title,
		Results:        conversation.SearchResults,
		ForceFull:      true,
	}

	if r.Header.Get("HX-Request") == "true" {
		updated, err := app.conversations.GetConversationView(r.Context(), meta.UserID, conversationID)
		if err != nil {
			http.Error(w, "failed to reload summaries", http.StatusInternalServerError)
			return
		}
		app.renderTemplate(w, "summaries", updated)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/conversations/%d", conversationID), http.StatusSeeOther)
}

func (app *App) handleConversationAnswerStream(w http.ResponseWriter, r *http.Request, conversationID int64) {
	meta := requestMetaFromContext(r.Context())
	conversation, err := app.conversations.GetConversationView(r.Context(), meta.UserID, conversationID)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to load conversation", http.StatusInternalServerError)
		return
	}

	if strings.TrimSpace(conversation.OverviewSummary) != "" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, "event: done\ndata: \"\"\n\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		return
	}

	// If a previous streaming attempt was interrupted (user navigated away),
	// answer_status may be stuck on "streaming". Reset it so WaitForAnswerReady
	// can proceed once the pipeline's ranked sources are available.
	if conversation.AnswerStatus == "streaming" || conversation.AnswerStatus == "error" {
		_ = app.conversations.UpdateAnswerStatus(r.Context(), conversationID, "ready", "Retrying after interrupted stream.")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	writeEvent := func(event, data string) {
		encoded, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
		fl.Flush()
	}

	writeEvent("status", "Waiting for the ranked sources.")

	readyConversation, err := app.conversations.WaitForAnswerReady(r.Context(), meta.UserID, conversationID, 90*time.Second, func(status, detail string) {
		message := strings.TrimSpace(detail)
		if message == "" {
			message = strings.TrimSpace(status)
		}
		writeEvent("status", message)
	})
	if err != nil {
		writeEvent("error", err.Error())
		return
	}

	sources, err := app.conversations.ListTopRankedSources(r.Context(), conversationID, app.cfg.ContextDocCount)
	if err != nil {
		writeEvent("error", "Failed to load ranked sources.")
		return
	}
	if len(sources) == 0 {
		writeEvent("error", "No ranked source is available yet.")
		return
	}

	if err := app.conversations.UpdateAnswerStatus(r.Context(), conversationID, "streaming", "Streaming the final grounded answer."); err != nil {
		loggerWithMeta(r.Context(), app.logger, conversationID).Error("updating answer status failed", "error", err)
	}

	maxSearchLoops := app.maxSearchLoops(r.Context())
	searchLoop := 0
	currentSources := sources
	currentQuery := readyConversation.RewrittenQuery
	var reply string
	var reasoningBuf strings.Builder

	for searchLoop < maxSearchLoops {
		replyMeta := RequestMeta{RequestID: meta.RequestID, UserID: meta.UserID, ConversationID: conversationID}

		reply, err = app.llm.GenerateGroundedSearchAnswerStream(r.Context(), replyMeta, readyConversation.Title, currentQuery, currentSources, func(token string) {
			reasoningBuf.WriteString(token)
			writeEvent("reasoning", token)
		})
		if err != nil {
			loggerWithMeta(r.Context(), app.logger, conversationID).Error("grounded answer generation failed", "error", err)
			_ = app.conversations.UpdateAnswerStatus(context.Background(), conversationID, "error", err.Error())
			writeEvent("error", chatFailureMessage(err))
			return
		}

		newSearchQuery, trigger := app.resolveFollowUpSearchQuery(r.Context(), replyMeta, readyConversation.Title, reply)
		if newSearchQuery == "" {
			break
		}

		searchLoop++
		_ = app.conversations.AddSearchQueryMessage(context.Background(), conversationID, newSearchQuery)
		loggerWithMeta(r.Context(), app.logger, conversationID).Info("follow_up_search_requested",
			"path", "answer_stream",
			"loop", searchLoop,
			"trigger", trigger,
			"query", newSearchQuery,
			"current_sources", len(currentSources),
		)
		writeEvent("search_query", newSearchQuery)

		intentEmb := app.generateIntentEmbedding(r.Context(), replyMeta, newSearchQuery, nil)
		newSources, processErr := app.inlineSearchAndProcess(r.Context(), replyMeta, conversationID, newSearchQuery, intentEmb)
		if processErr != nil || len(newSources) == 0 {
			loggerWithMeta(r.Context(), app.logger, conversationID).Error("iterative search processing failed", "trigger", trigger, "query", newSearchQuery, "error", processErr)
			break
		}

		currentSources = newSources
		if app.cfg.SummarizeURLLimit > 0 && len(currentSources) > app.cfg.SummarizeURLLimit {
			currentSources = currentSources[:app.cfg.SummarizeURLLimit]
		}
		currentQuery = newSearchQuery
	}

	if detectNeedMoreSearch(reply) != "" || app.shouldAutoSearchReply(reply, readyConversation.Title) {
		loggerWithMeta(r.Context(), app.logger, conversationID).Info("follow_up_search_limit_reached",
			"path", "answer_stream",
			"max_search_loops", maxSearchLoops,
		)
		pendingQuery := detectNeedMoreSearch(reply)
		if pendingQuery == "" {
			pendingQuery = readyConversation.Title
		}

		cleaned := stripNeedMoreSearch(reply)
		if cleaned != "" {
			encoded, _ := json.Marshal(cleaned)
			fmt.Fprintf(w, "data: %s\n\n", encoded)
			fl.Flush()
			_ = app.conversations.AddMessageWithReasoning(context.Background(), conversationID, "assistant", cleaned, reasoningBuf.String())
			_ = app.conversations.UpdateAnswerStatus(context.Background(), conversationID, "complete", "Answer generated (search limit reached).")
		}

		writeEvent("search_limit", pendingQuery)
		writeEvent("done", "")
		return
	}
	encoded, _ := json.Marshal(reply)
	fmt.Fprintf(w, "data: %s\n\n", encoded)
	fl.Flush()

	if err := app.conversations.AddMessageWithReasoning(context.Background(), conversationID, "assistant", reply, reasoningBuf.String()); err != nil {
		loggerWithMeta(r.Context(), app.logger, conversationID).Error("storing assistant answer failed", "error", err)
		writeEvent("error", "The answer was generated but could not be stored.")
		return
	}
	if err := app.conversations.UpdateAnswerStatus(context.Background(), conversationID, "complete", "Grounded answer generated."); err != nil {
		loggerWithMeta(r.Context(), app.logger, conversationID).Error("finalizing answer status failed", "error", err)
	}

	go app.memory.MaybeRefreshUserMemory(RequestMeta{RequestID: newRequestID(), UserID: meta.UserID, ConversationID: conversationID}, meta.UserID, conversationID)

	writeEvent("done", "")
}

func (app *App) handleConversationMessageStream(w http.ResponseWriter, r *http.Request, conversationID int64) {
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

	contextText, err := app.conversations.BuildSearchContext(r.Context(), meta.UserID, conversationID, app.cfg.ChatContextChars, app.cfg.ContextDocCount)
	if err != nil {
		http.Error(w, "failed to build search context", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	writeEvent := func(event, data string) {
		encoded, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
		fl.Flush()
	}

	replyMeta := RequestMeta{
		RequestID:      meta.RequestID,
		UserID:         meta.UserID,
		ConversationID: conversationID,
	}

	maxChatSearchLoops := app.maxSearchLoops(r.Context())
	var reply string
	var reasoningBuf strings.Builder

	for loop := 0; loop < maxChatSearchLoops; loop++ {
		reply, err = app.llm.GenerateConversationReplyStream(r.Context(), replyMeta, memorySummary, contextText, history, func(token string) {
			reasoningBuf.WriteString(token)
			writeEvent("reasoning", token)
		})

		if err != nil {
			loggerWithMeta(r.Context(), app.logger, conversationID).Error("stream chat generation failed", "error", err)
			failureMessage := chatFailureMessage(err)
			_ = app.conversations.AddMessage(r.Context(), conversationID, "system", failureMessage)
			writeEvent("error", failureMessage)
			return
		}

		newSearchQuery, trigger := app.resolveFollowUpSearchQuery(r.Context(), replyMeta, message, reply)
		if newSearchQuery == "" {
			break
		}

		_ = app.conversations.AddSearchQueryMessage(context.Background(), conversationID, newSearchQuery)
		loggerWithMeta(r.Context(), app.logger, conversationID).Info("follow_up_search_requested",
			"path", "message_stream",
			"loop", loop+1,
			"trigger", trigger,
			"query", newSearchQuery,
		)
		writeEvent("search_query", newSearchQuery)

		intentEmb := app.generateIntentEmbedding(r.Context(), replyMeta, newSearchQuery, history)
		_, processErr := app.inlineSearchAndProcess(r.Context(), replyMeta, conversationID, newSearchQuery, intentEmb)
		if processErr != nil {
			loggerWithMeta(r.Context(), app.logger, conversationID).Error("iterative search processing failed", "trigger", trigger, "query", newSearchQuery, "error", processErr)
			break
		}

		// Rebuild context with the re-ranked sources
		contextText, err = app.conversations.BuildSearchContext(r.Context(), meta.UserID, conversationID, app.cfg.ChatContextChars, app.cfg.ContextDocCount)
		if err != nil {
			break
		}
	}

	if detectNeedMoreSearch(reply) != "" || app.shouldAutoSearchReply(reply, message) {
		loggerWithMeta(r.Context(), app.logger, conversationID).Info("follow_up_search_limit_reached",
			"path", "message_stream",
			"max_search_loops", maxChatSearchLoops,
		)
		pendingQuery := detectNeedMoreSearch(reply)
		if pendingQuery == "" {
			pendingQuery = message
		}

		cleaned := stripNeedMoreSearch(reply)
		if cleaned != "" {
			encoded, _ := json.Marshal(cleaned)
			fmt.Fprintf(w, "data: %s\n\n", encoded)
			fl.Flush()
			_ = app.conversations.AddMessageWithReasoning(context.Background(), conversationID, "assistant", cleaned, reasoningBuf.String())
		}

		writeEvent("search_limit", pendingQuery)
		writeEvent("done", "")
		return
	}
	encoded, _ := json.Marshal(reply)
	fmt.Fprintf(w, "data: %s\n\n", encoded)
	fl.Flush()

	if err := app.conversations.AddMessageWithReasoning(context.Background(), conversationID, "assistant", reply, reasoningBuf.String()); err != nil {
		loggerWithMeta(r.Context(), app.logger, conversationID).Error("failed to store streamed reply", "error", err)
	}

	go app.memory.MaybeRefreshUserMemory(RequestMeta{
		RequestID:      newRequestID(),
		UserID:         meta.UserID,
		ConversationID: conversationID,
	}, meta.UserID, conversationID)

	writeEvent("done", "")
}

func (app *App) handleConversationMessageRegenerateStream(w http.ResponseWriter, r *http.Request, conversationID, assistantMessageID int64) {
	meta := requestMetaFromContext(r.Context())

	message, err := app.conversations.TruncateConversationForRegeneration(r.Context(), meta.UserID, conversationID, assistantMessageID)
	if err != nil {
		status := http.StatusInternalServerError
		if err == sql.ErrNoRows {
			status = http.StatusNotFound
		}
		http.Error(w, "failed to regenerate message", status)
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

	contextText, err := app.conversations.BuildSearchContext(r.Context(), meta.UserID, conversationID, app.cfg.ChatContextChars, app.cfg.ContextDocCount)
	if err != nil {
		http.Error(w, "failed to build search context", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	writeEvent := func(event, data string) {
		encoded, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
		fl.Flush()
	}

	replyMeta := RequestMeta{
		RequestID:      meta.RequestID,
		UserID:         meta.UserID,
		ConversationID: conversationID,
	}

	maxChatSearchLoops := app.maxSearchLoops(r.Context())
	var reply string
	var reasoningBuf strings.Builder

	for loop := 0; loop < maxChatSearchLoops; loop++ {
		reply, err = app.llm.GenerateConversationReplyStream(r.Context(), replyMeta, memorySummary, contextText, history, func(token string) {
			reasoningBuf.WriteString(token)
			writeEvent("reasoning", token)
		})

		if err != nil {
			loggerWithMeta(r.Context(), app.logger, conversationID).Error("stream chat regeneration failed", "error", err)
			failureMessage := chatFailureMessage(err)
			_ = app.conversations.AddMessage(r.Context(), conversationID, "system", failureMessage)
			writeEvent("error", failureMessage)
			return
		}

		newSearchQuery, trigger := app.resolveFollowUpSearchQuery(r.Context(), replyMeta, message, reply)
		if newSearchQuery == "" {
			break
		}

		_ = app.conversations.AddSearchQueryMessage(context.Background(), conversationID, newSearchQuery)
		loggerWithMeta(r.Context(), app.logger, conversationID).Info("follow_up_search_requested",
			"path", "message_regenerate_stream",
			"loop", loop+1,
			"trigger", trigger,
			"query", newSearchQuery,
		)
		writeEvent("search_query", newSearchQuery)

		intentEmb := app.generateIntentEmbedding(r.Context(), replyMeta, newSearchQuery, history)
		_, processErr := app.inlineSearchAndProcess(r.Context(), replyMeta, conversationID, newSearchQuery, intentEmb)
		if processErr != nil {
			loggerWithMeta(r.Context(), app.logger, conversationID).Error("iterative search processing failed", "trigger", trigger, "query", newSearchQuery, "error", processErr)
			break
		}

		contextText, err = app.conversations.BuildSearchContext(r.Context(), meta.UserID, conversationID, app.cfg.ChatContextChars, app.cfg.ContextDocCount)
		if err != nil {
			break
		}
	}

	if detectNeedMoreSearch(reply) != "" || app.shouldAutoSearchReply(reply, message) {
		loggerWithMeta(r.Context(), app.logger, conversationID).Info("follow_up_search_limit_reached",
			"path", "message_regenerate_stream",
			"max_search_loops", maxChatSearchLoops,
		)
		pendingQuery := detectNeedMoreSearch(reply)
		if pendingQuery == "" {
			pendingQuery = message
		}

		cleaned := stripNeedMoreSearch(reply)
		if cleaned != "" {
			encoded, _ := json.Marshal(cleaned)
			fmt.Fprintf(w, "data: %s\n\n", encoded)
			fl.Flush()
			_ = app.conversations.AddMessageWithReasoning(context.Background(), conversationID, "assistant", cleaned, reasoningBuf.String())
		}

		writeEvent("search_limit", pendingQuery)
		writeEvent("done", "")
		return
	}

	encoded, _ := json.Marshal(reply)
	fmt.Fprintf(w, "data: %s\n\n", encoded)
	fl.Flush()

	if err := app.conversations.AddMessageWithReasoning(context.Background(), conversationID, "assistant", reply, reasoningBuf.String()); err != nil {
		loggerWithMeta(r.Context(), app.logger, conversationID).Error("failed to store regenerated reply", "error", err)
	}

	go app.memory.MaybeRefreshUserMemory(RequestMeta{
		RequestID:      newRequestID(),
		UserID:         meta.UserID,
		ConversationID: conversationID,
	}, meta.UserID, conversationID)

	writeEvent("done", "")
}

func (app *App) handleSearchMoreStream(w http.ResponseWriter, r *http.Request, conversationID int64) {
	meta := requestMetaFromContext(r.Context())
	query := strings.TrimSpace(r.FormValue("query"))
	if query == "" {
		http.Error(w, "query is required", http.StatusBadRequest)
		return
	}

	conv, err := app.conversations.GetConversationView(r.Context(), meta.UserID, conversationID)
	if err != nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	writeEvent := func(event, data string) {
		encoded, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
		fl.Flush()
	}

	replyMeta := RequestMeta{RequestID: meta.RequestID, UserID: meta.UserID, ConversationID: conversationID}

	history, err := app.conversations.GetMessageHistory(r.Context(), meta.UserID, conversationID, app.cfg.MaxChatMessages)
	if err != nil {
		writeEvent("error", "Failed to load conversation history.")
		return
	}
	if len(history) == 0 {
		history = []LLMMessage{{Role: "user", Content: conv.Title}}
	}

	_ = app.conversations.AddSearchQueryMessage(context.Background(), conversationID, query)
	writeEvent("search_query", query)
	intentEmb := app.generateIntentEmbedding(r.Context(), replyMeta, query, history)
	_, processErr := app.inlineSearchAndProcess(r.Context(), replyMeta, conversationID, query, intentEmb)
	if processErr != nil {
		loggerWithMeta(r.Context(), app.logger, conversationID).Error("search-more processing failed", "error", processErr)
	}

	memorySummary, _ := app.memory.GetUserMemory(r.Context(), meta.UserID)
	contextText, err := app.conversations.BuildSearchContext(r.Context(), meta.UserID, conversationID, app.cfg.ChatContextChars, app.cfg.ContextDocCount)
	if err != nil {
		writeEvent("error", "Failed to build search context.")
		return
	}

	var reply string
	var reasoningBuf strings.Builder
	maxLoops := app.maxSearchLoops(r.Context())

	for loop := 0; loop < maxLoops; loop++ {
		reply, err = app.llm.GenerateConversationReplyStream(r.Context(), replyMeta, memorySummary, contextText, history, func(token string) {
			reasoningBuf.WriteString(token)
			writeEvent("reasoning", token)
		})
		if err != nil {
			loggerWithMeta(r.Context(), app.logger, conversationID).Error("search-more generation failed", "error", err)
			writeEvent("error", chatFailureMessage(err))
			return
		}

		newSearchQuery, trigger := app.resolveFollowUpSearchQuery(r.Context(), replyMeta, query, reply)
		if newSearchQuery == "" {
			break
		}

		_ = app.conversations.AddSearchQueryMessage(context.Background(), conversationID, newSearchQuery)
		loggerWithMeta(r.Context(), app.logger, conversationID).Info("follow_up_search_requested",
			"path", "search_more",
			"loop", loop+1,
			"trigger", trigger,
			"query", newSearchQuery,
		)
		writeEvent("search_query", newSearchQuery)

		moreIntentEmb := app.generateIntentEmbedding(r.Context(), replyMeta, newSearchQuery, history)
		_, processErr := app.inlineSearchAndProcess(r.Context(), replyMeta, conversationID, newSearchQuery, moreIntentEmb)
		if processErr != nil {
			break
		}

		contextText, err = app.conversations.BuildSearchContext(r.Context(), meta.UserID, conversationID, app.cfg.ChatContextChars, app.cfg.ContextDocCount)
		if err != nil {
			break
		}
	}

	if detectNeedMoreSearch(reply) != "" || app.shouldAutoSearchReply(reply, query) {
		pendingQuery := detectNeedMoreSearch(reply)
		if pendingQuery == "" {
			pendingQuery = query
		}

		cleaned := stripNeedMoreSearch(reply)
		if cleaned != "" {
			encoded, _ := json.Marshal(cleaned)
			fmt.Fprintf(w, "data: %s\n\n", encoded)
			fl.Flush()
			_ = app.conversations.AddMessageWithReasoning(context.Background(), conversationID, "assistant", cleaned, reasoningBuf.String())
			_ = app.conversations.UpdateAnswerStatus(context.Background(), conversationID, "complete", "Answer generated after additional search.")
		}

		writeEvent("search_limit", pendingQuery)
		writeEvent("done", "")
		return
	}

	encoded, _ := json.Marshal(reply)
	fmt.Fprintf(w, "data: %s\n\n", encoded)
	fl.Flush()

	if err := app.conversations.AddMessageWithReasoning(context.Background(), conversationID, "assistant", reply, reasoningBuf.String()); err != nil {
		loggerWithMeta(r.Context(), app.logger, conversationID).Error("search-more storing reply failed", "error", err)
	}
	_ = app.conversations.UpdateAnswerStatus(context.Background(), conversationID, "complete", "Answer generated after additional search.")

	go app.memory.MaybeRefreshUserMemory(RequestMeta{RequestID: newRequestID(), UserID: meta.UserID, ConversationID: conversationID}, meta.UserID, conversationID)

	writeEvent("done", "")
}

func (app *App) handleForceAnswerStream(w http.ResponseWriter, r *http.Request, conversationID int64) {
	meta := requestMetaFromContext(r.Context())

	conv, err := app.conversations.GetConversationView(r.Context(), meta.UserID, conversationID)
	if err != nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	writeEvent := func(event, data string) {
		encoded, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
		fl.Flush()
	}

	replyMeta := RequestMeta{RequestID: meta.RequestID, UserID: meta.UserID, ConversationID: conversationID}

	memorySummary, _ := app.memory.GetUserMemory(r.Context(), meta.UserID)
	contextText, err := app.conversations.BuildSearchContext(r.Context(), meta.UserID, conversationID, app.cfg.ChatContextChars, app.cfg.ContextDocCount)
	if err != nil {
		writeEvent("error", "Failed to build search context.")
		return
	}

	history, err := app.conversations.GetMessageHistory(r.Context(), meta.UserID, conversationID, app.cfg.MaxChatMessages)
	if err != nil {
		writeEvent("error", "Failed to load conversation history.")
		return
	}
	if len(history) == 0 {
		history = []LLMMessage{{Role: "user", Content: conv.Title}}
	}

	var reasoningBuf strings.Builder
	reply, err := app.llm.GenerateConversationForceReplyStream(r.Context(), replyMeta, memorySummary, contextText, history, func(token string) {
		reasoningBuf.WriteString(token)
		writeEvent("reasoning", token)
	})
	if err != nil {
		loggerWithMeta(r.Context(), app.logger, conversationID).Error("force-answer generation failed", "error", err)
		writeEvent("error", chatFailureMessage(err))
		return
	}

	encoded, _ := json.Marshal(reply)
	fmt.Fprintf(w, "data: %s\n\n", encoded)
	fl.Flush()

	if err := app.conversations.AddMessageWithReasoning(context.Background(), conversationID, "assistant", reply, reasoningBuf.String()); err != nil {
		loggerWithMeta(r.Context(), app.logger, conversationID).Error("force-answer storing reply failed", "error", err)
	}
	_ = app.conversations.UpdateAnswerStatus(context.Background(), conversationID, "complete", "Answer generated with available sources.")

	go app.memory.MaybeRefreshUserMemory(RequestMeta{RequestID: newRequestID(), UserID: meta.UserID, ConversationID: conversationID}, meta.UserID, conversationID)

	writeEvent("done", "")
}

// detectNeedMoreSearch scans LLM output for the <<NEED_MORE_SEARCH: query>> signal.
func detectNeedMoreSearch(text string) string {
	matches := needMoreSearchPattern.FindStringSubmatch(text)
	if len(matches) < 2 {
		return ""
	}
	query := strings.TrimSpace(matches[1])
	if cleaned := sanitizeSearchQuery(query); cleaned != "" {
		return cleaned
	}
	if query != "" {
		return query
	}
	return ""
}

// stripNeedMoreSearch removes the <<NEED_MORE_SEARCH: ...>> tag from a reply,
// returning only the real answer content (if any).
func stripNeedMoreSearch(text string) string {
	return strings.TrimSpace(needMoreSearchPattern.ReplaceAllString(text, ""))
}

func (app *App) maxSearchLoops(ctx context.Context) int {
	value := strings.TrimSpace(app.conversations.GetSetting(ctx, "max_search_loops", "3"))
	loops, err := strconv.Atoi(value)
	if err != nil || loops <= 0 {
		return 3
	}
	if loops > 6 {
		return 6
	}
	return loops
}

func (app *App) resolveFollowUpSearchQuery(ctx context.Context, meta RequestMeta, userRequest, reply string) (string, string) {
	if explicit := detectNeedMoreSearch(reply); explicit != "" {
		// Always rewrite the model's raw query for better search quality.
		if rewritten, err := app.llm.RewriteSearchQuery(ctx, meta, explicit); err == nil && strings.TrimSpace(rewritten) != "" {
			return rewritten, "model_need_more_search:rewritten"
		}
		return explicit, "model_need_more_search"
	}
	trigger := followUpSearchTrigger(userRequest, reply)
	if trigger == "" {
		return "", ""
	}
	if rewritten, err := app.llm.RewriteSearchQuery(ctx, meta, userRequest); err == nil && strings.TrimSpace(rewritten) != "" {
		return rewritten, trigger + ":rewrite_model"
	}
	if cleaned := sanitizeSearchQuery(userRequest); cleaned != "" {
		return cleaned, trigger + ":sanitized_user_request"
	}
	return strings.TrimSpace(userRequest), trigger + ":raw_user_request"
}

func (app *App) shouldAutoSearchReply(reply, userRequest string) bool {
	return followUpSearchTrigger(userRequest, reply) != ""
}

func followUpSearchTrigger(userRequest, reply string) string {
	replyText := strings.ToLower(strings.TrimSpace(reply))
	requestText := strings.ToLower(strings.TrimSpace(userRequest))
	if replyText == "" || requestText == "" {
		return ""
	}

	// --- Path 1: LLM deflection (suggesting user visit websites / search manually) ---
	// This is ALWAYS a failure regardless of what the user asked.
	deflectionSignals := []string{
		// English
		"suggest you visit", "recommend visiting", "suggest you consult",
		"recommend you consult", "you can find it at", "try searching for",
		"search on google", "visit the website", "check out the site",
		"i recommend clicking", "click directly",
		// French
		"je vous suggère de consulter", "je vous suggere de consulter",
		"je vous recommande de consulter", "vous pouvez consulter",
		"consulter directement", "tapez", "souhaitez-vous que je",
		"je vous recommande de cliquer", "cliquez directement",
		"vous pouvez trouver", "rendez-vous sur",
	}
	for _, signal := range deflectionSignals {
		if strings.Contains(replyText, signal) {
			return "fallback_llm_deflection"
		}
	}

	// --- Path 2: User asked for search + reply says context insufficient ---
	explicitSearchSignals := []string{
		"search", "find", "look up", "browse", "follow links", "search again",
		"cherche", "recherche", "trouve", "parcours", "suis des liens", "refais une recherche",
	}
	// Implicit search signals: user asks for something different/more
	implicitSearchSignals := []string{
		"another", "a different", "other", "something else", "more options",
		"autre", "un autre", "une autre", "différent", "differente",
		"d'autres", "encore", "donne-moi", "montre-moi", "propose-moi",
	}
	requestAskedForSearch := false
	for _, signal := range explicitSearchSignals {
		if strings.Contains(requestText, signal) {
			requestAskedForSearch = true
			break
		}
	}
	if !requestAskedForSearch {
		for _, signal := range implicitSearchSignals {
			if strings.Contains(requestText, signal) {
				requestAskedForSearch = true
				break
			}
		}
	}
	if !requestAskedForSearch {
		return ""
	}

	insufficientSignals := []string{
		// English
		"insufficient", "not enough information", "not in the provided context",
		"current sources", "do not provide", "don't provide", "does not contain",
		"doesn't contain", "not available in", "no other", "only recipe",
		"the only", "cannot find", "can't find",
		// French
		"pas assez d'informations", "sources actuelles", "ne fournissent",
		"ne contient pas", "ne contiennent pas", "le contexte actuel",
		"n'est pas disponible", "la seule recette", "la seule",
		"ne donnent pas", "sans les étapes", "pas d'autre",
	}
	for _, signal := range insufficientSignals {
		if strings.Contains(replyText, signal) {
			return "fallback_insufficient_context"
		}
	}
	return ""
}

// generateIntentEmbedding asks the LLM to describe the ideal answer document,
// then embeds it. The resulting vector produces better cosine-similarity matches
// than embedding the raw query. Returns nil on any error so callers can fall
// back to the plain query embedding.
func (app *App) generateIntentEmbedding(ctx context.Context, meta RequestMeta, query string, history []LLMMessage) []float64 {
	intent, err := app.llm.GenerateSearchIntent(ctx, meta, query, history)
	if err != nil || strings.TrimSpace(intent) == "" {
		return nil
	}
	embedding, err := app.llm.EmbedText(ctx, meta, intent)
	if err != nil {
		return nil
	}
	return embedding
}

// inlineSearchAndProcess performs a web search, fetches the result pages,
// embeds them, stores them, and then re-ranks ALL documents in the conversation
// by cosine similarity to the given query. It returns the full ranked list.
// If intentEmbedding is non-nil it is used for re-ranking instead of the raw
// query embedding — this typically comes from a short LLM-generated paragraph
// that describes the ideal answer document and embeds more meaningfully.
func (app *App) inlineSearchAndProcess(ctx context.Context, meta RequestMeta, conversationID int64, query string, intentEmbedding []float64) ([]RankedSource, error) {
	searchResp, err := app.search.Search(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}
	if len(searchResp.Results) == 0 {
		return nil, fmt.Errorf("search returned no results")
	}

	// Tag and store the raw search results
	for i := range searchResp.Results {
		searchResp.Results[i].QueryVariant = "iterative"
	}
	inserted, err := app.conversations.AppendSearchResults(ctx, conversationID, searchResp.Results, searchResp.EngineStatus)
	if err != nil {
		return nil, fmt.Errorf("storing search results failed: %w", err)
	}
	if len(inserted) == 0 {
		return nil, fmt.Errorf("no new results after deduplication")
	}

	// Limit how many pages we fetch inline to keep latency reasonable
	limit := app.cfg.SummarizeURLLimit
	if limit <= 0 {
		limit = 3
	}
	if len(inserted) > limit {
		inserted = inserted[:limit]
	}

	// Fetch and extract text from the pages
	documents := app.fetch.FetchAndExtract(ctx, meta, inserted, func(url, status, detail string) {
		app.conversations.UpdateSummaryStatus(ctx, conversationID, url, status, detail)
	})

	// Embed each document and store it
	for _, doc := range documents {
		text := strings.TrimSpace(doc.Text)
		if text == "" || len([]rune(text)) < 80 {
			continue
		}

		embedding, err := app.llm.EmbedText(ctx, meta, text)
		if err != nil {
			continue
		}
		embeddingJSON, err := json.Marshal(embedding)
		if err != nil {
			continue
		}

		preview := buildExtractionPreview(text)
		_ = app.conversations.StoreDocument(ctx, conversationID, doc.URL, preview, text, string(embeddingJSON))
	}

	// Determine which embedding to use for ranking
	rankEmbedding := intentEmbedding
	if len(rankEmbedding) == 0 {
		rankEmbedding, err = app.llm.EmbedText(ctx, meta, query)
		if err != nil {
			return nil, fmt.Errorf("query embedding failed: %w", err)
		}
	}

	// Re-rank ALL documents in the conversation by similarity to the query
	ranked, err := app.conversations.RerankAllSources(ctx, app.logger, conversationID, rankEmbedding)
	if err != nil {
		return nil, fmt.Errorf("re-ranking sources failed: %w", err)
	}

	return ranked, nil
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

	contextText, err := app.conversations.BuildSearchContext(r.Context(), meta.UserID, conversationID, app.cfg.ChatContextChars, app.cfg.ContextDocCount)
	if err != nil {
		http.Error(w, "failed to build search context", http.StatusInternalServerError)
		return
	}

	replyMeta := RequestMeta{
		RequestID:      meta.RequestID,
		UserID:         meta.UserID,
		ConversationID: conversationID,
	}

	maxChatSearchLoops := app.maxSearchLoops(r.Context())
	var reply string
	var reasoning string
	for loop := 0; loop < maxChatSearchLoops; loop++ {
		reply, reasoning, err = app.llm.GenerateConversationReply(r.Context(), replyMeta, memorySummary, contextText, history)
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

		newSearchQuery, trigger := app.resolveFollowUpSearchQuery(r.Context(), replyMeta, message, reply)
		if newSearchQuery == "" {
			break
		}

		loggerWithMeta(r.Context(), app.logger, conversationID).Info("follow_up_search_requested",
			"path", "message_post",
			"loop", loop+1,
			"trigger", trigger,
			"query", newSearchQuery,
		)

		intentEmb := app.generateIntentEmbedding(r.Context(), replyMeta, newSearchQuery, history)
		_, processErr := app.inlineSearchAndProcess(r.Context(), replyMeta, conversationID, newSearchQuery, intentEmb)
		if processErr != nil {
			loggerWithMeta(r.Context(), app.logger, conversationID).Error("iterative search processing failed", "trigger", trigger, "query", newSearchQuery, "error", processErr)
			break
		}

		contextText, err = app.conversations.BuildSearchContext(r.Context(), meta.UserID, conversationID, app.cfg.ChatContextChars, app.cfg.ContextDocCount)
		if err != nil {
			break
		}
	}

	if detectNeedMoreSearch(reply) != "" || app.shouldAutoSearchReply(reply, message) {
		loggerWithMeta(r.Context(), app.logger, conversationID).Info("follow_up_search_limit_reached",
			"path", "message_post",
			"max_search_loops", maxChatSearchLoops,
		)
		cleaned := stripNeedMoreSearch(reply)
		if cleaned != "" {
			reply = cleaned
		} else {
			reply = "I still need more source material to answer reliably, but the search loop limit was reached. Increase max search loops or narrow the request."
		}
	}

	if err := app.conversations.AddMessageWithReasoning(context.Background(), conversationID, "assistant", reply, reasoning); err != nil {
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
	return service.AddMessageWithReasoning(ctx, conversationID, role, content, "")
}

func (service *ConversationService) AddSearchQueryMessage(ctx context.Context, conversationID int64, query string) error {
	return service.AddMessage(ctx, conversationID, "system", "search_query:"+strings.TrimSpace(query))
}

func (service *ConversationService) AddMessageWithReasoning(ctx context.Context, conversationID int64, role, content, reasoning string) error {
	if _, err := service.db.ExecContext(ctx, `
        INSERT INTO messages (conversation_id, role, content, reasoning) VALUES (?, ?, ?, ?)
    `, conversationID, role, content, reasoning); err != nil {
		return err
	}
	_, err := service.db.ExecContext(ctx, `
        UPDATE conversations SET updated_at = CURRENT_TIMESTAMP WHERE id = ?
    `, conversationID)
	return err
}

// TruncateConversationForRegeneration deletes the assistant message with assistantMessageID and all
// subsequent messages in the conversation, then returns the preceding user message content to
// regenerate from.
func (service *ConversationService) TruncateConversationForRegeneration(ctx context.Context, userID string, conversationID, assistantMessageID int64) (string, error) {
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// Ensure the conversation belongs to the user.
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM conversations WHERE id = ? AND user_id = ?`, conversationID, userID).Scan(&exists); err != nil {
		return "", err
	}

	// Ensure target message exists and is an assistant message.
	if err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM messages WHERE conversation_id = ? AND id = ? AND role = 'assistant'
	`, conversationID, assistantMessageID).Scan(&exists); err != nil {
		return "", err
	}

	// Find the preceding user message to use as the prompt.
	var prompt string
	if err := tx.QueryRowContext(ctx, `
		SELECT content
		FROM messages
		WHERE conversation_id = ? AND id < ? AND role = 'user'
		ORDER BY id DESC
		LIMIT 1
	`, conversationID, assistantMessageID).Scan(&prompt); err != nil {
		return "", err
	}

	// Delete the assistant message and everything after it.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM messages
		WHERE conversation_id = ? AND id >= ?
	`, conversationID, assistantMessageID); err != nil {
		return "", err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE conversations SET updated_at = CURRENT_TIMESTAMP WHERE id = ?
	`, conversationID); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}

	return strings.TrimSpace(prompt), nil
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

func (service *ConversationService) DeleteConversation(ctx context.Context, userID string, conversationID int64) error {
	result, err := service.db.ExecContext(ctx, `
		DELETE FROM conversations
		WHERE id = ? AND user_id = ?
	`, conversationID, userID)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return sql.ErrNoRows
	}
	return nil
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
			INSERT INTO search_results (conversation_id, url, title, snippet, query_variant, rank)
			VALUES (?, ?, ?, ?, ?, ?)
		`, conversationID, result.URL, result.Title, result.Snippet, result.QueryVariant, result.Rank); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO summaries (conversation_id, url, summary, source_text, embedding_json, similarity_score, rerank_position, status, status_detail)
			VALUES (?, ?, '', '', '', 0, 0, 'pending', '')
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

func (service *ConversationService) AppendSearchResults(ctx context.Context, conversationID int64, results []SearchResult, engineStatus []SearchEngineStatus) ([]SearchResult, error) {
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	existing := map[string]struct{}{}
	rows, err := tx.QueryContext(ctx, `SELECT url FROM search_results WHERE conversation_id = ?`, conversationID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			rows.Close()
			return nil, err
		}
		existing[url] = struct{}{}
	}
	rows.Close()

	var maxRank int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rank), 0) FROM search_results WHERE conversation_id = ?`, conversationID).Scan(&maxRank); err != nil {
		return nil, err
	}

	inserted := make([]SearchResult, 0, len(results))
	for _, result := range results {
		if strings.TrimSpace(result.URL) == "" {
			continue
		}
		if _, ok := existing[result.URL]; ok {
			continue
		}
		maxRank++
		result.Rank = maxRank
		if strings.TrimSpace(result.QueryVariant) == "" {
			result.QueryVariant = "original"
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO search_results (conversation_id, url, title, snippet, query_variant, rank)
			VALUES (?, ?, ?, ?, ?, ?)
		`, conversationID, result.URL, result.Title, result.Snippet, result.QueryVariant, result.Rank); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO summaries (conversation_id, url, summary, source_text, embedding_json, similarity_score, rerank_position, status, status_detail)
			VALUES (?, ?, '', '', '', 0, 0, 'pending', '')
			ON CONFLICT(conversation_id, url) DO NOTHING
		`, conversationID, result.URL); err != nil {
			return nil, err
		}

		existing[result.URL] = struct{}{}
		inserted = append(inserted, result)
	}

	for _, status := range engineStatus {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO search_engine_statuses (conversation_id, engine, status, detail, result_count)
			VALUES (?, ?, ?, ?, ?)
		`, conversationID, status.Engine, status.Status, status.Detail, status.ResultCount); err != nil {
			return nil, err
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, conversationID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return inserted, nil
}

func (service *ConversationService) StoreRewrittenQuery(ctx context.Context, conversationID int64, rewrittenQuery string) error {
	_, err := service.db.ExecContext(ctx, `
		UPDATE conversations
		SET rewritten_query = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, strings.TrimSpace(rewrittenQuery), conversationID)
	return err
}

func (service *ConversationService) UpdateRewriteStatus(ctx context.Context, conversationID int64, status, detail string) error {
	_, err := service.db.ExecContext(ctx, `
		UPDATE conversations
		SET rewrite_status = ?, rewrite_detail = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, strings.TrimSpace(status), strings.TrimSpace(detail), conversationID)
	return err
}

func (service *ConversationService) GetRewrittenQuery(ctx context.Context, conversationID int64) (string, error) {
	var value string
	err := service.db.QueryRowContext(ctx, `SELECT rewritten_query FROM conversations WHERE id = ?`, conversationID).Scan(&value)
	return value, err
}

func (service *ConversationService) UpdateAnswerStatus(ctx context.Context, conversationID int64, status, detail string) error {
	_, err := service.db.ExecContext(ctx, `
		UPDATE conversations
		SET answer_status = ?, answer_detail = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, strings.TrimSpace(status), strings.TrimSpace(detail), conversationID)
	return err
}

func (service *ConversationService) StoreSummary(ctx context.Context, conversationID int64, url, summary, sourceText string) error {
	_, err := service.db.ExecContext(ctx, `
		INSERT INTO summaries (conversation_id, url, summary, source_text, embedding_json, similarity_score, rerank_position, status, status_detail)
		VALUES (?, ?, ?, ?, '', 0, 0, 'ready', '')
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

func (service *ConversationService) StoreDocument(ctx context.Context, conversationID int64, url, summary, sourceText, embeddingJSON string) error {
	_, err := service.db.ExecContext(ctx, `
		INSERT INTO summaries (conversation_id, url, summary, source_text, embedding_json, similarity_score, rerank_position, status, status_detail)
		VALUES (?, ?, ?, ?, ?, 0, 0, 'ready', '')
		ON CONFLICT(conversation_id, url) DO UPDATE SET
			summary = excluded.summary,
			source_text = excluded.source_text,
			embedding_json = excluded.embedding_json,
			status = 'ready',
			status_detail = '',
			updated_at = CURRENT_TIMESTAMP
	`, conversationID, url, summary, sourceText, embeddingJSON)
	if err != nil {
		return err
	}
	_, err = service.db.ExecContext(ctx, `UPDATE conversations SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, conversationID)
	return err
}

func (service *ConversationService) UpdateSummaryStatus(ctx context.Context, conversationID int64, url, status, detail string) error {
	_, err := service.db.ExecContext(ctx, `
		INSERT INTO summaries (conversation_id, url, summary, source_text, embedding_json, similarity_score, rerank_position, status, status_detail)
		VALUES (?, ?, '', '', '', 0, 0, ?, ?)
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
		INSERT INTO summaries (conversation_id, url, summary, source_text, embedding_json, similarity_score, rerank_position, status, status_detail)
		VALUES (?, ?, '', ?, '', 0, 0, 'pending', '')
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

func (service *ConversationService) UpdateDocumentRanking(ctx context.Context, conversationID int64, url string, similarity float64, position int) error {
	_, err := service.db.ExecContext(ctx, `
		UPDATE summaries
		SET similarity_score = ?, rerank_position = ?, updated_at = CURRENT_TIMESTAMP
		WHERE conversation_id = ? AND url = ?
	`, similarity, position, conversationID, url)
	return err
}

func (service *ConversationService) GetConversationView(ctx context.Context, userID string, conversationID int64) (ConversationView, error) {
	conversation := ConversationView{}
	row := service.db.QueryRowContext(ctx, `
		SELECT id, user_id, title, rewritten_query, rewrite_status, rewrite_detail, answer_status, answer_detail, created_at, updated_at
        FROM conversations
        WHERE id = ? AND user_id = ?
    `, conversationID, userID)
	if err := row.Scan(&conversation.ID, &conversation.UserID, &conversation.Title, &conversation.RewrittenQuery, &conversation.RewriteStatus, &conversation.RewriteDetail, &conversation.AnswerStatus, &conversation.AnswerDetail, &conversation.CreatedAt, &conversation.UpdatedAt); err != nil {
		return ConversationView{}, err
	}

	messages, err := service.getMessages(ctx, conversationID)
	if err != nil {
		return ConversationView{}, err
	}
	// Mark the very first user message (initial search query) so the template
	// can skip it — it's already shown as the conversation title.
	for i := range messages {
		if messages[i].Role == "user" {
			messages[i].SkipInDisplay = true
			break
		}
	}
	conversation.Messages = messages

	results, err := service.GetSearchResults(ctx, conversationID)
	if err != nil {
		return ConversationView{}, err
	}
	conversation.SearchResults = results
	for _, result := range results {
		if result.QueryVariant == "rewritten" {
			conversation.RewrittenResultCount++
		} else {
			conversation.OriginalResultCount++
		}
	}

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

func (service *ConversationService) ResetSummaries(ctx context.Context, conversationID int64) error {
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE summaries
		SET summary = '', source_text = '', embedding_json = '', similarity_score = 0, rerank_position = 0, status = 'pending', status_detail = '', updated_at = CURRENT_TIMESTAMP
		WHERE conversation_id = ?
	`, conversationID); err != nil {
		return err
	}

	// Delete the overview message (first assistant message)
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM messages
		WHERE id = (
			SELECT id FROM messages
			WHERE conversation_id = ? AND role = 'assistant'
			ORDER BY timestamp ASC, id ASC
			LIMIT 1
		)
	`, conversationID); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE conversations
		SET rewritten_query = '', rewrite_status = 'pending', rewrite_detail = '', answer_status = 'searching', answer_detail = 'Reprocessing the existing raw results.', updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, conversationID); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE conversations SET updated_at = CURRENT_TIMESTAMP WHERE id = ?
	`, conversationID); err != nil {
		return err
	}

	return tx.Commit()
}

func (service *ConversationService) GetSearchResults(ctx context.Context, conversationID int64) ([]SearchResult, error) {
	rows, err := service.db.QueryContext(ctx, `
		SELECT r.url, r.title, r.snippet, r.query_variant, r.rank
		FROM search_results r
		LEFT JOIN summaries s ON s.conversation_id = r.conversation_id AND s.url = r.url
		WHERE r.conversation_id = ?
		ORDER BY CASE WHEN COALESCE(s.status, '') IN ('error', 'skipped') THEN 2
		              WHEN COALESCE(s.rerank_position, 0) > 0 THEN 0
		              ELSE 1 END ASC,
		         COALESCE(s.rerank_position, 0) ASC,
		         r.rank ASC
    `, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := []SearchResult{}
	for rows.Next() {
		var item SearchResult
		if err := rows.Scan(&item.URL, &item.Title, &item.Snippet, &item.QueryVariant, &item.Rank); err != nil {
			return nil, err
		}
		results = append(results, item)
	}
	return results, rows.Err()
}

func (service *ConversationService) GetSearchEngineStatus(ctx context.Context, conversationID int64) ([]SearchEngineStatus, error) {
	rows, err := service.db.QueryContext(ctx, `
        SELECT engine,
               CASE WHEN SUM(CASE WHEN status = 'error'   THEN 1 ELSE 0 END) > 0 THEN 'error'
                    WHEN SUM(CASE WHEN status = 'timeout' THEN 1 ELSE 0 END) > 0 THEN 'timeout'
                    ELSE 'ok' END AS status,
               '' AS detail,
               SUM(result_count) AS result_count
        FROM search_engine_statuses
        WHERE conversation_id = ?
        GROUP BY engine
        ORDER BY
            CASE WHEN SUM(CASE WHEN status = 'error'   THEN 1 ELSE 0 END) > 0 THEN 0
                 WHEN SUM(CASE WHEN status = 'timeout' THEN 1 ELSE 0 END) > 0 THEN 1
                 ELSE 2 END,
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
	SELECT url, summary, source_text, status, status_detail, similarity_score, rerank_position
        FROM summaries
        WHERE conversation_id = ?
	ORDER BY CASE WHEN rerank_position > 0 THEN 0 ELSE 1 END, rerank_position ASC, created_at ASC, id ASC
    `, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	summaries := []SummaryRecord{}
	for rows.Next() {
		var item SummaryRecord
		if err := rows.Scan(&item.URL, &item.Summary, &item.SourceText, &item.Status, &item.Detail, &item.SimilarityScore, &item.RerankPosition); err != nil {
			return nil, err
		}
		summaries = append(summaries, item)
	}
	return summaries, rows.Err()
}

func (service *ConversationService) ListRankedSources(ctx context.Context, conversationID int64) ([]RankedSource, error) {
	rows, err := service.db.QueryContext(ctx, `
		SELECT s.url, r.title, r.snippet, r.query_variant, s.summary, s.source_text, s.embedding_json, s.similarity_score, s.rerank_position
		FROM summaries s
		JOIN search_results r ON r.conversation_id = s.conversation_id AND r.url = s.url
		WHERE s.conversation_id = ?
	`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sources := []RankedSource{}
	for rows.Next() {
		var item RankedSource
		if err := rows.Scan(&item.URL, &item.Title, &item.Snippet, &item.QueryVariant, &item.Summary, &item.SourceText, &item.EmbeddingJSON, &item.SimilarityScore, &item.RerankPosition); err != nil {
			return nil, err
		}
		sources = append(sources, item)
	}
	return sources, rows.Err()
}

func (service *ConversationService) ListTopRankedSources(ctx context.Context, conversationID int64, limit int) ([]RankedSource, error) {
	if limit <= 0 {
		limit = 3
	}

	rows, err := service.db.QueryContext(ctx, `
		SELECT s.url, r.title, r.snippet, r.query_variant, s.summary, s.source_text, s.embedding_json, s.similarity_score, s.rerank_position
		FROM summaries s
		JOIN search_results r ON r.conversation_id = s.conversation_id AND r.url = s.url
		WHERE s.conversation_id = ? AND s.rerank_position > 0 AND s.status = 'ready' AND s.source_text != ''
		ORDER BY s.rerank_position ASC
		LIMIT ?
	`, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sources := []RankedSource{}
	for rows.Next() {
		var item RankedSource
		if err := rows.Scan(&item.URL, &item.Title, &item.Snippet, &item.QueryVariant, &item.Summary, &item.SourceText, &item.EmbeddingJSON, &item.SimilarityScore, &item.RerankPosition); err != nil {
			return nil, err
		}
		sources = append(sources, item)
	}
	return sources, rows.Err()
}

// RerankAllSources recomputes similarity scores for every stored document in a
// conversation against the given query embedding, sorts by similarity, and
// updates rerank_position in the database. This is the single source of truth
// for ranking — called by both the initial pipeline and inline search handlers.
func (service *ConversationService) RerankAllSources(ctx context.Context, logger *slog.Logger, conversationID int64, queryEmbedding []float64) ([]RankedSource, error) {
	sources, err := service.ListRankedSources(ctx, conversationID)
	if err != nil {
		return nil, err
	}

	ranked := make([]RankedSource, 0, len(sources))
	for _, source := range sources {
		if strings.TrimSpace(source.EmbeddingJSON) == "" || strings.TrimSpace(source.SourceText) == "" {
			continue
		}

		var embedding []float64
		if err := json.Unmarshal([]byte(source.EmbeddingJSON), &embedding); err != nil {
			logger.Error("embedding deserialization failed", "url", source.URL, "error", err)
			continue
		}

		source.SimilarityScore = cosineSimilarity(queryEmbedding, embedding)
		ranked = append(ranked, source)
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].SimilarityScore == ranked[j].SimilarityScore {
			return ranked[i].URL < ranked[j].URL
		}
		return ranked[i].SimilarityScore > ranked[j].SimilarityScore
	})

	for index := range ranked {
		position := index + 1
		ranked[index].RerankPosition = position
		if err := service.UpdateDocumentRanking(ctx, conversationID, ranked[index].URL, ranked[index].SimilarityScore, position); err != nil {
			return nil, err
		}
	}

	return ranked, nil
}

func (service *ConversationService) getMessages(ctx context.Context, conversationID int64) ([]MessageRecord, error) {
	rows, err := service.db.QueryContext(ctx, `
        SELECT id, role, content, reasoning, timestamp
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
		if err := rows.Scan(&item.ID, &item.Role, &item.Content, &item.Reasoning, &item.Timestamp); err != nil {
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
		if message.Role != "user" && message.Role != "assistant" {
			continue
		}
		history = append(history, LLMMessage{Role: message.Role, Content: message.Content})
	}
	return history, nil
}

func (service *ConversationService) BuildSearchContext(ctx context.Context, userID string, conversationID int64, maxChars int, maxDocs int) (string, error) {
	conversation, err := service.GetConversationView(ctx, userID, conversationID)
	if err != nil {
		return "", err
	}

	if maxChars <= 0 {
		maxChars = 12000
	}
	if maxDocs <= 0 {
		maxDocs = 5
	}

	builder := strings.Builder{}
	docIndex := 0
	for _, summary := range conversation.Summaries {
		if summary.Status != "ready" {
			continue
		}
		if docIndex >= maxDocs {
			break
		}
		docIndex++

		excerpt := compactContextText(summary.SourceText, 2500)
		block := fmt.Sprintf("[%d] URL: %s\nSimilarity: %.4f\n", summary.RerankPosition, summary.URL, summary.SimilarityScore)
		if excerpt != "" {
			block += fmt.Sprintf("Extracted text excerpt:\n%s\n\n", excerpt)
		}
		block += fmt.Sprintf("Summary:\n%s\n", compactContextText(summary.Summary, 1200))
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

func (service *ConversationService) WaitForAnswerReady(ctx context.Context, userID string, conversationID int64, timeout time.Duration, onProgress func(status, detail string)) (ConversationView, error) {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	deadline := time.Now().Add(timeout)
	lastStatus := ""
	lastDetail := ""

	for {
		conversation, err := service.GetConversationView(ctx, userID, conversationID)
		if err != nil {
			return ConversationView{}, err
		}
		if strings.TrimSpace(conversation.OverviewSummary) != "" || conversation.AnswerStatus == "ready" || conversation.AnswerStatus == "complete" {
			return conversation, nil
		}
		if conversation.AnswerStatus == "error" {
			if strings.TrimSpace(conversation.AnswerDetail) == "" {
				return ConversationView{}, fmt.Errorf("the answer pipeline failed")
			}
			return ConversationView{}, fmt.Errorf("%s", conversation.AnswerDetail)
		}

		if onProgress != nil && (conversation.AnswerStatus != lastStatus || conversation.AnswerDetail != lastDetail) {
			onProgress(conversation.AnswerStatus, conversation.AnswerDetail)
			lastStatus = conversation.AnswerStatus
			lastDetail = conversation.AnswerDetail
		}

		if time.Now().After(deadline) {
			return ConversationView{}, fmt.Errorf("the answer pipeline is still preparing sources")
		}

		select {
		case <-ctx.Done():
			return ConversationView{}, ctx.Err()
		case <-time.After(750 * time.Millisecond):
		}
	}
}

func (service *ConversationService) GetSetting(ctx context.Context, key, fallback string) string {
	var value string
	err := service.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err != nil {
		return fallback
	}
	return value
}

func (service *ConversationService) SetSetting(ctx context.Context, key, value string) error {
	_, err := service.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	return err
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
		if summary.Status == "ready" && strings.TrimSpace(summary.SourceText) != "" {
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
