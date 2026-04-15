package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ModelInfo struct {
	Name     string
	Path     string
	Selected bool
}

const (
	modelRoleAnswer     = "answer"
	modelRoleEmbeddings = "embeddings"
)

type ModelDownloadStatus struct {
	State           string    `json:"state"`
	URL             string    `json:"url"`
	Filename        string    `json:"filename"`
	BytesDownloaded int64     `json:"bytes_downloaded"`
	TotalBytes      int64     `json:"total_bytes"`
	Error           string    `json:"error"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (app *App) setModelDownloadStatus(update func(*ModelDownloadStatus)) {
	app.modelDownloadMu.Lock()
	defer app.modelDownloadMu.Unlock()
	update(&app.modelDownload)
	app.modelDownload.UpdatedAt = time.Now()
}

func (app *App) getModelDownloadStatus() ModelDownloadStatus {
	app.modelDownloadMu.Lock()
	defer app.modelDownloadMu.Unlock()
	return app.modelDownload
}

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

	current := app.getModelDownloadStatus()
	if current.State == "downloading" {
		http.Redirect(w, r, "/settings?status=download+already+in+progress", http.StatusSeeOther)
		return
	}

	if err := os.MkdirAll(app.cfg.ModelsDir, 0o755); err != nil {
		http.Redirect(w, r, "/settings?status=failed+to+prepare+models+folder", http.StatusSeeOther)
		return
	}

	filename, err := inferModelFilename(modelURL)
	if err != nil {
		http.Redirect(w, r, "/settings?status=invalid+url", http.StatusSeeOther)
		return
	}
	if !strings.HasSuffix(strings.ToLower(filename), ".gguf") {
		http.Redirect(w, r, "/settings?status=only+.gguf+files+are+accepted", http.StatusSeeOther)
		return
	}

	destination := filepath.Join(app.cfg.ModelsDir, filename)
	tempDestination := destination + ".part"
	if _, err := os.Stat(destination); err == nil {
		app.setModelDownloadStatus(func(s *ModelDownloadStatus) {
			s.State = "done"
			s.URL = modelURL
			s.Filename = filename
			s.Error = ""
			s.TotalBytes = 0
			s.BytesDownloaded = 0
		})
		http.Redirect(w, r, "/settings?status=model+already+downloaded", http.StatusSeeOther)
		return
	}

	startingBytes, _ := fileSize(tempDestination)

	app.setModelDownloadStatus(func(s *ModelDownloadStatus) {
		s.State = "downloading"
		s.URL = modelURL
		s.Filename = filename
		s.BytesDownloaded = startingBytes
		s.TotalBytes = 0
		s.Error = ""
	})

	go func() {
		ctx := context.Background()
		err := downloadFileWithResume(ctx, http.DefaultClient, modelURL, tempDestination, destination, func(bytesDownloaded, totalBytes int64, statusLine string) {
			app.setModelDownloadStatus(func(s *ModelDownloadStatus) {
				s.BytesDownloaded = bytesDownloaded
				if totalBytes > 0 {
					s.TotalBytes = totalBytes
				}
				s.Error = statusLine
			})
		})
		if err != nil {
			app.setModelDownloadStatus(func(s *ModelDownloadStatus) {
				s.State = "error"
				s.Error = err.Error()
			})
			loggerWithMeta(context.Background(), app.logger, 0).Error("model_download_failed", "url", modelURL, "filename", filename, "error", err)
			return
		}

		app.setModelDownloadStatus(func(s *ModelDownloadStatus) {
			// Leave URL/filename + bytes for display.
			s.State = "done"
			s.Error = ""
		})
		loggerWithMeta(context.Background(), app.logger, 0).Info("model_downloaded", "url", modelURL, "filename", filename)
	}()

	http.Redirect(w, r, "/settings?status=download+started", http.StatusSeeOther)
	return
	// (async download continues in background)
}

func (app *App) handleModelDownloadStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := app.getModelDownloadStatus()
	percent := int64(0)
	if status.TotalBytes > 0 && status.BytesDownloaded > 0 {
		percent = (status.BytesDownloaded * 100) / status.TotalBytes
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = app.templates.ExecuteTemplate(w, "model_download_status", struct {
		Status  ModelDownloadStatus
		Percent int64
	}{
		Status:  status,
		Percent: percent,
	})
}

func inferModelFilename(modelURL string) (string, error) {
	if strings.TrimSpace(modelURL) == "" {
		return "", errors.New("missing url")
	}
	req, err := http.NewRequest(http.MethodGet, modelURL, nil)
	if err != nil {
		return "", err
	}
	base := filepath.Base(req.URL.Path)
	if base == "" || base == "." || base == "/" {
		return "", errors.New("could not infer filename")
	}
	return base, nil
}

func downloadFileWithResume(
	ctx context.Context,
	client *http.Client,
	modelURL string,
	partPath string,
	finalPath string,
	progress func(bytesDownloaded, totalBytes int64, statusLine string),
) error {
	const (
		maxConsecutiveFailures = 25
		minBackoff             = 700 * time.Millisecond
		maxBackoff             = 20 * time.Second
	)

	consecutiveFailures := 0
	backoff := minBackoff

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		bytesOnDisk, err := fileSize(partPath)
		if err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelURL, nil)
		if err != nil {
			return err
		}
		if bytesOnDisk > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", bytesOnDisk))
		}

		resp, err := client.Do(req)
		if err != nil {
			consecutiveFailures++
			if consecutiveFailures >= maxConsecutiveFailures {
				return fmt.Errorf("download failed after %d retries: %w", consecutiveFailures, err)
			}
			if progress != nil {
				progress(bytesOnDisk, 0, fmt.Sprintf("retrying (%d/%d): %v", consecutiveFailures, maxConsecutiveFailures, err))
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff = minDuration(maxBackoff, backoff*2)
			continue
		}

		totalBytes := int64(0)
		switch resp.StatusCode {
		case http.StatusOK:
			// Server ignored Range; restart from scratch.
			if bytesOnDisk > 0 {
				_ = os.Remove(partPath)
				bytesOnDisk = 0
			}
			if resp.ContentLength > 0 {
				totalBytes = resp.ContentLength
			}
		case http.StatusPartialContent:
			start, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
			if ok {
				totalBytes = total
				// If our local offset differs, we trust server and adjust.
				if start != bytesOnDisk {
					bytesOnDisk = start
					if bytesOnDisk == 0 {
						_ = os.Remove(partPath)
					}
				}
			} else if resp.ContentLength > 0 {
				totalBytes = bytesOnDisk + resp.ContentLength
			}
		case http.StatusRequestedRangeNotSatisfiable:
			// Often means we already have the whole file.
			if resp.ContentLength > 0 {
				totalBytes = resp.ContentLength
			}
			// If final exists, accept; otherwise treat as complete if part exists.
			if _, err := os.Stat(finalPath); err == nil {
				resp.Body.Close()
				return nil
			}
			// If we have a .part, publish it.
			resp.Body.Close()
			if bytesOnDisk > 0 {
				if err := os.Rename(partPath, finalPath); err != nil {
					return err
				}
				return nil
			}
			return fmt.Errorf("range not satisfiable")
		default:
			resp.Body.Close()
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500 {
				consecutiveFailures++
				if consecutiveFailures >= maxConsecutiveFailures {
					return fmt.Errorf("download failed after %d retries: HTTP %d", consecutiveFailures, resp.StatusCode)
				}
				if progress != nil {
					progress(bytesOnDisk, totalBytes, fmt.Sprintf("retrying (%d/%d): HTTP %d", consecutiveFailures, maxConsecutiveFailures, resp.StatusCode))
				}
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return ctx.Err()
				}
				backoff = minDuration(maxBackoff, backoff*2)
				continue
			}
			return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
		}

		fileFlags := os.O_CREATE | os.O_WRONLY
		if bytesOnDisk > 0 {
			fileFlags |= os.O_APPEND
		} else {
			fileFlags |= os.O_TRUNC
		}
		file, err := os.OpenFile(partPath, fileFlags, 0o644)
		if err != nil {
			resp.Body.Close()
			return err
		}

		buf := make([]byte, 1024*256)
		written := int64(0)
		lastUpdate := time.Now()
		copyErr := error(nil)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				wn, werr := file.Write(buf[:n])
				if werr != nil {
					copyErr = werr
					break
				}
				written += int64(wn)
				if wn != n {
					copyErr = io.ErrShortWrite
					break
				}
				if progress != nil && time.Since(lastUpdate) > 250*time.Millisecond {
					progress(bytesOnDisk+written, totalBytes, "")
					lastUpdate = time.Now()
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					break
				}
				copyErr = readErr
				break
			}
		}

		resp.Body.Close()
		_ = file.Close()

		// If we made progress, don't keep counting earlier failures against the cap.
		if written > 0 {
			consecutiveFailures = 0
			backoff = minBackoff
		}

		// Final update at end of attempt.
		if progress != nil {
			progress(bytesOnDisk+written, totalBytes, "")
		}

		// Success path: if no copy error, and we have all bytes (or total unknown), publish.
		if copyErr == nil {
			if totalBytes > 0 {
				finalSize, _ := fileSize(partPath)
				if finalSize < totalBytes {
					copyErr = io.ErrUnexpectedEOF
				}
			}
		}

		if copyErr == nil {
			if err := os.Rename(partPath, finalPath); err != nil {
				return err
			}
			return nil
		}

		// Retryable copy errors (unstable connections) keep the .part file.
		if isRetryableDownloadError(copyErr) {
			consecutiveFailures++
			if consecutiveFailures >= maxConsecutiveFailures {
				return fmt.Errorf("download failed after %d retries: %w", consecutiveFailures, copyErr)
			}
			if progress != nil {
				progress(bytesOnDisk+written, totalBytes, fmt.Sprintf("retrying (%d/%d): %v", consecutiveFailures, maxConsecutiveFailures, copyErr))
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff = minDuration(maxBackoff, backoff*2)
			continue
		}

		return copyErr
	}
}

func isRetryableDownloadError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// Common transport errors come through as plain strings.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "tls")
}

func parseContentRange(value string) (start int64, total int64, ok bool) {
	// Example: "bytes 0-1023/2048"
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, 0, false
	}
	if !strings.HasPrefix(strings.ToLower(value), "bytes") {
		return 0, 0, false
	}
	parts := strings.SplitN(value, " ", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	rangeAndTotal := strings.TrimSpace(parts[1])
	slash := strings.LastIndex(rangeAndTotal, "/")
	if slash < 0 {
		return 0, 0, false
	}
	rangePart := rangeAndTotal[:slash]
	totalPart := rangeAndTotal[slash+1:]
	if totalPart == "*" {
		return 0, 0, false
	}
	dash := strings.Index(rangePart, "-")
	if dash < 0 {
		return 0, 0, false
	}
	startStr := strings.TrimSpace(rangePart[:dash])
	startVal, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	totalVal, err := strconv.ParseInt(strings.TrimSpace(totalPart), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return startVal, totalVal, true
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if info.IsDir() {
		return 0, fmt.Errorf("%s is a directory", path)
	}
	return info.Size(), nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
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
	case modelRoleEmbeddings:
		return filepath.Join(app.cfg.ModelsDir, "current-embedding-model.txt")
	default:
		return app.cfg.CurrentModelPath
	}
}

func normalizeModelRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case modelRoleEmbeddings:
		return modelRoleEmbeddings
	default:
		return modelRoleAnswer
	}
}
