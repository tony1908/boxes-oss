package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEnvironmentResponsesNeverContainSecretValuesOrFileContents(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	var signup map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{"email": "owner@example.test", "password": "correct horse battery staple", "setupToken": srv.adminToken}, http.StatusCreated, &signup)
	token := signup["token"].(string)
	const marker = "SECRET_FIXTURE_VALUE_8db3b1"
	create := doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/environments", map[string]string{"Authorization": "Bearer " + token}, map[string]any{"name": "secure-env", "vars": map[string]string{"VISIBLE": "ordinary"}, "secrets": map[string]string{"API_TOKEN": marker}, "secretFiles": map[string]string{"credentials/config": "FILE_" + marker}})
	if create.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(create.Body)
		create.Body.Close()
		t.Fatalf("create environment status=%d body=%s", create.StatusCode, data)
	}
	var body map[string]any
	if err := json.NewDecoder(create.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	create.Body.Close()
	envID := body["environment"].(map[string]any)["id"].(string)
	assertNoSecretMarker(t, "create", body, marker)
	requests := []struct {
		name, method, path string
		payload            any
		want               int
	}{
		{"list", http.MethodGet, "/api/v1/environments", nil, http.StatusOK},
		{"get", http.MethodGet, "/api/v1/environments/" + envID, nil, http.StatusOK},
		{"update", http.MethodPatch, "/api/v1/environments/" + envID, map[string]any{"secrets": map[string]string{"API_TOKEN": marker}, "secretFiles": map[string]string{"credentials/config": "FILE_" + marker}}, http.StatusOK},
		{"upgrade", http.MethodPost, "/api/v1/environments/" + envID + "/upgrade", map[string]any{}, http.StatusOK},
	}
	for _, item := range requests {
		t.Run(item.name, func(t *testing.T) {
			response := doJSONRequest(t, httpServer.Client(), item.method, httpServer.URL+item.path, map[string]string{"Authorization": "Bearer " + token}, item.payload)
			defer response.Body.Close()
			data, _ := io.ReadAll(response.Body)
			if response.StatusCode != item.want {
				t.Fatalf("status=%d body=%s", response.StatusCode, data)
			}
			if strings.Contains(string(data), marker) {
				t.Fatalf("response leaked secret fixture: %s", data)
			}
		})
	}
}

func assertNoSecretMarker(t *testing.T, operation string, value any, marker string) {
	t.Helper()
	encoded, _ := json.Marshal(value)
	if strings.Contains(string(encoded), marker) {
		t.Fatalf("%s response leaked secret fixture: %s", operation, encoded)
	}
}
