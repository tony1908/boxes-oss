package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boxes-oss/boxes/internal/provider"
	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/tunnel"
	"github.com/boxes-oss/boxes/internal/types"
)

type reconcileRoundTripper func(*http.Request) (*http.Response, error)

func (f reconcileRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestReconcileDoesNotDeleteBoxWhenLookupFailsTransiently(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, boxID = "nd_reconcile_transient", "bx_reconciletransient"
	var deleted bytes.Buffer
	client := &agentClient{http: &http.Client{Transport: reconcileRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodDelete {
			deleted.WriteString(request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}}
	srv.reconcileGetBox = func(context.Context, string) (store.BoxRecord, error) {
		return store.BoxRecord{}, errors.New("temporary database failure")
	}
	health := agentHealth{}
	health.Boxes = append(health.Boxes, struct {
		BoxID       string `json:"boxId"`
		ContainerID string `json:"containerId"`
		State       string `json:"state"`
	}{BoxID: boxID, State: "running"})
	srv.reconcileNode(context.Background(), nodeID, client, health)
	if deleted.Len() != 0 {
		t.Fatalf("DELETE paths = %q, want no delete for transient lookup failure", deleted.String())
	}
}

func TestReconcileDoesNotDeleteOrphanBoxAndLogsWarning(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, boxID = "nd_reconcile_missing", "bx_reconcilemissing"
	var deleted bytes.Buffer
	client := &agentClient{http: &http.Client{Transport: reconcileRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodDelete {
			deleted.WriteString(request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}}
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousOutput)
	health := agentHealth{}
	health.Boxes = append(health.Boxes, struct {
		BoxID       string `json:"boxId"`
		ContainerID string `json:"containerId"`
		State       string `json:"state"`
	}{BoxID: boxID, State: "running"})
	srv.reconcileNode(context.Background(), nodeID, client, health)
	if deleted.Len() != 0 {
		t.Fatalf("DELETE paths = %q, want no delete for orphan box", deleted.String())
	}
	if !strings.Contains(logs.String(), "orphan container "+boxID+" on node "+nodeID) {
		t.Fatalf("logs = %q, want orphan warning", logs.String())
	}
}

type capturingProvider struct {
	*provider.Fake
	specs []provider.NodeSpec
}

type failingDeleteImageProvider struct {
	*provider.Fake
	err error
}

func (p *failingDeleteImageProvider) DeleteImage(context.Context, string) error { return p.err }

type bakeOrderingProvider struct {
	*provider.Fake
	createImageReturned bool
}

type failedBakeImageProvider struct{ *provider.Fake }

func (p *failedBakeImageProvider) Get(context.Context, string) (provider.Instance, error) {
	return provider.Instance{ID: "bake-instance", Status: "off"}, nil
}
func (p *failedBakeImageProvider) CreateImage(ctx context.Context, instanceID, name string) (string, error) {
	if _, err := p.Fake.CreateImage(ctx, instanceID, name); err != nil {
		return "", err
	}
	return "", errors.New("snapshot action failed after starting")
}

func TestFailedBakeDeletesNewNamedImage(t *testing.T) {
	fake := provider.NewFake()
	cloud := &failedBakeImageProvider{Fake: fake}
	st, err := store.Open(t.TempDir() + "/bake-failed.db")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: st, DataDir: t.TempDir(), ProviderFactory: func(_, _ string) provider.Provider { return cloud }})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	defer func() { srv.Close(); _ = st.Close() }()
	credentials, err := srv.secrets.encrypt([]byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	pool := types.Pool{ID: "pl_bake_cleanup", Name: "workers", Provider: "digitalocean", Region: "nyc3", Size: "s-2vcpu-4gb", Image: "ubuntu"}
	if err := st.CreatePool(context.Background(), store.PoolRecord{Pool: pool, CredentialsEnc: credentials}); err != nil {
		t.Fatal(err)
	}
	if err := srv.bakePoolImage(context.Background(), store.PoolRecord{Pool: pool, CredentialsEnc: credentials}); err == nil {
		t.Fatal("bake unexpectedly succeeded")
	}
	images, err := fake.ListImagesByName(context.Background(), "workers-baked")
	if err != nil || len(images) != 0 {
		t.Fatalf("failed bake images = %v, err %v; want none", images, err)
	}
}

func TestReconcileRecoversBoxFromErrorWhenAgentReportsRunning(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, boxID = "nd_recover_error", "bx_recovererror1"
	seedPlatformBox(t, srv, nodeID, boxID, "recover-node-token", "", "")
	record, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil {
		t.Fatal(err)
	}
	record.Box.State = types.StateError
	record.Box.ErrorCode = "provision_failed"
	record.Box.Error = "agent request timed out"
	if err := srv.store.UpdateBox(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	health := agentHealth{}
	health.Boxes = append(health.Boxes, struct {
		BoxID       string `json:"boxId"`
		ContainerID string `json:"containerId"`
		State       string `json:"state"`
	}{BoxID: boxID, State: "running"})
	srv.reconcileNode(context.Background(), nodeID, nil, health)
	updated, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil || updated.Box.State != types.StateReady || updated.Box.Error != "" || updated.Box.ErrorCode != "" {
		t.Fatalf("reconciled box = %#v, err %v; want ready with cleared error", updated.Box, err)
	}
}

func TestQueuedProvisionSurvivesShortCallbackContextAndSlowAgent(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, boxID, token = "nd_slowqueue", "bx_slowqueue123", "slow-queue-token"
	if err := srv.store.CreateNode(context.Background(), store.NodeRecord{Node: types.Node{ID: nodeID, Name: "slow", Status: "healthy", CPUs: 8, MemoryBytes: 16 << 30, FreeMemoryBytes: 16 << 30}, TokenHash: tokenHash(token)}); err != nil {
		t.Fatal(err)
	}
	box := types.Box{ID: boxID, NodeID: nodeID, Type: "small", State: types.StateProvisioning, Image: "boxes/test", CreatedAt: time.Now().UTC()}
	if err := srv.store.CreateBox(context.Background(), store.BoxRecord{Box: box}); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"boxId": boxID, "type": "small"})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := srv.secrets.encrypt(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreatePendingBox(context.Background(), boxID, encrypted); err != nil {
		t.Fatal(err)
	}
	const agentDelay = 150 * time.Millisecond
	hub := &tunnel.Hub{}
	srv.hub = hub
	httpServer := httptest.NewServer(hub.Handler(func(got string) (string, bool) { return nodeID, got == token }))
	defer httpServer.Close()
	connectCtx, disconnect := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tunnel.Dial(connectCtx, websocketURL(httpServer.URL), token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/v1/boxes" {
				time.Sleep(agentDelay)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if r.URL.Path == "/v1/health" {
				_, _ = io.WriteString(w, `{"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":15032385536,"memoryPressureSomeAvg10":0,"boxes":[]}`)
				return
			}
			http.NotFound(w, r)
		}))
	}()
	defer func() {
		disconnect()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("slow fake agent tunnel did not stop")
		}
	}()
	var client *agentClient
	waitFor(t, func() bool {
		var ok bool
		var httpClient interface{}
		httpClient, ok = hub.Client(nodeID)
		if ok {
			client = &agentClient{http: httpClient.(*http.Client)}
		}
		return ok
	})
	callbackCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	started := time.Now()
	srv.provisionPendingBoxes(callbackCtx, nodeID, client)
	cancel()
	if elapsed := time.Since(started); elapsed < agentDelay {
		t.Fatalf("provision returned after %s, before slow agent response", elapsed)
	}
	updated, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil || updated.Box.State != types.StateReady {
		t.Fatalf("slow queued box = %#v, err %v; want ready", updated.Box, err)
	}
}

