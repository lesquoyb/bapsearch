package main

import (
	"net/http"
	"os"
	"strings"
)

// StarterPreset bundles a recommended set of parameters for a given hardware
// tier. Applying a preset writes these into the settings table; model
// selection is left to the user (their list depends on what they have
// downloaded), but the description points at sensible defaults.
type StarterPreset struct {
	ID          string
	Title       string
	Description string
	ModelHint   string
	Values      map[string]string
}

var starterPresets = []StarterPreset{
	{
		ID:          "cpu",
		Title:       "CPU only (no GPU / integrated)",
		Description: "Smallest models, short context, no reasoning, low concurrency. The answer model runs entirely on CPU.",
		ModelHint:   "Suggested: Qwen2.5-1.5B-Instruct Q4_K_M for the answer model, nomic-embed-text-v1.5 (or bge-small) for embeddings.",
		Values: map[string]string{
			"temperature": "0.2", "top_p": "1.0", "top_k": "40",
			"max_tokens":           "512",
			"enable_thinking":      "false",
			"max_embedding_tokens": "256",
			"embedding_batch_size": "1",
			"summarize_url_limit":  "2",
			"fetch_workers":        "2",
		},
	},
	{
		ID:          "low_gpu",
		Title:       "Entry GPU (≈4 GB VRAM)",
		Description: "Small models with a usable context. Reasoning off to keep latency down. One-doc-at-a-time embeddings.",
		ModelHint:   "Suggested: Qwen2.5-3B-Instruct Q4_K_M, bge-small for embeddings (under 100 MB VRAM).",
		Values: map[string]string{
			"temperature": "0.2", "top_p": "1.0", "top_k": "40",
			"max_tokens":           "1024",
			"enable_thinking":      "false",
			"max_embedding_tokens": "500",
			"embedding_batch_size": "2",
			"summarize_url_limit":  "3",
			"fetch_workers":        "3",
		},
	},
	{
		ID:          "consumer_gpu",
		Title:       "Consumer GPU (8–24 GB VRAM)",
		Description: "Mid-size answer model with reasoning enabled, batched embeddings, more sources per query.",
		ModelHint:   "Suggested: Qwen3-7B-Instruct or Qwen2.5-7B-Instruct Q4_K_M, bge-large or e5-large for embeddings.",
		Values: map[string]string{
			"temperature": "0.2", "top_p": "1.0", "top_k": "40",
			"max_tokens":           "2048",
			"enable_thinking":      "true",
			"reasoning_budget":     "2048",
			"max_embedding_tokens": "1024",
			"embedding_batch_size": "4",
			"summarize_url_limit":  "5",
			"fetch_workers":        "4",
		},
	},
	{
		ID:          "server",
		Title:       "Workstation / server (48 GB+ VRAM)",
		Description: "Large models, deep reasoning budget, aggressive batching, lots of sources.",
		ModelHint:   "Suggested: Qwen2.5-32B-Instruct or DeepSeek-R1-Distill-Qwen-32B, bge-m3 or jina-embeddings-v3 for embeddings.",
		Values: map[string]string{
			"temperature": "0.2", "top_p": "1.0", "top_k": "40",
			"max_tokens":           "4096",
			"enable_thinking":      "true",
			"reasoning_budget":     "4096",
			"max_embedding_tokens": "2048",
			"embedding_batch_size": "8",
			"summarize_url_limit":  "8",
			"fetch_workers":        "8",
		},
	},
}

