package server

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
)

func TestGetStorageSettingsIsOwnerOnlyAndNeverExposesCredentials(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ownerToken := addPlatformSession(t, srv)
	member := types.User{ID: "usr_storage_member", Email: "member@example.test", Role: "user", PasswordHash: "unused", CreatedAt: time.Now().UTC()}
	if err := srv.store.CreateUser(context.Background(), member); err != nil {
		t.Fatal(err)
	}
	memberToken := "storage-member-session"
	if err := srv.store.CreateSession(context.Background(), store.Session{ID: tokenHash(memberToken), UserID: member.ID, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	request := func(token string) *http.Response {
		req, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/settings/storage", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := httpServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	memberResponse := request(memberToken)
	memberResponse.Body.Close()
	if memberResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("member storage settings status = %d, want forbidden", memberResponse.StatusCode)
	}
	ownerResponse := request(ownerToken)
	defer ownerResponse.Body.Close()
	if ownerResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(ownerResponse.Body)
		t.Fatalf("owner storage settings status = %d, body %s", ownerResponse.StatusCode, body)
	}
	var payload map[string]any
	if err := json.NewDecoder(ownerResponse.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["type"] != "local" || payload["endpoint"] != "" || payload["bucket"] != "" || payload["prefix"] != "" {
		t.Fatalf("local storage settings = %#v", payload)
	}
	encoded, _ := json.Marshal(payload)
	if strings.Contains(strings.ToLower(string(encoded)), "secret") || strings.Contains(string(encoded), "access") || strings.Contains(string(encoded), "password") {
		t.Fatalf("storage settings exposed credentials: %s", encoded)
	}
}

func TestS3RepositoryURLIncludesOptionalPrefix(t *testing.T) {
	got, err := s3Repository("https://nyc3.digitaloceanspaces.com", "boxes", "team/production")
	if err != nil {
		t.Fatal(err)
	}
	if want := "s3:https://nyc3.digitaloceanspaces.com/boxes/team/production"; got != want {
		t.Fatalf("s3Repository() = %q, want %q", got, want)
	}
	got, err = s3Repository("https://s3.amazonaws.com", "boxes", "")
	if err != nil || got != "s3:https://s3.amazonaws.com/boxes" {
		t.Fatalf("s3Repository() without prefix = %q, %v", got, err)
	}
}

func TestStorageSwitchEncryptsCredentials(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	token := addPlatformSession(t, srv)
	var updated map[string]any
	adminToken, err := os.ReadFile(filepath.Join(srv.cfg.DataDir, "admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/admin/storage", string(adminToken), map[string]any{
		"endpoint": "https://s3.example.test", "bucket": "boxes", "prefix": "snapshots", "region": "region-1",
		"accessKey": "access-plaintext", "secretKey": "secret-plaintext",
	}, http.StatusOK, &updated)
	if updated["type"] != "s3" || updated["endpoint"] != "https://s3.example.test" || updated["prefix"] != "snapshots" {
		t.Fatalf("storage switch response = %#v", updated)
	}
	persisted, err := srv.store.GetSetting(context.Background(), storageSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "access-plaintext") || strings.Contains(string(persisted), "secret-plaintext") {
		t.Fatalf("storage credentials persisted without encryption: %s", persisted)
	}
	var publicSettings map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/settings/storage", token, nil, http.StatusOK, &publicSettings)
	publicJSON, _ := json.Marshal(publicSettings)
	if strings.Contains(string(publicJSON), "access-plaintext") || strings.Contains(string(publicJSON), "secret-plaintext") {
		t.Fatalf("public storage settings exposed credentials: %s", publicJSON)
	}
}

func TestStorageAdminRejectsForwardedRequestsFromPublicHost(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	request := httptest.NewRequest(http.MethodPost, "/admin/storage", strings.NewReader(`{}`))
	request.RemoteAddr = "127.0.0.1:4321"
	request.Host = "boxes.example.test"
	adminToken, err := os.ReadFile(filepath.Join(srv.cfg.DataDir, "admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+string(adminToken))
	response := httptest.NewRecorder()
	srv.APIHandler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("public-host admin request status = %d, want forbidden", response.Code)
	}
}

func TestStorageAdminRequiresTokenAndLoopback(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	adminToken, err := os.ReadFile(filepath.Join(srv.cfg.DataDir, "admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"endpoint":"https://s3.example.test","bucket":"boxes","accessKey":"access","secretKey":"secret"}`
	tests := []struct {
		name       string
		token      string
		remoteAddr string
		wantStatus int
	}{
		{name: "missing token", remoteAddr: "127.0.0.1:4321", wantStatus: http.StatusForbidden},
		{name: "wrong token", token: "wrong-token", remoteAddr: "127.0.0.1:4321", wantStatus: http.StatusForbidden},
		{name: "correct token and loopback", token: string(adminToken), remoteAddr: "127.0.0.1:4321", wantStatus: http.StatusOK},
		{name: "correct token and non-loopback", token: string(adminToken), remoteAddr: "192.0.2.10:4321", wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/admin/storage", strings.NewReader(payload))
			request.RemoteAddr = test.remoteAddr
			request.Host = "localhost"
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			srv.APIHandler().ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("admin request status = %d, want %d; body: %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func TestAdminTokenIsCreatedSecurelyAndReused(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newServer := func() *Server {
		t.Helper()
		srv, err := New(Config{Store: st, DataDir: dataDir})
		if err != nil {
			t.Fatalf("create server: %v", err)
		}
		return srv
	}
	first := newServer()
	tokenPath := filepath.Join(dataDir, "admin.token")
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 {
		t.Fatalf("admin token length = %d, want 64 hex characters", len(token))
	}
	if _, err := hex.DecodeString(string(token)); err != nil {
		t.Fatalf("admin token is not hex: %v", err)
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("admin token permissions = %04o, want 0600", info.Mode().Perm())
	}
	first.Close()
	second := newServer()
	defer second.Close()
	reused, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(reused) != string(token) {
		t.Fatal("admin token changed on server restart")
	}
}

func TestSnapshotStorageHeaderUsesSnapshotRepositoryAndEncryptedEnvironment(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const boxID = "bx_snapshotstorage1"
	seedPlatformBox(t, srv, "nd_snapshotstorage", boxID, "snapshot-storage-token", "", "")
	envJSON, err := json.Marshal(map[string]string{
		"AWS_ACCESS_KEY_ID": "old-access", "AWS_SECRET_ACCESS_KEY": "old-secret", "RESTIC_PASSWORD": "old-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := srv.secrets.encrypt(envJSON)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := types.Snapshot{ID: "sn_snapshotstorage", BoxID: boxID, ResticID: "old-restic-id", Repository: "s3:https://old.example.test/boxes", StorageEnvEnc: encrypted}
	if err := srv.store.CreateSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	stored, err := srv.store.GetSnapshot(context.Background(), snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	headers, err := srv.snapshotStorageHeader(stored)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.RawURLEncoding.DecodeString(headers.Get("X-Boxes-Snapshot-Storage"))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Repository string            `json:"repository"`
		Env        map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Repository != snapshot.Repository || payload.Env["AWS_SECRET_ACCESS_KEY"] != "old-secret" || payload.Env["RESTIC_PASSWORD"] != "old-password" {
		t.Fatalf("per-snapshot storage context = %#v", payload)
	}
}
