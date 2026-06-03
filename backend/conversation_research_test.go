package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// DeleteOverviewMessage backs the re-search "regenerate the answer" flow: it
// must remove the oldest assistant message (the grounded overview) and nothing
// else, so the fresh-load answer stream regenerates it.
func TestDeleteOverviewMessageRemovesOnlyFirstAssistant(t *testing.T) {
	db := openTestDB(t)

	ctx := context.Background()
	userID := "user-1"
	if _, err := db.Exec(`INSERT INTO users (id) VALUES (?)`, userID); err != nil {
		t.Fatalf("insert user failed: %v", err)
	}

	convID := insertConversation(t, db, userID, "hello")
	_ = insertMessage(t, db, convID, "user", "first prompt")
	overviewID := insertMessage(t, db, convID, "assistant", "overview answer")
	_ = insertMessage(t, db, convID, "user", "follow up")
	laterAssistantID := insertMessage(t, db, convID, "assistant", "follow up answer")

	service := &ConversationService{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := service.DeleteOverviewMessage(ctx, convID); err != nil {
		t.Fatalf("DeleteOverviewMessage() failed: %v", err)
	}

	// The overview must be gone, the user turns and the later assistant turn kept.
	var overviewCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ?`, overviewID).Scan(&overviewCount); err != nil {
		t.Fatalf("count overview failed: %v", err)
	}
	if overviewCount != 0 {
		t.Fatalf("overview message still present (count=%d)", overviewCount)
	}

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, convID).Scan(&total); err != nil {
		t.Fatalf("count messages failed: %v", err)
	}
	if total != 3 {
		t.Fatalf("remaining message count = %d, want 3", total)
	}

	var laterCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ?`, laterAssistantID).Scan(&laterCount); err != nil {
		t.Fatalf("count later assistant failed: %v", err)
	}
	if laterCount != 1 {
		t.Fatalf("later assistant message was deleted (count=%d)", laterCount)
	}
}

// With a single assistant message, deleting the overview clears the conversation
// of assistant turns entirely — the precondition the re-search handler relies on
// before letting the answer stream regenerate.
func TestDeleteOverviewMessageClearsSoleAssistant(t *testing.T) {
	db := openTestDB(t)

	ctx := context.Background()
	userID := "user-1"
	if _, err := db.Exec(`INSERT INTO users (id) VALUES (?)`, userID); err != nil {
		t.Fatalf("insert user failed: %v", err)
	}

	convID := insertConversation(t, db, userID, "hello")
	_ = insertMessage(t, db, convID, "user", "the prompt")
	_ = insertMessage(t, db, convID, "assistant", "the overview")

	service := &ConversationService{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := service.DeleteOverviewMessage(ctx, convID); err != nil {
		t.Fatalf("DeleteOverviewMessage() failed: %v", err)
	}

	var assistantCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ? AND role = 'assistant'`, convID).Scan(&assistantCount); err != nil {
		t.Fatalf("count assistant failed: %v", err)
	}
	if assistantCount != 0 {
		t.Fatalf("assistant messages remaining = %d, want 0", assistantCount)
	}
}
