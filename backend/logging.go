package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type RequestMeta struct {
	RequestID      string
	UserID         string
	ConversationID int64
}

type contextKey string

const requestMetaKey contextKey = "request-meta"

func newJSONLogger(logPath string) (*slog.Logger, func() error, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, nil, err
	}

	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}

	handler := slog.NewJSONHandler(io.MultiWriter(os.Stdout, file), &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)
	return logger, file.Close, nil
}

func withMiddlewares(next http.Handler, logger *slog.Logger, allowAnonymous bool) http.Handler {
	return recoverMiddleware(logger, authMiddleware(loggingMiddleware(next, logger), allowAnonymous))
}

func loggingMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		meta := requestMetaFromContext(r.Context())
		logger.Info("http_request",
			"timestamp", time.Now().UTC().Format(time.RFC3339),
			"request_id", meta.RequestID,
			"user_id", meta.UserID,
			"conversation_id", meta.ConversationID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

func authMiddleware(next http.Handler, allowAnonymous bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			meta := RequestMeta{RequestID: newRequestID(), UserID: "healthcheck"}
			ctx := context.WithValue(r.Context(), requestMetaKey, meta)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		userID := strings.TrimSpace(r.Header.Get("X-Forwarded-User"))
		if userID == "" {
			if !allowAnonymous {
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			userID = "dev-user"
		}

		conversationID := int64(0)
		if header := strings.TrimSpace(r.Header.Get("X-Conversation-ID")); header != "" {
			if parsed, err := strconv.ParseInt(header, 10, 64); err == nil {
				conversationID = parsed
			}
		}

		meta := RequestMeta{
			RequestID:      newRequestID(),
			UserID:         userID,
			ConversationID: conversationID,
		}
		ctx := context.WithValue(r.Context(), requestMetaKey, meta)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func recoverMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				meta := requestMetaFromContext(r.Context())
				logger.Error("panic recovered",
					"timestamp", time.Now().UTC().Format(time.RFC3339),
					"request_id", meta.RequestID,
					"user_id", meta.UserID,
					"conversation_id", meta.ConversationID,
					"panic", recovered,
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func requestMetaFromContext(ctx context.Context) RequestMeta {
	meta, ok := ctx.Value(requestMetaKey).(RequestMeta)
	if !ok {
		return RequestMeta{}
	}
	return meta
}

func loggerWithMeta(ctx context.Context, logger *slog.Logger, conversationID int64) *slog.Logger {
	meta := requestMetaFromContext(ctx)
	if conversationID != 0 {
		meta.ConversationID = conversationID
	}
	return logger.With(
		"request_id", meta.RequestID,
		"user_id", meta.UserID,
		"conversation_id", meta.ConversationID,
	)
}

func newRequestID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buffer)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (recorder *statusRecorder) WriteHeader(statusCode int) {
	recorder.status = statusCode
	recorder.ResponseWriter.WriteHeader(statusCode)
}
