package agent

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ForwardAuthHandler returns the local-only Traefik forward-auth endpoint.
func (a *Agent) ForwardAuthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/forward", a.forwardAuthRequest)
	return mux
}

func (a *Agent) forwardAuthRequest(w http.ResponseWriter, r *http.Request) {
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if forwardedHost, _, err := net.SplitHostPort(host); err == nil {
		host = forwardedHost
	}
	host = strings.ToLower(host)
	forwardedURI := r.Header.Get("X-Forwarded-Uri")
	parsed, err := url.ParseRequestURI(forwardedURI)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := parsed.Query().Get("token")
	fromQuery := token != ""
	if !fromQuery {
		if cookie, err := r.Cookie("boxes_auth"); err == nil {
			token = cookie.Value
		}
	}
	a.mu.RLock()
	credential, ok := a.forwardAuth[host]
	a.mu.RUnlock()
	if !ok || token == "" || !tokenMatches(credential.hash, token) || (!credential.expiresAt.IsZero() && time.Now().After(credential.expiresAt)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if fromQuery {
		cookie := &http.Cookie{Name: "boxes_auth", Value: token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 3600}
		if !credential.expiresAt.IsZero() {
			cookie.Expires = credential.expiresAt
			remaining := int(time.Until(credential.expiresAt).Seconds())
			if remaining < 1 {
				remaining = 1
			}
			cookie.MaxAge = remaining
		}
		http.SetCookie(w, cookie)
	}
	w.WriteHeader(http.StatusNoContent)
}

func tokenMatches(expectedHash, token string) bool {
	if expectedHash == "" || token == "" {
		return false
	}
	actual := sha256.Sum256([]byte(token))
	expected := []byte(strings.ToLower(expectedHash))
	actualHex := []byte(hex.EncodeToString(actual[:]))
	if len(expected) != len(actualHex) {
		return false
	}
	return subtle.ConstantTimeCompare(expected, actualHex) == 1
}
