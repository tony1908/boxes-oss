package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
)

func TestRegistrySettingsEncryptPasswordAndPropagateCredentialsToNodeInstall(t *testing.T) {
	t.Setenv("BOXES_REGISTRY_URL", "registry.example.test")
	t.Setenv("BOXES_REGISTRY_USERNAME", "boxes-publisher")
	t.Setenv("BOXES_REGISTRY_PASSWORD", "registry-password-secret")
	srv, cleanup := newTestServer(t)
	defer cleanup()

	persisted, err := srv.store.GetSetting(context.Background(), registrySettingKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "registry-password-secret") {
		t.Fatalf("registry password persisted in plaintext: %s", persisted)
	}
	var stored storedRegistryConfig
	if err := json.Unmarshal(persisted, &stored); err != nil {
		t.Fatal(err)
	}
	password, err := srv.secrets.decrypt(stored.PasswordEnc)
	if err != nil {
		t.Fatalf("decrypt registry password: %v", err)
	}
	if stored.URL != "registry.example.test" || stored.Username != "boxes-publisher" || string(password) != "registry-password-secret" {
		t.Fatalf("stored registry settings = url %q username %q password %q", stored.URL, stored.Username, password)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	token := addPlatformSession(t, srv)
	var node map[string]any
	requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/nodes", token,
		map[string]any{"name": "registry-worker"}, http.StatusCreated, &node)
	command, _ := node["installCommand"].(string)
	for _, expected := range []string{"BOXES_REGISTRY_URL='registry.example.test'", "BOXES_REGISTRY_USERNAME='boxes-publisher'", "BOXES_REGISTRY_PASSWORD='registry-password-secret'"} {
		if !strings.Contains(command, expected) {
			t.Errorf("node install command %q does not contain %q", command, expected)
		}
	}
}

func TestRegistrySettingsAreOwnerOnlyAndReturnPasswordForPublishing(t *testing.T) {
	t.Setenv("BOXES_REGISTRY_URL", "registry.example.test")
	t.Setenv("BOXES_REGISTRY_USERNAME", "publisher")
	t.Setenv("BOXES_REGISTRY_PASSWORD", "secret")
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ownerToken := addPlatformSession(t, srv)
	member := types.User{ID: "usr_registry_member", Email: "registry-member@example.test", Role: "user", PasswordHash: "unused", CreatedAt: time.Now().UTC()}
	if err := srv.store.CreateUser(context.Background(), member); err != nil {
		t.Fatal(err)
	}
	memberToken := "registry-member-session"
	if err := srv.store.CreateSession(context.Background(), store.Session{ID: tokenHash(memberToken), UserID: member.ID, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/settings/registry", memberToken, nil, http.StatusForbidden, nil)
	var settings map[string]any
	requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/settings/registry", ownerToken, nil, http.StatusOK, &settings)
	if settings["url"] != "registry.example.test" || settings["username"] != "publisher" || settings["password"] != "secret" {
		t.Fatalf("registry settings response = %#v", settings)
	}
}

func TestDefaultImagesCanBeUpdatedAndUsedForNewBoxes(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	token := addPlatformSession(t, srv)

	var response map[string]any
	requestJSON(t, server.Client(), http.MethodPut, server.URL+"/api/v1/settings/images", token,
		map[string]string{"boxImage": "registry.example.test/boxes/box:v7", "toolsImage": "registry.example.test/boxes/tools:v7"}, http.StatusOK, &response)
	if response["boxImage"] != "registry.example.test/boxes/box:v7" || response["toolsImage"] != "registry.example.test/boxes/tools:v7" {
		t.Fatalf("image settings response = %#v", response)
	}
	images := srv.defaultImages()
	if images.Box != response["boxImage"].(string) || images.Tools != response["toolsImage"].(string) {
		t.Fatalf("default images = %#v", images)
	}
}
