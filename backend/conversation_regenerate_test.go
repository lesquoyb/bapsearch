package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() failed: %v", err)
	}

	db.SetConnMaxLifetime(0)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := applySchema(db, "../database/schema.sql"); err != nil {
		db.Close()
		t.Fatalf("applySchema() failed: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	return db
}

func insertConversation(t *testing.T, db *sql.DB, userID, title string) int64 {
	t.Helper()

	res, err := db.Exec(`INSERT INTO conversations (user_id, title) VALUES (?, ?)`, userID, title)
	if err != nil {
		t.Fatalf("insert conversation failed: %v", err)
	}
	convID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("conversation LastInsertId failed: %v", err)
	}
	return convID
}

func insertMessage(t *testing.T, db *sql.DB, conversationID int64, role, content string) int64 {
	t.Helper()

	res, err := db.Exec(`INSERT INTO messages (conversation_id, role, content) VALUES (?, ?, ?)`, conversationID, role, content)
	if err != nil {
		t.Fatalf("insert message failed: %v", err)
	}
	messageID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("message LastInsertId failed: %v", err)
	}
	return messageID
}

func TestTruncateConversationForRegenerationDeletesTailAndReturnsPrompt(t *testing.T) {
	db := openTestDB(t)

	ctx := context.Background()
	userID := "user-1"
	if _, err := db.Exec(`INSERT INTO users (id) VALUES (?)`, userID); err != nil {
		t.Fatalf("insert user failed: %v", err)
	}

	convID := insertConversation(t, db, userID, "hello")
	_ = insertMessage(t, db, convID, "user", "first prompt")
	_ = insertMessage(t, db, convID, "assistant", "first answer")
	userPromptID := insertMessage(t, db, convID, "user", "  follow up prompt\n")
	assistantID := insertMessage(t, db, convID, "assistant", "second answer")

	service := &ConversationService{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	prompt, err := service.TruncateConversationForRegeneration(ctx, userID, convID, assistantID)
	if err != nil {
		t.Fatalf("TruncateConversationForRegeneration() failed: %v", err)
	}
	if prompt != "follow up prompt" {
		t.Fatalf("prompt = %q, want %q", prompt, "follow up prompt")
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, convID).Scan(&count); err != nil {
		t.Fatalf("count messages failed: %v", err)
	}
	if count != 3 {
		t.Fatalf("message count after truncation = %d, want %d", count, 3)
	}

	// Ensure the last remaining message is the preceding user prompt.
	var maxID int64
	if err := db.QueryRow(`SELECT MAX(id) FROM messages WHERE conversation_id = ?`, convID).Scan(&maxID); err != nil {
		t.Fatalf("max message id failed: %v", err)
	}
	if maxID != userPromptID {
		t.Fatalf("max message id = %d, want %d", maxID, userPromptID)
	}
}

func TestTruncateConversationForRegenerationMidThreadTruncatesFromSelectedAssistant(t *testing.T) {
	db := openTestDB(t)

	ctx := context.Background()
	userID := "user-1"
	if _, err := db.Exec(`INSERT INTO users (id) VALUES (?)`, userID); err != nil {
		t.Fatalf("insert user failed: %v", err)
	}

	convID := insertConversation(t, db, userID, "hello")
	firstPromptID := insertMessage(t, db, convID, "user", "first prompt")
	assistantID := insertMessage(t, db, convID, "assistant", "first answer")
	_ = insertMessage(t, db, convID, "user", "second prompt")
	_ = insertMessage(t, db, convID, "assistant", "second answer")

	service := &ConversationService{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	prompt, err := service.TruncateConversationForRegeneration(ctx, userID, convID, assistantID)
	if err != nil {
		t.Fatalf("TruncateConversationForRegeneration() failed: %v", err)
	}
	if prompt != "first prompt" {
		t.Fatalf("prompt = %q, want %q", prompt, "first prompt")
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, convID).Scan(&count); err != nil {
		t.Fatalf("count messages failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("message count after truncation = %d, want %d", count, 1)
	}

	var maxID int64
	if err := db.QueryRow(`SELECT MAX(id) FROM messages WHERE conversation_id = ?`, convID).Scan(&maxID); err != nil {
		t.Fatalf("max message id failed: %v", err)
	}
	if maxID != firstPromptID {
		t.Fatalf("max message id = %d, want %d", maxID, firstPromptID)
	}
}

func TestTruncateConversationForRegenerationRejectsNonAssistantTarget(t *testing.T) {
	db := openTestDB(t)

	ctx := context.Background()
	userID := "user-1"
	if _, err := db.Exec(`INSERT INTO users (id) VALUES (?)`, userID); err != nil {
		t.Fatalf("insert user failed: %v", err)
	}

	convID := insertConversation(t, db, userID, "hello")
	_ = insertMessage(t, db, convID, "user", "first prompt")
	userMessageID := insertMessage(t, db, convID, "user", "second prompt")

	service := &ConversationService{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, err := service.TruncateConversationForRegeneration(ctx, userID, convID, userMessageID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestTruncateConversationForRegenerationRequiresPrecedingUserMessage(t *testing.T) {
	db := openTestDB(t)

	ctx := context.Background()
	userID := "user-1"
	if _, err := db.Exec(`INSERT INTO users (id) VALUES (?)`, userID); err != nil {
		t.Fatalf("insert user failed: %v", err)
	}

	convID := insertConversation(t, db, userID, "hello")
	assistantID := insertMessage(t, db, convID, "assistant", "first answer")

	service := &ConversationService{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, err := service.TruncateConversationForRegeneration(ctx, userID, convID, assistantID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestTruncateConversationForRegenerationRequiresConversationOwnership(t *testing.T) {
	db := openTestDB(t)

	ctx := context.Background()
	ownerID := "user-1"
	otherID := "user-2"
	if _, err := db.Exec(`INSERT INTO users (id) VALUES (?)`, ownerID); err != nil {
		t.Fatalf("insert owner failed: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (id) VALUES (?)`, otherID); err != nil {
		t.Fatalf("insert other user failed: %v", err)
	}

	convID := insertConversation(t, db, ownerID, "hello")
	_ = insertMessage(t, db, convID, "user", "first prompt")
	assistantID := insertMessage(t, db, convID, "assistant", "first answer")

	service := &ConversationService{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, err := service.TruncateConversationForRegeneration(ctx, otherID, convID, assistantID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}
