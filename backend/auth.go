package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookieName = "bap_session"
	sessionDuration   = 30 * 24 * time.Hour // 30 days
	bcryptCost        = 12
	minPasswordLength = 6
	maxUsernameLength = 64
)

// --- Session helpers ---

// createSessionToken builds a signed token: userID|expiry|signature
func createSessionToken(userID string, secret string) string {
	expiry := time.Now().Add(sessionDuration).Unix()
	payload := fmt.Sprintf("%s|%d", userID, expiry)
	signature := signPayload(payload, secret)
	return fmt.Sprintf("%s|%s", payload, signature)
}

func signPayload(payload, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// parseSessionToken extracts the userID from a signed token, or "" if invalid/expired.
func parseSessionToken(token, secret string) string {
	parts := strings.SplitN(token, "|", 3)
	if len(parts) != 3 {
		return ""
	}

	userID := parts[0]
	expiryStr := parts[1]
	signature := parts[2]

	payload := fmt.Sprintf("%s|%s", userID, expiryStr)
	expectedSig := signPayload(payload, secret)
	if !hmac.Equal([]byte(signature), []byte(expectedSig)) {
		return ""
	}

	var expiry int64
	if _, err := fmt.Sscanf(expiryStr, "%d", &expiry); err != nil {
		return ""
	}
	if time.Now().Unix() > expiry {
		return ""
	}

	return userID
}

func setSessionCookie(w http.ResponseWriter, userID, secret string) {
	token := createSessionToken(userID, secret)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionDuration.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// --- Database helpers ---

// createUserWithPassword registers a new user with a bcrypt-hashed password.
func (service *ConversationService) CreateUserWithPassword(ctx context.Context, userID, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return fmt.Errorf("bcrypt hash: %w", err)
	}

	_, err = service.db.ExecContext(ctx, `
		INSERT INTO users (id, password_hash) VALUES (?, ?)
	`, userID, string(hash))
	return err
}

// AuthenticateUser checks the password against the stored hash and returns true if it matches.
func (service *ConversationService) AuthenticateUser(ctx context.Context, userID, password string) (bool, error) {
	var hash string
	err := service.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&hash)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if hash == "" {
		// User exists without password (e.g., created via proxy header).
		return false, nil
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return false, nil
	}
	return true, nil
}

// UserExists returns true if the user ID already exists.
func (service *ConversationService) UserExists(ctx context.Context, userID string) (bool, error) {
	var count int
	err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, userID).Scan(&count)
	return count > 0, err
}

// --- HTTP handlers ---

func (app *App) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		app.render(w, "login", PageData{
			AppName: "bap-search",
			Error:   r.URL.Query().Get("error"),
		})
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	if username == "" || password == "" {
		http.Redirect(w, r, "/login?error=Username+and+password+are+required", http.StatusSeeOther)
		return
	}

	ok, err := app.conversations.AuthenticateUser(r.Context(), username, password)
	if err != nil {
		app.logger.Error("login authentication error", "error", err, "user", username)
		http.Redirect(w, r, "/login?error=Internal+error", http.StatusSeeOther)
		return
	}
	if !ok {
		http.Redirect(w, r, "/login?error=Invalid+username+or+password", http.StatusSeeOther)
		return
	}

	// Update last_seen_at
	_ = app.conversations.EnsureUser(r.Context(), username)

	setSessionCookie(w, username, app.cfg.SessionSecret)
	app.logger.Info("user_login", "user", username)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (app *App) handleRegisterPage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		app.render(w, "register", PageData{
			AppName: "bap-search",
			Error:   r.URL.Query().Get("error"),
		})
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")

	// Validation
	if username == "" || password == "" {
		http.Redirect(w, r, "/register?error=All+fields+are+required", http.StatusSeeOther)
		return
	}

	if len(username) > maxUsernameLength {
		http.Redirect(w, r, fmt.Sprintf("/register?error=Username+must+be+at+most+%d+characters", maxUsernameLength), http.StatusSeeOther)
		return
	}

	if len(password) < minPasswordLength {
		http.Redirect(w, r, fmt.Sprintf("/register?error=Password+must+be+at+least+%d+characters", minPasswordLength), http.StatusSeeOther)
		return
	}

	if password != confirm {
		http.Redirect(w, r, "/register?error=Passwords+do+not+match", http.StatusSeeOther)
		return
	}

	exists, err := app.conversations.UserExists(r.Context(), username)
	if err != nil {
		app.logger.Error("register check user error", "error", err)
		http.Redirect(w, r, "/register?error=Internal+error", http.StatusSeeOther)
		return
	}
	if exists {
		http.Redirect(w, r, "/register?error=Username+already+taken", http.StatusSeeOther)
		return
	}

	if err := app.conversations.CreateUserWithPassword(r.Context(), username, password); err != nil {
		app.logger.Error("register create user error", "error", err)
		http.Redirect(w, r, "/register?error=Failed+to+create+account", http.StatusSeeOther)
		return
	}

	setSessionCookie(w, username, app.cfg.SessionSecret)
	app.logger.Info("user_registered", "user", username)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (app *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// generateSessionSecret creates a random 32-byte hex string for use as a default session secret.
func generateSessionSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "bap-search-default-secret-change-me"
	}
	return hex.EncodeToString(b)
}
