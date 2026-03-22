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

const (
	modelRoleAnswer     = "answer"
	modelRoleRewrite    = "rewrite"
	modelRoleEmbeddings = "embeddings"
)

func (app *App) handleModelDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	modelURL := strings.TrimSpace(r.FormValue("url"))
	if modelURL == "" {
		http.Redirect(w, r, "/settings?status=missing+url", http.StatusSeeOther)
		return
	}

	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, modelURL, nil)
	if err != nil {
		http.Redirect(w, r, "/settings?status=download+failed", http.StatusSeeOther)
		return
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		http.Redirect(w, r, "/settings?status=download+failed", http.StatusSeeOther)
		return
	}
	defer response.Body.Close()

	if response.StatusCode >= http.StatusBadRequest {
		http.Redirect(w, r, "/settings?status=download+failed", http.StatusSeeOther)
		return
	}

	filename := filepath.Base(response.Request.URL.Path)
	if !strings.HasSuffix(strings.ToLower(filename), ".gguf") {
		http.Redirect(w, r, "/settings?status=only+.gguf+files+are+accepted", http.StatusSeeOther)
		return
	}

	destination := filepath.Join(app.cfg.ModelsDir, filename)
	tempDestination := destination + ".part"

	file, err := os.Create(tempDestination)
	if err != nil {
		http.Redirect(w, r, "/settings?status=failed+to+write+model", http.StatusSeeOther)
		return
	}

	if _, err := io.Copy(file, response.Body); err != nil {
		file.Close()
		os.Remove(tempDestination)
		http.Redirect(w, r, "/settings?status=download+failed", http.StatusSeeOther)
		return
	}
	file.Close()

	if err := os.Rename(tempDestination, destination); err != nil {
		os.Remove(tempDestination)
		http.Redirect(w, r, "/settings?status=failed+to+publish+model", http.StatusSeeOther)
		return
	}

	loggerWithMeta(r.Context(), app.logger, 0).Info("model_downloaded", "url", modelURL, "filename", filename)
	http.Redirect(w, r, "/settings?status=model+downloaded", http.StatusSeeOther)
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
	return app.currentModelNameForRole(modelRoleAnswer)
}

func (app *App) currentModelNameForRole(role string) string {
	payload, err := os.ReadFile(app.modelPathForRole(role))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(payload))
}

func (app *App) modelPathForRole(role string) string {
	switch normalizeModelRole(role) {
	case modelRoleRewrite:
		return filepath.Join(app.cfg.ModelsDir, "current-rewrite-model.txt")
	case modelRoleEmbeddings:
		return filepath.Join(app.cfg.ModelsDir, "current-embedding-model.txt")
	default:
		return app.cfg.CurrentModelPath
	}
}

func normalizeModelRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case modelRoleRewrite:
		return modelRoleRewrite
	case modelRoleEmbeddings:
		return modelRoleEmbeddings
	default:
		return modelRoleAnswer
	}
}
