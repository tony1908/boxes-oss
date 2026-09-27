package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
	"golang.org/x/crypto/bcrypt"
)

const sessionLifetime = 30 * 24 * time.Hour
const minimumPasswordLength = 12

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	var input credentials
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "email and password are required")
		return
	}
	if utf8.RuneCountInString(input.Password) < minimumPasswordLength {
		writeError(w, http.StatusBadRequest, "weak_password", "password must be at least 12 characters")
		return
	}
	address, err := mail.ParseAddress(input.Email)
	if err != nil || address.Address != input.Email {
		writeError(w, http.StatusBadRequest, "invalid_email", "a valid email address is required")
		return
	}
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	role := "user"
	if len(users) == 0 {
		role = "owner"
	} else {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			writeError(w, http.StatusForbidden, "owner_required", "only an owner can create users")
			return
		}
		owner, ok := s.userForToken(r, strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
		if !ok || owner.Role != "owner" {
			writeError(w, http.StatusForbidden, "owner_required", "only an owner can create users")
			return
		}
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	if err != nil {
		s.apiError(w, err)
		return
	}
	now := time.Now().UTC()
	user := types.User{ID: types.NewID("usr_"), Email: input.Email, Role: role, PasswordHash: string(passwordHash), CreatedAt: now}
	if err := s.store.CreateUser(r.Context(), user); err != nil {
		writeError(w, http.StatusConflict, "email_exists", "an account with this email already exists")
		return
	}
	token, err := s.createSession(r, user)
	if err != nil {
		s.apiError(w, err)
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "sessionToken": token, "user": user})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var input credentials
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "email and password are required")
		return
	}
	user, err := s.store.UserByEmail(r.Context(), input.Email)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(input.Password)) != nil {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
		return
	}
	token, err := s.createSession(r, user)
	if err != nil {
		s.apiError(w, err)
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "sessionToken": token, "user": user})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	token, _ := r.Context().Value(authTokenContextKey).(string)
	if !strings.HasPrefix(token, "bx_live_") {
		if err := s.store.DeleteSession(r.Context(), tokenHash(token)); err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.apiError(w, err)
			return
		}
	}
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionLifetime.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func csrfMatchesRequestHost(r *http.Request) bool {
	value := r.Header.Get("Origin")
	if value == "" {
		value = r.Header.Get("Referer")
	}
	parsed, err := url.Parse(value)
	return err == nil && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")) && parsed.Host != "" && strings.EqualFold(parsed.Host, r.Host)
}

func (s *Server) createSession(r *http.Request, user types.User) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	return token, s.store.CreateSession(r.Context(), store.Session{
		ID: tokenHash(token), UserID: user.ID, CreatedAt: now, ExpiresAt: now.Add(sessionLifetime),
	})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": currentUser(r)})
}

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListAPIKeys(r.Context(), currentUser(r).ID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	items := make([]types.APIKey, 0, len(keys))
	for _, key := range keys {
		usage, usageErr := s.apiKeyUsageFor(r.Context(), key.ID)
		if usageErr != nil {
			s.apiError(w, usageErr)
			return
		}
		public := key.APIKey
		public.Usage = usage
		items = append(items, public)
	}
	writeJSON(w, http.StatusOK, map[string]any{"apiKeys": items})
}

func (s *Server) apiKeyUsage(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListAPIKeys(r.Context(), currentUser(r).ID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	keyID := pathID(r)
	owned := false
	for _, key := range keys {
		if key.ID == keyID {
			owned = true
			break
		}
	}
	if !owned {
		writeError(w, http.StatusNotFound, "not_found", "API key not found")
		return
	}
	usage, err := s.apiKeyUsageFor(r.Context(), keyID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"usage": usage})
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name      string     `json:"name"`
		Scopes    []string   `json:"scopes"`
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	if err := decodeJSON(r, &input); err != nil || strings.TrimSpace(input.Name) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	keyValue, err := randomToken(32)
	if err != nil {
		s.apiError(w, err)
		return
	}
	keyValue = "bx_live_" + keyValue
	scopes := input.Scopes
	if len(scopes) == 0 {
		scopes = []string{"*"}
	}
	if len(scopes) != 1 || scopes[0] != "*" {
		writeError(w, http.StatusBadRequest, "invalid_scopes", "only the '*' API key scope is supported")
		return
	}
	key := types.APIKey{ID: types.NewID("key_"), Name: input.Name, Key: keyValue, Scopes: scopes, ExpiresAt: input.ExpiresAt, CreatedAt: time.Now().UTC()}
	if err := s.store.CreateAPIKey(r.Context(), currentUser(r).ID, key, tokenHash(keyValue)); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"apiKey": key})
}

func (s *Server) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteAPIKey(r.Context(), currentUser(r).ID, pathID(r)); err != nil {
		s.apiError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) userForToken(r *http.Request, token string) (types.User, bool) {
	if token == "" {
		return types.User{}, false
	}
	ctx := r.Context()
	if strings.HasPrefix(token, "bx_live_") {
		key, err := s.store.APIKeyByHash(ctx, tokenHash(token))
		if err != nil || (key.ExpiresAt != nil && !key.ExpiresAt.After(time.Now())) {
			return types.User{}, false
		}
		user, err := s.store.GetUser(ctx, key.UserID)
		return user, err == nil
	}
	session, err := s.store.GetSession(ctx, tokenHash(token))
	if err != nil || !session.ExpiresAt.After(time.Now()) {
		return types.User{}, false
	}
	user, err := s.store.GetUser(ctx, session.UserID)
	return user, err == nil
}

func randomToken(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
