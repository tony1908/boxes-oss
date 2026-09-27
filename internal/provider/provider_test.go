package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDigitalOceanProviderUsesDropletAPIAndTags(t *testing.T) {
	var created map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer do-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/droplets":
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"droplet":{"id":42,"name":"pool-a-1","status":"new","networks":{"v4":[{"ip_address":"203.0.113.4","type":"public"}]}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/droplets/42":
			_, _ = w.Write([]byte(`{"droplet":{"id":42,"name":"pool-a-1","status":"active","networks":{"v4":[{"ip_address":"203.0.113.4","type":"public"}]}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/droplets" && r.URL.Query().Get("tag_name") == "boxes-pool-pool_a" && r.URL.Query().Get("page") == "1":
			_, _ = w.Write([]byte(`{"droplets":[{"id":42,"name":"pool-a-1","status":"active","networks":{"v4":[{"ip_address":"203.0.113.4","type":"public"}]}}],"links":{"pages":{"next":"https://api.digitalocean.com/v2/droplets?page=2"}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/droplets" && r.URL.Query().Get("tag_name") == "boxes-pool-pool_a" && r.URL.Query().Get("page") == "2":
			_, _ = w.Write([]byte(`{"droplets":[{"id":43,"name":"pool-a-2","status":"active","networks":{"v4":[]}}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v2/droplets/42":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected DO request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := &DigitalOcean{Token: "do-token", BaseURL: server.URL + "/v2", Client: server.Client()}
	spec := NodeSpec{Name: "pool-a-1", Region: "nyc3", Size: "s-2vcpu-4gb", Image: "ubuntu-24-04-x64", UserData: "#cloud-config\nruncmd: []", Tags: []string{"boxes-pool-pool_a"}, SSHKeys: []string{"123"}}
	instance, err := p.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != "42" || instance.PublicIPv4 != "203.0.113.4" || instance.Status != "new" {
		t.Fatalf("Create() = %#v", instance)
	}
	if created["user_data"] != spec.UserData || created["region"] != "nyc3" || !reflect.DeepEqual(created["tags"], []any{"boxes-pool-pool_a"}) {
		t.Fatalf("create request = %#v", created)
	}
	got, err := p.Get(context.Background(), "42")
	if err != nil || got.Status != "active" {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	instances, err := p.List(context.Background(), "boxes-pool-pool_a")
	if err != nil || len(instances) != 2 || instances[0].ID != "42" || instances[1].ID != "43" {
		t.Fatalf("List() = %#v, %v", instances, err)
	}
	if err := p.Delete(context.Background(), "42"); err != nil {
		t.Fatal(err)
	}
}

func TestDigitalOceanCreateImageWaitsForActionAndReturnsNewestSnapshot(t *testing.T) {
	actionPolls, snapshotLookups := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/droplets/42/actions":
			_, _ = w.Write([]byte(`{"action":{"id":901}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/actions/901":
			actionPolls++
			if actionPolls == 1 {
				_, _ = w.Write([]byte(`{"action":{"status":"in-progress"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"action":{"status":"completed"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/droplets/42/snapshots":
			snapshotLookups++
			_, _ = w.Write([]byte(`{"snapshots":[{"id":11,"name":"baked","created_at":"2026-09-24T10:00:00Z"},{"id":12,"name":"other","created_at":"2026-09-25T10:00:00Z"},{"id":13,"name":"baked","created_at":"2026-09-25T11:00:00Z"}]}`))
		default:
			t.Errorf("unexpected DO request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := &DigitalOcean{BaseURL: server.URL + "/v2", Client: server.Client()}
	id, err := p.CreateImage(context.Background(), "42", "baked")
	if err != nil || id != "13" {
		t.Fatalf("CreateImage() = %q, %v", id, err)
	}
	if actionPolls != 2 || snapshotLookups != 1 {
		t.Fatalf("polls/lookups = %d/%d", actionPolls, snapshotLookups)
	}
}

func TestDigitalOceanCreateImageReturnsErroredAction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/droplets/42/actions":
			_, _ = w.Write([]byte(`{"action":{"id":901}}`))
		case "/v2/actions/901":
			_, _ = w.Write([]byte(`{"action":{"status":"errored"}}`))
		default:
			t.Errorf("unexpected DO request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := &DigitalOcean{BaseURL: server.URL + "/v2", Client: server.Client()}
	if _, err := p.CreateImage(context.Background(), "42", "baked"); err == nil {
		t.Fatal("CreateImage() error = nil, want errored action")
	}
}

func TestHetznerProviderUsesServerAPIAndLabels(t *testing.T) {
	var created map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hcloud-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/servers":
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"server":{"id":77,"name":"pool-b-1","status":"initializing","public_net":{"ipv4":{"ip":"198.51.100.9"}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/servers/77":
			_, _ = w.Write([]byte(`{"server":{"id":77,"name":"pool-b-1","status":"running","public_net":{"ipv4":{"ip":"198.51.100.9"}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/servers" && r.URL.Query().Get("label_selector") == "boxes-pool=pool_b" && r.URL.Query().Get("page") == "1":
			_, _ = w.Write([]byte(`{"servers":[{"id":77,"name":"pool-b-1","status":"running","public_net":{"ipv4":{"ip":"198.51.100.9"}}}],"meta":{"pagination":{"next_page":2}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/servers" && r.URL.Query().Get("label_selector") == "boxes-pool=pool_b" && r.URL.Query().Get("page") == "2":
			_, _ = w.Write([]byte(`{"servers":[{"id":78,"name":"pool-b-2","status":"running","public_net":{"ipv4":{"ip":"198.51.100.10"}}}],"meta":{"pagination":{"next_page":null}}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/servers/77":
			_, _ = w.Write([]byte(`{"action":{"id":1,"status":"running"}}`))
		default:
			t.Errorf("unexpected Hetzner request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := &Hetzner{Token: "hcloud-token", BaseURL: server.URL + "/v1", Client: server.Client()}
	spec := NodeSpec{Name: "pool-b-1", Region: "fsn1", Size: "cx22", Image: "ubuntu-24.04", UserData: "#cloud-config\nruncmd: []", Tags: []string{"boxes-pool-pool_b"}}
	instance, err := p.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != "77" || instance.PublicIPv4 != "198.51.100.9" || instance.Status != "initializing" {
		t.Fatalf("Create() = %#v", instance)
	}
	labels, ok := created["labels"].(map[string]any)
	if !ok || labels["boxes-pool"] != "pool_b" || created["user_data"] != spec.UserData {
		t.Fatalf("create request = %#v", created)
	}
	got, err := p.Get(context.Background(), "77")
	if err != nil || got.Status != "running" {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	instances, err := p.List(context.Background(), "boxes-pool-pool_b")
	if err != nil || len(instances) != 2 || instances[0].ID != "77" || instances[1].ID != "78" {
		t.Fatalf("List() = %#v, %v", instances, err)
	}
	if err := p.Delete(context.Background(), "77"); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryFakeCreateListAndDelete(t *testing.T) {
	p := NewFake()
	created, err := p.Create(context.Background(), NodeSpec{Name: "node-a", Tags: []string{"boxes-pool-p"}})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := p.List(context.Background(), "boxes-pool-p")
	if err != nil || len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("List() = %#v, %v", listed, err)
	}
	if err := p.Delete(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	listed, err = p.List(context.Background(), "boxes-pool-p")
	if err != nil || len(listed) != 0 {
		t.Fatalf("List after Delete() = %#v, %v", listed, err)
	}
}

func TestScriptProviderUsesJSONStdinAndArgumentVector(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "fake provider")
	createInput := filepath.Join(directory, "create.json")
	deleteArgs := filepath.Join(directory, "delete-arg")
	listArg := filepath.Join(directory, "list-arg")
	script := "#!/bin/sh\ncase \"$1\" in\ncreate) IFS= read -r payload || true; printf '%s' \"$payload\" > \"" + createInput + "\"; printf '{\"id\":\"script-1\",\"name\":\"script-node\",\"status\":\"active\"}' ;;\ndelete) printf '%s' \"$2\" > \"" + deleteArgs + "\" ;;\nlist) printf '%s' \"$2\" > \"" + listArg + "\"; printf '[{\"id\":\"script-1\",\"name\":\"script-node\",\"status\":\"active\"}]' ;;\nesac\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	p := Script{Executable: executable, Timeout: 10 * time.Second}
	spec := NodeSpec{Name: "script-node", Region: "test", Tags: []string{"pool-tag"}}
	instance, err := p.Create(context.Background(), spec)
	if err != nil || instance.ID != "script-1" {
		t.Fatalf("Create() = %#v, %v", instance, err)
	}
	input, err := os.ReadFile(createInput)
	if err != nil {
		t.Fatal(err)
	}
	var gotSpec NodeSpec
	if err := json.Unmarshal(input, &gotSpec); err != nil || gotSpec.Name != spec.Name || !reflect.DeepEqual(gotSpec.Tags, spec.Tags) {
		t.Fatalf("create stdin = %s, %v", input, err)
	}
	if err := p.Delete(context.Background(), "provider id; not shell syntax"); err != nil {
		t.Fatal(err)
	}
	arg, err := os.ReadFile(deleteArgs)
	if err != nil || string(arg) != "provider id; not shell syntax" {
		t.Fatalf("delete argument = %q, %v", arg, err)
	}
	instances, err := p.List(context.Background(), "pool-tag")
	if err != nil || len(instances) != 1 || instances[0].ID != "script-1" {
		t.Fatalf("List() = %#v, %v", instances, err)
	}
	arg, err = os.ReadFile(listArg)
	if err != nil || string(arg) != "pool-tag" {
		t.Fatalf("list argument = %q, %v", arg, err)
	}
}