func (p *bakeOrderingProvider) Get(context.Context, string) (provider.Instance, error) {
	return provider.Instance{ID: "bake-instance", Status: "off"}, nil
}
func (p *bakeOrderingProvider) CreateImage(context.Context, string, string) (string, error) {
	p.createImageReturned = true
	return "baked-image", nil
}
func (p *bakeOrderingProvider) Delete(_ context.Context, _ string) error {
	if !p.createImageReturned {
		return errors.New("Delete called before CreateImage returned")
	}
	return nil
}

func TestBakeDeletesInstanceOnlyAfterCreateImageReturns(t *testing.T) {
	fake := &bakeOrderingProvider{Fake: provider.NewFake()}
	st, err := store.Open(t.TempDir() + "/bake-order.db")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: st, DataDir: t.TempDir(), PublicURL: "http://boxes.example.test", ProviderFactory: func(_, _ string) provider.Provider { return fake }})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	defer func() { srv.Close(); _ = st.Close() }()
	credentials, err := srv.secrets.encrypt([]byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	pool := types.Pool{ID: "pl_bake_order", Name: "workers", Provider: "digitalocean", Region: "nyc3", Size: "s-2vcpu-4gb", Image: "ubuntu", MaxNodes: 1, BakedBoxDigest: "sha256:box", BakedToolsDigest: "sha256:tools"}
	if err := st.CreatePool(context.Background(), store.PoolRecord{Pool: pool, CredentialsEnc: credentials}); err != nil {
		t.Fatal(err)
	}
	if err := srv.bakePoolImage(context.Background(), store.PoolRecord{Pool: pool, CredentialsEnc: credentials}); err != nil {
		t.Fatal(err)
	}
	if !fake.createImageReturned {
		t.Fatal("CreateImage was not called")
	}
}