// handleSettingsPreset applies one of the starter presets and redirects back
// to /settings?status=preset-<id>.
func (app *App) handleSettingsPreset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSpace(r.FormValue("preset"))
	var preset *StarterPreset
	for i := range starterPresets {
		if starterPresets[i].ID == name {
			preset = &starterPresets[i]
			break
		}
	}
	if preset == nil {
		http.Error(w, "unknown preset", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	for k, v := range preset.Values {
		_ = app.conversations.SetSetting(ctx, k, v)
	}
	app.loadSettingsFromDB()
	http.Redirect(w, r, "/settings?status=preset+"+preset.ID+"+applied", http.StatusSeeOther)
}

// handleSettingsPage serves GET /settings. POST is delegated to handleSettingsSave.
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
	c, m := app.llm.Prompts.GetAll()
	if strings.TrimSpace(c) == "" {
		c = DefaultPromptChat
	}
	if strings.TrimSpace(m) == "" {
		m = DefaultPromptMemory
	}

	settings := map[string]string{
		"llm_model":            app.currentModelName(),
		"embedding_model":      app.currentModelNameForRole("embeddings"),
		"temperature":          app.conversations.GetSetting(ctx, "temperature", "0.2"),
		"top_p":                app.conversations.GetSetting(ctx, "top_p", "1.0"),
		"top_k":                app.conversations.GetSetting(ctx, "top_k", "40"),
		"max_tokens":           app.conversations.GetSetting(ctx, "max_tokens", "1024"),
		"enable_thinking":      app.conversations.GetSetting(ctx, "enable_thinking", "true"),
		"reasoning_budget":     app.conversations.GetSetting(ctx, "reasoning_budget", "2048"),
		"max_embedding_tokens": app.conversations.GetSetting(ctx, "max_embedding_tokens", "500"),
		"embedding_batch_size": app.conversations.GetSetting(ctx, "embedding_batch_size", "1"),
		"query_reformulations": app.conversations.GetSetting(ctx, "query_reformulations", ""),
		"rewrite_temperature":  app.conversations.GetSetting(ctx, "rewrite_temperature", ""),
		"rewrite_top_p":        app.conversations.GetSetting(ctx, "rewrite_top_p", ""),
		"rewrite_top_k":        app.conversations.GetSetting(ctx, "rewrite_top_k", ""),
		"utility_temperature":  app.conversations.GetSetting(ctx, "utility_temperature", ""),
		"summarize_url_limit":  app.conversations.GetSetting(ctx, "summarize_url_limit", "3"),
		"max_extract_chars":    app.conversations.GetSetting(ctx, "max_extract_chars", "12000"),
		"fetch_workers":        app.conversations.GetSetting(ctx, "fetch_workers", "3"),
		"chat_context_chars":   app.conversations.GetSetting(ctx, "chat_context_chars", "4200"),
		"max_chat_messages":    app.conversations.GetSetting(ctx, "max_chat_messages", "8"),
		"max_search_loops":       app.conversations.GetSetting(ctx, "max_search_loops", "3"),
		"context_doc_count":      app.conversations.GetSetting(ctx, "context_doc_count", "5"),
		"results_display_limit":  app.conversations.GetSetting(ctx, "results_display_limit", "10"),
	}

	app.render(w, "settings", PageData{
		AppName:        "bap-search",
		UserID:         meta.UserID,
		Conversations:  conversations,
		Models:         models,
		CurrentModel:   settings["llm_model"],
		EmbeddingModel: settings["embedding_model"],
		Status:         r.URL.Query().Get("status"),
		Settings:       settings,
		StarterPresets: starterPresets,
		Prompts: map[string]string{
			"prompt_chat":   c,
			"prompt_memory": m,
		},
	})
}

// handleSettingsSave processes the settings form submission.
func (app *App) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Model role assignments (written to files)
	if model := strings.TrimSpace(r.FormValue("llm_model")); model != "" {
		_ = os.WriteFile(app.modelPathForRole("answer"), []byte(model), 0o644)
	}
	if model := strings.TrimSpace(r.FormValue("embedding_model")); model != "" {
		_ = os.WriteFile(app.modelPathForRole("embeddings"), []byte(model), 0o644)
	}

	// All DB-backed settings
	dbKeys := []string{
		"temperature", "top_p", "top_k", "max_tokens",
		"enable_thinking", "reasoning_budget",
		"max_embedding_tokens", "embedding_batch_size",
		"query_reformulations",
		"rewrite_temperature", "rewrite_top_p", "rewrite_top_k",
		"utility_temperature",
		"summarize_url_limit", "max_extract_chars", "fetch_workers",
		"chat_context_chars", "max_chat_messages", "max_search_loops",
		"context_doc_count", "results_display_limit",
		"prompt_chat", "prompt_memory",
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
