package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ModelInfo struct {
	Name     string
	Path     string
	Selected bool
}

func (app *App) handleModelsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	meta := requestMetaFromContext(r.Context())
	conversations, err := app.conversations.ListConversations(r.Context(), meta.UserID)
	if err != nil {
		http.Error(w, "failed to load conversations", http.StatusInternalServerError)
		return
	}

	models, err := app.listModels()
	if err != nil {
		http.Error(w, "failed to list models", http.StatusInternalServerError)
		return
	}

	app.render(w, "models", PageData{
		AppName:       "bap-search",
		UserID:        meta.UserID,
		Conversations: conversations,
		Models:        models,
		CurrentModel:  app.currentModelName(),
		Status:        r.URL.Query().Get("status"),
	})
}

func (app *App) handleModelSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		http.Redirect(w, r, "/models?status=missing+model", http.StatusSeeOther)
		return
	}

	candidate := filepath.Join(app.cfg.ModelsDir, model)
	if _, err := os.Stat(candidate); err != nil {
		http.Redirect(w, r, "/models?status=unknown+model", http.StatusSeeOther)
		return
	}

	if err := os.WriteFile(app.cfg.CurrentModelPath, []byte(model), 0o644); err != nil {
		http.Redirect(w, r, "/models?status=failed+to+select+model", http.StatusSeeOther)
		return
	}

	loggerWithMeta(r.Context(), app.logger, 0).Info("model_selected", "model", model)
	http.Redirect(w, r, "/models?status=model+selection+saved", http.StatusSeeOther)
}

func (app *App) handleModelDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	modelURL := strings.TrimSpace(r.FormValue("url"))
	if modelURL == "" {
		http.Redirect(w, r, "/models?status=missing+url", http.StatusSeeOther)
		return
	}

	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, modelURL, nil)
	if err != nil {
		http.Redirect(w, r, "/models?status=download+failed", http.StatusSeeOther)
		return
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		http.Redirect(w, r, "/models?status=download+failed", http.StatusSeeOther)
		return
	}
	defer response.Body.Close()

	if response.StatusCode >= http.StatusBadRequest {
		http.Redirect(w, r, "/models?status=download+failed", http.StatusSeeOther)
		return
	}

	filename := filepath.Base(response.Request.URL.Path)
	if !strings.HasSuffix(strings.ToLower(filename), ".gguf") {
		http.Redirect(w, r, "/models?status=only+.gguf+files+are+accepted", http.StatusSeeOther)
		return
	}

	destination := filepath.Join(app.cfg.ModelsDir, filename)
	tempDestination := destination + ".part"

	file, err := os.Create(tempDestination)
	if err != nil {
		http.Redirect(w, r, "/models?status=failed+to+write+model", http.StatusSeeOther)
		return
	}

	if _, err := io.Copy(file, response.Body); err != nil {
		file.Close()
		os.Remove(tempDestination)
		http.Redirect(w, r, "/models?status=download+failed", http.StatusSeeOther)
		return
	}
	file.Close()

	if err := os.Rename(tempDestination, destination); err != nil {
		os.Remove(tempDestination)
		http.Redirect(w, r, "/models?status=failed+to+publish+model", http.StatusSeeOther)
		return
	}

	loggerWithMeta(r.Context(), app.logger, 0).Info("model_downloaded", "url", modelURL, "filename", filename)
	http.Redirect(w, r, "/models?status=model+downloaded", http.StatusSeeOther)
}

func (app *App) listModels() ([]ModelInfo, error) {
	if err := os.MkdirAll(app.cfg.ModelsDir, 0o755); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(app.cfg.ModelsDir)
	if err != nil {
		return nil, err
	}

	current := app.currentModelName()
	models := []ModelInfo{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(entry.Name()), ".gguf") {
			continue
		}
		models = append(models, ModelInfo{
			Name:     entry.Name(),
			Path:     filepath.Join(app.cfg.ModelsDir, entry.Name()),
			Selected: entry.Name() == current,
		})
	}

	sort.Slice(models, func(left, right int) bool {
		return models[left].Name < models[right].Name
	})

	return models, nil
}

func (app *App) currentModelName() string {
	payload, err := os.ReadFile(app.cfg.CurrentModelPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(payload))
}