func (p *capturingProvider) Create(ctx context.Context, spec provider.NodeSpec) (provider.Instance, error) {
	p.specs = append(p.specs, spec)
	return p.Fake.Create(ctx, spec)
}

func TestPoolAPIIsOwnerOnlyAndNeverReturnsCredentials(t *testing.T) {
	fake := &capturingProvider{Fake: provider.NewFake()}
	st, err := store.Open(t.TempDir() + "/pools.db")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: st, DataDir: t.TempDir(), PublicURL: "https://boxes.example.test", ProviderFactory: func(_, _ string) provider.Provider { return fake }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { srv.Close(); _ = st.Close() }()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{"email": "owner@pools.test", "password": "a long test password"}, http.StatusCreated, nil)
	var login map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/login", "", map[string]any{"email": "owner@pools.test", "password": "a long test password"}, http.StatusOK, &login)
	token := login["token"].(string)
	input := map[string]any{"name": "workers", "provider": "digitalocean", "credentials": "secret-provider-token", "region": "nyc3", "size": "s-2vcpu-4gb", "image": "ubuntu-24-04-x64", "minNodes": 0, "maxNodes": 2, "idleMinutes": 10}
	var created map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/pools", token, input, http.StatusCreated, &created)
	encoded, _ := json.Marshal(created)
	if strings.Contains(string(encoded), "secret-provider-token") {
		t.Fatalf("pool API exposed credentials: %s", encoded)
	}
	pool, ok := created["pool"].(map[string]any)
	if !ok || pool["hasCredentials"] != true || pool["provider"] != "digitalocean" {
		t.Fatalf("created pool = %#v", created)
	}
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/pools", token, nil, http.StatusOK, &created)
	encoded, _ = json.Marshal(created)
	if strings.Contains(string(encoded), "secret-provider-token") {
		t.Fatalf("pool list exposed credentials: %s", encoded)
	}
	id := pool["id"].(string)
	imageID, err := fake.CreateImage(context.Background(), "bake-instance", "workers-baked")
	if err != nil {
		t.Fatal(err)
	}
	record, err := st.GetPool(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	record.Pool.BakedImageID = imageID
	if err := st.UpdatePool(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	var updated map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/pools/"+id, token, map[string]any{"minNodes": 0}, http.StatusOK, &updated)
	if updated["pool"].(map[string]any)["minNodes"] != float64(0) {
		t.Fatalf("pool update did not accept minNodes=0: %#v", updated)
	}
	memberToken := "member-session-token"
	memberID := "usr_poolmember"
	if err := st.CreateUser(context.Background(), types.User{ID: memberID, Email: "member@pools.test", Role: "member", PasswordHash: "unused", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(context.Background(), store.Session{ID: tokenHash(memberToken), UserID: memberID, ExpiresAt: time.Now().UTC().Add(time.Hour), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/pools", memberToken, nil, http.StatusForbidden, nil)
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/pools", "", nil, http.StatusUnauthorized, nil)
	detachedWithBox := types.Node{ID: "nd_detached_delete", Name: "retired", Status: "offline"}
	if err := st.CreateNode(context.Background(), store.NodeRecord{Node: detachedWithBox}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateBox(context.Background(), store.BoxRecord{Box: types.Box{ID: "bx_detached_delete", NodeID: detachedWithBox.ID, Type: "small", State: types.StateStopped, CreatedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	requestJSON(t, httpServer.Client(), http.MethodDelete, httpServer.URL+"/api/v1/sandboxes/bx_detached_delete", token, nil, http.StatusNoContent, nil)
	if _, err := st.GetNode(context.Background(), detachedWithBox.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("detached node after box deletion = %v, want not found", err)
	}
	detached := types.Node{ID: "nd_detached_after_pool", Name: "retired", Status: "offline"}
	if err := st.CreateNode(context.Background(), store.NodeRecord{Node: detached}); err != nil {
		t.Fatal(err)
	}
	requestJSON(t, httpServer.Client(), http.MethodDelete, httpServer.URL+"/api/v1/pools/"+id, token, nil, http.StatusNoContent, nil)
	images, err := fake.ListImagesByName(context.Background(), "workers-baked")
	if err != nil || len(images) != 0 {
		t.Fatalf("baked images after pool deletion = %v, err %v; want none", images, err)
	}
	if _, err := st.GetNode(context.Background(), detached.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("detached node after pool deletion = %v, want not found", err)
	}
}

func TestDeletePoolContinuesWhenBakedImageDeletionFails(t *testing.T) {
	fake := provider.NewFake()
	cloud := &failingDeleteImageProvider{Fake: fake, err: errors.New("provider image cleanup failed")}
	st, err := store.Open(t.TempDir() + "/pool-image-delete-failure.db")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: st, DataDir: t.TempDir(), ProviderFactory: func(_, _ string) provider.Provider { return cloud }})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	defer func() { srv.Close(); _ = st.Close() }()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{"email": "image-delete@pools.test", "password": "a long test password"}, http.StatusCreated, nil)
	var login map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/login", "", map[string]any{"email": "image-delete@pools.test", "password": "a long test password"}, http.StatusOK, &login)
	token := login["token"].(string)
	credentials, err := srv.secrets.encrypt([]byte("provider-token"))
	if err != nil {
		t.Fatal(err)
	}
	imageID, err := fake.CreateImage(context.Background(), "bake-instance", "workers-baked")
	if err != nil {
		t.Fatal(err)
	}
	pool := types.Pool{ID: "pl_image_delete_failure", Name: "workers", Provider: "digitalocean", Region: "nyc3", Size: "s-2vcpu-4gb", Image: "ubuntu", BakedImageID: imageID}
	if err := st.CreatePool(context.Background(), store.PoolRecord{Pool: pool, CredentialsEnc: credentials}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousOutput)
	requestJSON(t, httpServer.Client(), http.MethodDelete, httpServer.URL+"/api/v1/pools/"+pool.ID, token, nil, http.StatusNoContent, nil)
	if _, err := st.GetPool(context.Background(), pool.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("pool after image deletion failure = %v, want not found", err)
	}
	if !strings.Contains(logs.String(), "delete baked image "+imageID+" for pool "+pool.ID+" failed") {
		t.Fatalf("logs = %q, want baked image deletion failure", logs.String())
	}
}

func TestCloudInitContainsInstallerAndStorageButOnlyConfiguredRegistry(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()
	userdata, err := srv.poolUserData(ctx, "join-token-one-time")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"#cloud-config", "runcmd:", "http://boxes.example.test/install/node-install.sh", "BOXES_SERVER_URL='ws://boxes.example.test/api/v1/agent/connect'", "BOXES_PUBLIC_URL='http://boxes.example.test'", "BOXES_NODE_TOKEN='join-token-one-time'", "RESTIC_REPOSITORY='/var/lib/boxes/restic'"} {
		if !strings.Contains(userdata, expected) {
			t.Errorf("cloud-init missing %q:\n%s", expected, userdata)
		}
	}
	if strings.Contains(userdata, "BOXES_REGISTRY_") {
		t.Fatalf("cloud-init included unconfigured registry settings:\n%s", userdata)
	}
	if err := srv.saveRegistry(ctx, registryConfig{URL: "registry.example.test", Username: "boxes", Password: "secret"}); err != nil {
		t.Fatal(err)
	}
	userdata, err = srv.poolUserData(ctx, "join-token-one-time")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"BOXES_REGISTRY_URL='registry.example.test'", "BOXES_REGISTRY_USERNAME='boxes'", "BOXES_REGISTRY_PASSWORD='secret'"} {
		if !strings.Contains(userdata, expected) {
			t.Errorf("configured cloud-init missing %q:\n%s", expected, userdata)
		}
	}
}

func TestBakeCloudInitPassesImagesRegistryAndReportToken(t *testing.T) {
	userdata := bakePoolUserData("https://boxes.example.test/", "pl_workers", "one-time-token", defaultImageConfig{Box: "registry.example.test/boxes/box:v1", Tools: "registry.example.test/boxes/tools:v1"}, registryConfig{URL: "registry.example.test", Username: "boxes", Password: "secret"})
	for _, expected := range []string{"#cloud-config", "BOXES_NODE_MODE=bake", "BOXES_PUBLIC_URL='https://boxes.example.test'", "BOXES_BAKE_REPORT_URL='https://boxes.example.test/api/v1/pools/pl_workers/bake-report'", "BOXES_BAKE_TOKEN='one-time-token'", "BOXES_BOX_IMAGE='registry.example.test/boxes/box:v1'", "BOXES_TOOLS_IMAGE='registry.example.test/boxes/tools:v1'", "BOXES_REGISTRY_USERNAME='boxes'", "BOXES_REGISTRY_PASSWORD='secret'"} {
		if !strings.Contains(userdata, expected) {
			t.Errorf("bake cloud-init missing %q:\n%s", expected, userdata)
		}
	}
}

func TestBakeReportRequiresOneTimeTokenAndStoresDigests(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	pool := types.Pool{ID: "pl_bake_report", Name: "workers", Provider: "digitalocean", Region: "nyc3", Size: "s-2vcpu-4gb", Image: "ubuntu", MaxNodes: 1, BakeStatus: "baking"}
	if err := srv.store.CreatePool(context.Background(), store.PoolRecord{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	const token = "report-once"
	srv.bakeReportMu.Lock()
	srv.bakeReportTokens = map[string]string{pool.ID: tokenHash(token)}
	srv.bakeReportMu.Unlock()
	r := httptest.NewRequest(http.MethodPost, "/pools/"+pool.ID+"/bake-report", strings.NewReader(`{"boxDigest":"registry.example/box@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","toolsDigest":"registry.example/tools@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`))
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	srv.APIHandler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", w.Code)
	}
	r = httptest.NewRequest(http.MethodPost, "/pools/"+pool.ID+"/bake-report", strings.NewReader(`{"boxDigest":"registry.example/box@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","toolsDigest":"registry.example/tools@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	srv.APIHandler().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("valid report status = %d: %s", w.Code, w.Body.String())
	}
	stored, err := srv.store.GetPool(context.Background(), pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Pool.BakedBoxDigest == "" || stored.Pool.BakedToolsDigest == "" {
		t.Fatalf("digests not stored: %#v", stored.Pool)
	}
}

func TestCapacitySchedulesNewPoolNodeAndEncryptsWaitingBoxPayload(t *testing.T) {
	fake := &capturingProvider{Fake: provider.NewFake()}
	st, err := store.Open(t.TempDir() + "/autoscale.db")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: st, DataDir: t.TempDir(), PublicURL: "https://boxes.example.test", Image: "boxes/box:test", ProviderFactory: func(_, _ string) provider.Provider { return fake }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { srv.Close(); _ = st.Close() }()
	credentials, err := srv.secrets.encrypt([]byte("test-token"))
	if err != nil {
		t.Fatal(err)
	}
	pool := types.Pool{ID: "pl_autoscale", Name: "workers", Provider: "digitalocean", Region: "nyc3", Size: "s-2vcpu-4gb", Image: "ubuntu", MaxNodes: 1, IdleMinutes: 10, BootTimeoutMinutes: 15}
	if err := st.CreatePool(context.Background(), store.PoolRecord{Pool: pool, CredentialsEnc: credentials}); err != nil {
		t.Fatal(err)
	}
	box, err := srv.provisionSandbox(context.Background(), createSandboxRequest{Type: "small"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if box.State != types.StateProvisioning || box.WaitingFor != "node" {
		t.Fatalf("queued box = %#v", box)
	}
	persisted, err := st.GetBox(context.Background(), box.ID)
	if err != nil || persisted.Box.WaitingFor != "node" {
		t.Fatalf("persisted waiting box = %#v, %v", persisted.Box, err)
	}
	queued, err := st.GetPendingBox(context.Background(), box.ID)
	if err != nil {
		t.Fatalf("GetPendingBox(): %v", err)
	}
	if strings.Contains(string(queued), "test-token") {
		t.Fatal("queued payload leaked provider credentials")
	}
	plain, err := srv.secrets.decrypt(queued)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(plain, &payload); err != nil || payload["boxId"] != box.ID {
		t.Fatalf("queued payload = %s, %v", plain, err)
	}
	nodes, err := st.ListNodes(context.Background())
	if err != nil || len(nodes) != 1 || nodes[0].Node.PoolID != pool.ID || nodes[0].Node.ProviderInstanceID == "" {
		t.Fatalf("pool nodes = %#v, %v", nodes, err)
	}
	if len(fake.specs) != 1 || !strings.Contains(fake.specs[0].UserData, "#cloud-config") || fake.specs[0].Tags[0] != poolTag(pool.ID) {
		t.Fatalf("provider create spec = %#v", fake.specs)
	}
	second, err := srv.provisionSandbox(context.Background(), createSandboxRequest{Type: "small"}, "", "")
	if err != nil {
		t.Fatalf("second queued sandbox: %v", err)
	}
	if second.WaitingFor != "node" || second.NodeID != box.NodeID || len(fake.specs) != 1 {
		t.Fatalf("pending boxes did not share the new node: first=%#v second=%#v creates=%d", box, second, len(fake.specs))
	}
}

func TestResumeMigrationQueuesRestoreWhenOriginalPoolNodeIsGone(t *testing.T) {
	fake := &capturingProvider{Fake: provider.NewFake()}
	st, err := store.Open(t.TempDir() + "/resume-pool.db")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: st, DataDir: t.TempDir(), PublicURL: "https://boxes.example.test", ProviderFactory: func(_, _ string) provider.Provider { return fake }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { srv.Close(); _ = st.Close() }()
	ctx := context.Background()
	credentials, err := srv.secrets.encrypt([]byte("test-token"))
	if err != nil {
		t.Fatal(err)
	}
	pool := types.Pool{ID: "pl_resume", Name: "workers", Provider: "hetzner", Region: "fsn1", Size: "cx22", Image: "ubuntu", MaxNodes: 1, IdleMinutes: 10, BootTimeoutMinutes: 15}
	if err := st.CreatePool(ctx, store.PoolRecord{Pool: pool, CredentialsEnc: credentials}); err != nil {
		t.Fatal(err)
	}
	oldNode := types.Node{ID: "nd_oldpool", Name: "retired", Status: "offline"}
	if err := st.CreateNode(ctx, store.NodeRecord{Node: oldNode}); err != nil {
		t.Fatal(err)
	}
	stoppedAt := time.Now().UTC()
	password, _ := srv.secrets.encrypt([]byte("desktop"))
	boxToken, _ := srv.secrets.encrypt([]byte("box-token"))
	box := store.BoxRecord{Box: types.Box{ID: "bx_resumequeue", NodeID: oldNode.ID, Type: "small", State: types.StateResuming, Image: "boxes/box:test", CreatedAt: stoppedAt}, DesktopPasswordEnc: password, BoxdTokenEnc: boxToken}
	if err := st.CreateBox(ctx, box); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSnapshot(ctx, types.Snapshot{ID: "sn_resumequeue", BoxID: box.Box.ID, ResticID: "restic-snapshot", CreatedAt: stoppedAt}); err != nil {
		t.Fatal(err)
	}
	if err := srv.migrateBox(ctx, &box, struct {
		Type string `json:"type"`
		Node string `json:"node"`
	}{}); err != nil {
		t.Fatal(err)
	}
	if box.Box.State != types.StateProvisioning || box.Box.WaitingFor != "node" || box.Box.NodeID == oldNode.ID {
		t.Fatalf("queued resume box = %#v", box.Box)
	}
	if _, err := st.GetPendingBox(ctx, box.Box.ID); err != nil {
		t.Fatalf("GetPendingBox(): %v", err)
	}
	if _, err := st.GetNode(ctx, oldNode.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("detached source node after box migration = %v, want not found", err)
	}
}

func TestReconcileRemovesUnreferencedDetachedOfflineNodes(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()
	node := types.Node{ID: "nd_reconcile_orphan", Name: "retired", Status: "offline"}
	if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: node}); err != nil {
		t.Fatal(err)
	}
	retained := types.Node{ID: "nd_reconcile_referenced", Name: "retired-with-box", Status: "offline"}
	if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: retained}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: types.Box{ID: "bx_reconcile_referenced", NodeID: retained.ID, State: types.StateStopped}}); err != nil {
		t.Fatal(err)
	}

	srv.reconcilePoolsOnce(ctx)

	if _, err := srv.store.GetNode(ctx, node.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unreferenced detached node after reconcile = %v, want not found", err)
	}
	if _, err := srv.store.GetNode(ctx, retained.ID); err != nil {
		t.Fatalf("referenced detached node after reconcile = %v, want node retained", err)
	}
}

func TestPoolScaleDownDestroysOnlyIdleNodesAboveMinimum(t *testing.T) {
	fake := &capturingProvider{Fake: provider.NewFake()}
	st, err := store.Open(t.TempDir() + "/scale-down.db")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: st, DataDir: t.TempDir(), ProviderFactory: func(_, _ string) provider.Provider { return fake }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { srv.Close(); _ = st.Close() }()
	ctx := context.Background()
	credentials, _ := srv.secrets.encrypt([]byte("token"))
	pool := types.Pool{ID: "pl_scale", Name: "workers", Provider: "digitalocean", Region: "nyc3", Size: "s-2vcpu-4gb", Image: "ubuntu", MinNodes: 1, MaxNodes: 2, IdleMinutes: 1, BootTimeoutMinutes: 15}
	if err := st.CreatePool(ctx, store.PoolRecord{Pool: pool, CredentialsEnc: credentials}); err != nil {
		t.Fatal(err)
	}
	first, err := srv.createPoolNode(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := srv.createPoolNode(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if scaleDownPreservesHeadroom(ctx, srv, pool.ID, first.Node.ID, 1) != true {
		t.Fatal("remaining provisioning node should preserve one headroom slot")
	}
	if scaleDownPreservesHeadroom(ctx, srv, pool.ID, first.Node.ID, 11) != false {
		t.Fatal("scale-down must protect configured headroom")
	}
	past := time.Now().UTC().Add(-2 * time.Minute)
	for _, node := range []store.NodeRecord{first, second} {
		node.Node.IdleSince = &past
		if err := st.UpdateNode(ctx, node); err != nil {
			t.Fatal(err)
		}
	}
	srv.scaleDownOnce(ctx)
	instances, err := fake.List(ctx, poolTag(pool.ID))
	if err != nil || len(instances) != 1 {
		t.Fatalf("provider instances after scale-down = %#v, %v", instances, err)
	}
	nodes, err := st.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	counted := 0
	for _, node := range nodes {
		if node.Node.PoolID == pool.ID && countedPoolNode(node.Node) {
			counted++
		}
	}
	if counted != 1 {
		t.Fatalf("counted pool nodes = %d, want minNodes=1", counted)
	}
}

func TestWarmScheduleUsesIANAClockAndRaisesMinimum(t *testing.T) {
	schedule := "Mon-Fri 09:00-19:00 America/Mexico_City min=2"
	if _, err := parseWarmSchedule(schedule); err != nil {
		t.Fatal(err)
	}
	fakeNow := func() time.Time { return time.Date(2026, 9, 28, 15, 0, 0, 0, time.UTC) } // Monday, 09:00 in Mexico City
	inside := fakeNow()
	if got := effectivePoolMinimum(1, schedule, inside); got != 2 {
		t.Fatalf("inside minimum=%d", got)
	}
	if got := effectivePoolMinimum(3, schedule, inside); got != 3 {
		t.Fatalf("minimum lowered: %d", got)
	}
	if got := effectivePoolMinimum(1, schedule, inside.Add(-time.Minute)); got != 1 {
		t.Fatalf("outside minimum=%d", got)
	}
	for _, invalid := range []string{"Mon-Fri 09:00-19:00 Not/AZone min=1", "Mon-Fri 25:00-19:00 UTC min=1", "Mon-Fri 09:00-19:00 UTC min=-1", "Mon-Fri 09:00-19:00 UTC"} {
		if _, err := parseWarmSchedule(invalid); err == nil {
			t.Errorf("accepted invalid schedule %q", invalid)
		}
	}
}

func TestPoolCreateAndUpdateValidateWarmSchedule(t *testing.T) {
	min, max, idle := 0, 2, 10
	base := poolInput{Name: "workers", Provider: "script", Credentials: "provider", Region: "local", Size: "small", Image: "ubuntu", MinNodes: &min, MaxNodes: &max, IdleMinutes: &idle}
	bad := "Mon-Fri 09:00-19:00 Mars/Olympus min=1"
	base.WarmSchedule = &bad
	if _, _, err := normalizePoolInput(base, nil); err == nil {
		t.Fatal("create accepted invalid warm schedule")
	}
	good := "Mon-Fri 09:00-19:00 America/Mexico_City min=1"
	base.WarmSchedule = &good
	current, _, err := normalizePoolInput(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	update := poolInput{WarmSchedule: &bad}
	if _, _, err := normalizePoolInput(update, &current); err == nil {
		t.Fatal("update accepted invalid warm schedule")
	}
}

func TestReconcileMarksMissingInstancesLostAndBoxesUnreachable(t *testing.T) {
	fake := &capturingProvider{Fake: provider.NewFake()}
	st, err := store.Open(t.TempDir() + "/reconcile.db")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: st, DataDir: t.TempDir(), ProviderFactory: func(_, _ string) provider.Provider { return fake }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { srv.Close(); _ = st.Close() }()
	ctx := context.Background()
	credentials, _ := srv.secrets.encrypt([]byte("token"))
	pool := types.Pool{ID: "pl_reconcile", Name: "workers", Provider: "hetzner", Region: "fsn1", Size: "cx22", Image: "ubuntu", MaxNodes: 1, IdleMinutes: 10, BootTimeoutMinutes: 15}
	if err := st.CreatePool(ctx, store.PoolRecord{Pool: pool, CredentialsEnc: credentials}); err != nil {
		t.Fatal(err)
	}
	node, err := srv.createPoolNode(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateBox(ctx, store.BoxRecord{Box: types.Box{ID: "bx_orphaned", NodeID: node.Node.ID, Type: "small", State: types.StateReady, Image: "boxes/box:test", CreatedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	if err := fake.Delete(ctx, node.Node.ProviderInstanceID); err != nil {
		t.Fatal(err)
	}
	srv.reconcilePoolsOnce(ctx)
	updatedNode, err := st.GetNode(ctx, node.Node.ID)
	if err != nil || updatedNode.Node.Status != "lost" {
		t.Fatalf("reconciled node = %#v, %v", updatedNode.Node, err)
	}
	updatedBox, err := st.GetBox(ctx, "bx_orphaned")
	if err != nil || updatedBox.Box.State != types.StateUnreachable {
		t.Fatalf("reconciled box = %#v, %v", updatedBox.Box, err)
	}
}

func TestBootTimeoutDeletesInstanceAndMarksWaitingBoxesNoCapacity(t *testing.T) {
	fake := &capturingProvider{Fake: provider.NewFake()}
	st, err := store.Open(t.TempDir() + "/boot-timeout.db")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: st, DataDir: t.TempDir(), ProviderFactory: func(_, _ string) provider.Provider { return fake }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { srv.Close(); _ = st.Close() }()
	ctx := context.Background()
	credentials, _ := srv.secrets.encrypt([]byte("token"))
	pool := types.Pool{ID: "pl_timeout", Name: "workers", Provider: "digitalocean", Region: "nyc3", Size: "s-2vcpu-4gb", Image: "ubuntu", MaxNodes: 1, IdleMinutes: 10, BootTimeoutMinutes: 1}
	if err := st.CreatePool(ctx, store.PoolRecord{Pool: pool, CredentialsEnc: credentials}); err != nil {
		t.Fatal(err)
	}
	node, err := srv.createPoolNode(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	box := types.Box{ID: "bx_timeout", NodeID: node.Node.ID, Type: "small", State: types.StateProvisioning, Image: "boxes/box:test", CreatedAt: time.Now().UTC(), WaitingFor: "node"}
	if err := st.CreateBox(ctx, store.BoxRecord{Box: box}); err != nil {
		t.Fatal(err)
	}
	payload, _ := srv.secrets.encrypt([]byte(`{"boxId":"bx_timeout"}`))
	if err := st.CreatePendingBox(ctx, box.ID, payload); err != nil {
		t.Fatal(err)
	}
	node.Node.CreatedAt = time.Now().UTC().Add(-2 * time.Minute)
	if err := st.UpdateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	srv.expireProvisioningNodes(ctx, []store.NodeRecord{node})
	updated, err := st.GetBox(ctx, box.ID)
	if err != nil || updated.Box.State != types.StateError || updated.Box.ErrorCode != "no_capacity" {
		t.Fatalf("timed out box = %#v, %v", updated.Box, err)
	}
	instances, err := fake.List(ctx, poolTag(pool.ID))
	if err != nil || len(instances) != 0 {
		t.Fatalf("instances after boot timeout = %#v, %v", instances, err)
	}
}

func TestDestroyedPoolNodeCanBeCleanedUpAfterStoppedBoxDeletion(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/detached-delete.db")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: st, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { srv.Close(); _ = st.Close() }()
	ctx := context.Background()
	node := types.Node{ID: "nd_destroyed", Name: "retired", Status: "destroyed"}
	if err := st.CreateNode(ctx, store.NodeRecord{Node: node}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	box := store.BoxRecord{Box: types.Box{ID: "bx_stopped_retired", NodeID: node.ID, Type: "small", State: types.StateStopped, Image: "boxes/box:test", CreatedAt: now, StoppedAt: &now}}
	if err := st.CreateBox(ctx, box); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSnapshot(ctx, types.Snapshot{ID: "sn_retired", BoxID: box.Box.ID, ResticID: "snapshot-retired", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := srv.deleteDetachedSandbox(ctx, box); err != nil {
		t.Fatalf("deleteDetachedSandbox(): %v", err)
	}
	if _, err := st.GetBox(ctx, box.Box.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetBox() after delete = %v, want not found", err)
	}
	if _, err := st.GetNode(ctx, node.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetNode() after cleanup = %v, want not found", err)
	}
}
