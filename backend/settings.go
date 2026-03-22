package main

import (
	"net/http"
	"os"
	"strings"
)

// handleSettingsPage serves GET /settings and POST /settings (save).
func (app *App) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		app.handleSettingsSave(w, r)
		return
	}

	ctx := r.Context()
	meta := requestMetaFromContext(ctx)
	conversations, _ := app.conversations.ListConversations(ctx, meta.UserID)
	models, _ := app.listModels()

	// Prompts
	s, sy, c, m := app.llm.Prompts.GetAll()
	if strings.TrimSpace(s) == "" {
		s = DefaultPromptSummarize
	}
	if strings.TrimSpace(sy) == "" {
		sy = DefaultPromptSynthesize
	}
	if strings.TrimSpace(c) == "" {
		c = DefaultPromptChat
	}
	if strings.TrimSpace(m) == "" {
		m = DefaultPromptMemory
	}

	settings := map[string]string{
		"llm_model":            app.currentModelName(),
		"rewrite_model":        app.currentModelNameForRole("rewrite"),
		"embedding_model":      app.currentModelNameForRole("embeddings"),
		"temperature":          app.conversations.GetSetting(ctx, "temperature", "0.2"),
		"top_p":                app.conversations.GetSetting(ctx, "top_p", "1.0"),
		"top_k":                app.conversations.GetSetting(ctx, "top_k", "40"),
		"max_tokens":           app.conversations.GetSetting(ctx, "max_tokens", "1024"),
		"enable_thinking":      app.conversations.GetSetting(ctx, "enable_thinking", "true"),
		"reasoning_budget":     app.conversations.GetSetting(ctx, "reasoning_budget", "2048"),
		"search_engine":        app.conversations.GetSetting(ctx, "search_engine", "searxng"),
		"search_count":         app.conversations.GetSetting(ctx, "search_count", "8"),
		"similarity_threshold": app.conversations.GetSetting(ctx, "similarity_threshold", "0.5"),
		"summarize_url_limit":  app.conversations.GetSetting(ctx, "summarize_url_limit", "3"),
		"max_extract_chars":    app.conversations.GetSetting(ctx, "max_extract_chars", "12000"),
		"fetch_workers":        app.conversations.GetSetting(ctx, "fetch_workers", "3"),
		"chat_context_chars":   app.conversations.GetSetting(ctx, "chat_context_chars", "4200"),
		"max_chat_messages":    app.conversations.GetSetting(ctx, "max_chat_messages", "8"),
		"max_search_loops":     app.conversations.GetSetting(ctx, "max_search_loops", "3"),
		"context_doc_count":    app.conversations.GetSetting(ctx, "context_doc_count", "5"),
	}

	app.render(w, "settings", PageData{
		AppName:        "bap-search",
		UserID:         meta.UserID,
		Conversations:  conversations,
		Models:         models,
		CurrentModel:   settings["llm_model"],
		RewriteModel:   settings["rewrite_model"],
		EmbeddingModel: settings["embedding_model"],
		Status:         r.URL.Query().Get("status"),
		Settings:       settings,
		Prompts: map[string]string{
			"prompt_summarize":  s,
			"prompt_synthesize": sy,
			"prompt_chat":       c,
			"prompt_memory":     m,
		},
	})
}

// handleSettingsSave processes POST /settings.
func (app *App) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Model role assignments (written to files)
	if model := strings.TrimSpace(r.FormValue("llm_model")); model != "" {
		_ = os.WriteFile(app.modelPathForRole("answer"), []byte(model), 0o644)
	}
	if model := strings.TrimSpace(r.FormValue("rewrite_model")); model != "" {
		_ = os.WriteFile(app.modelPathForRole("rewrite"), []byte(model), 0o644)
	}
	if model := strings.TrimSpace(r.FormValue("embedding_model")); model != "" {
		_ = os.WriteFile(app.modelPathForRole("embeddings"), []byte(model), 0o644)
	}

	// All DB-backed settings
	dbKeys := []string{
		"temperature", "top_p", "top_k", "max_tokens",
		"enable_thinking", "reasoning_budget",
		"search_engine", "search_count",
		"similarity_threshold",
		"summarize_url_limit", "max_extract_chars", "fetch_workers",
		"chat_context_chars", "max_chat_messages", "max_search_loops",
		"context_doc_count",
		"prompt_summarize", "prompt_synthesize", "prompt_chat", "prompt_memory",
	}
	for _, key := range dbKeys {
		val := strings.TrimSpace(r.FormValue(key))
		if val != "" {
			_ = app.conversations.SetSetting(ctx, key, val)
		}
	}

	// Checkbox: if not submitted, explicitly set to "false"
	if r.FormValue("enable_thinking") == "" {
		_ = app.conversations.SetSetting(ctx, "enable_thinking", "false")
	}

	app.loadPromptsFromDB()
	app.loadSettingsFromDB()

	http.Redirect(w, r, "/settings?status=saved", http.StatusSeeOther)
}
