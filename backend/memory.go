package main

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
)

type MemoryService struct {
	db            *sql.DB
	llm           *LLMService
	conversations *ConversationService
	logger        *slog.Logger
}

func (service *MemoryService) GetUserMemory(ctx context.Context, userID string) (string, error) {
	row := service.db.QueryRowContext(ctx, `
        SELECT memory_summary
        FROM user_memory
        WHERE user_id = ?
    `, userID)

	var summary string
	err := row.Scan(&summary)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return summary, err
}

func (service *MemoryService) UpsertUserMemory(ctx context.Context, userID, summary string) error {
	_, err := service.db.ExecContext(ctx, `
        INSERT INTO user_memory (user_id, memory_summary)
        VALUES (?, ?)
        ON CONFLICT(user_id) DO UPDATE SET
            memory_summary = excluded.memory_summary,
            updated_at = CURRENT_TIMESTAMP
    `, userID, summary)
	return err
}

func (service *MemoryService) MaybeRefreshUserMemory(meta RequestMeta, userID string, conversationID int64) {
	ctx := context.WithValue(context.Background(), requestMetaKey, meta)

	transcript, err := service.conversations.BuildTranscript(ctx, conversationID, 18)
	if err != nil {
		loggerWithMeta(ctx, service.logger, conversationID).Error("memory transcript build failed", "error", err)
		return
	}

	if strings.Count(transcript, "user:") < 3 {
		return
	}

	currentMemory, err := service.GetUserMemory(ctx, userID)
	if err != nil {
		loggerWithMeta(ctx, service.logger, conversationID).Error("loading user memory failed", "error", err)
		return
	}

	updatedMemory, err := service.llm.UpdateUserMemory(ctx, meta, currentMemory, transcript)
	if err != nil {
		loggerWithMeta(ctx, service.logger, conversationID).Error("memory generation failed", "error", err)
		return
	}

	if err := service.UpsertUserMemory(ctx, userID, updatedMemory); err != nil {
		loggerWithMeta(ctx, service.logger, conversationID).Error("memory storage failed", "error", err)
		return
	}

	loggerWithMeta(ctx, service.logger, conversationID).Info("user_memory_updated")
}
