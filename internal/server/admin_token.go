package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const adminTokenFile = "admin.token"

func openAdminToken(dataDir string) (string, error) {
	path := filepath.Join(dataDir, adminTokenFile)
	if _, err := os.Lstat(path); err == nil {
		return readAdminToken(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect admin token: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return "", fmt.Errorf("generate admin token: %w", err)
	}
	token := hex.EncodeToString(secret)

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return readAdminToken(path)
	}
	if err != nil {
		return "", fmt.Errorf("create admin token: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("secure admin token: %w", err)
	}
	if _, err := io.WriteString(file, token); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write admin token: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close admin token: %w", err)
	}
	return token, nil
}

func readAdminToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect admin token: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("admin token must be a regular file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("secure admin token: %w", err)
	}
	token, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read admin token: %w", err)
	}
	if len(token) != hex.EncodedLen(32) {
		return "", errors.New("admin token must contain 32 bytes encoded as hex")
	}
	decoded := make([]byte, hex.DecodedLen(len(token)))
	if _, err := hex.Decode(decoded, token); err != nil || len(decoded) != 32 {
		return "", errors.New("admin token must contain 32 bytes encoded as hex")
	}
	return string(token), nil
}

func adminTokenMatches(header, expected string) bool {
	const bearerPrefix = "Bearer "
	if len(header) < len(bearerPrefix) || subtle.ConstantTimeCompare([]byte(header[:len(bearerPrefix)]), []byte(bearerPrefix)) != 1 {
		return false
	}
	provided := header[len(bearerPrefix):]
	return len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func (s *Server) requireLocalAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !adminTokenMatches(r.Header.Get("Authorization"), s.adminToken) {
		writeError(w, http.StatusForbidden, "admin_token_required", "valid local admin token required")
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	requestHost := r.Host
	if parsedHost, _, splitErr := net.SplitHostPort(requestHost); splitErr == nil {
		requestHost = parsedHost
	}
	localRequestHost := strings.EqualFold(requestHost, "localhost")
	if ip := net.ParseIP(requestHost); ip != nil && ip.IsLoopback() {
		localRequestHost = true
	}
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || !localRequestHost {
		writeError(w, http.StatusForbidden, "local_only", "administration is available only from localhost")
		return false
	}
	return true
}
