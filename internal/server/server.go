// Package server implements the boxes control-plane HTTP API.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tony1908/boxes-oss/install"
	"github.com/tony1908/boxes-oss/internal/diagnostics"
	"github.com/tony1908/boxes-oss/internal/store"
	"github.com/tony1908/boxes-oss/internal/tunnel"
	"github.com/tony1908/boxes-oss/internal/types"
	"github.com/tony1908/boxes-oss/internal/webui"
)

// Config contains the persistent and public-facing server settings.
type Config struct {
	Store                  *store.Store
	DataDir                string
	PublicURL              string
	Image                  string
	ToolsImage             string
	SnapshotInterval       time.Duration
	SnapshotBusyInterval   time.Duration
	DesktopSessionTTL      time.Duration
	MemoryOvercommit       float64
	AdmissionMode          string
	AdmissionBufferPercent float64
	AdmissionBufferMin     int64
	AdmissionExpectedBox   int64
	AdmissionPSIMax        float64
	AdmissionSwapAllowance int64
	BootstrapJoinToken     string
	BootstrapNodeName      string
	BootstrapHostBase      string
	StorageBootstrap       StorageBootstrap
}

// Server owns the API handlers and the agent tunnel hub.
type Server struct {
	store                *store.Store
	hub                  *tunnel.Hub
	secrets              *secretBox
	adminToken           string
	storageMu            sync.RWMutex
	storage              storageConfig
	imageMu              sync.RWMutex
	images               defaultImageConfig
	cfg                  Config
	api                  http.Handler
	agent                http.Handler
	platformMu           sync.Mutex
	desktopRoutes        map[string]desktopRouteState
	desktopSessions      map[string]desktopSession
	eventPumps           map[string]eventPump
	eventCursors         map[string]int64
	eventPumpLogAt       map[string]time.Time
	webhookQueue         chan webhookEventDelivery
	webhookCancel        context.CancelFunc
	lifecycleCtx         context.Context
	lifecycleCancel      context.CancelFunc
	eventWG              sync.WaitGroup
	snapshotMu           sync.Mutex
	snapshotBusy         map[string]chan struct{}
	snapshotRefs         sync.Mutex
	pruneMu              sync.Mutex
	pruneRetryAt         map[string]time.Time
	pruneLastAt          map[string]time.Time // In-memory throttle resets when the server restarts.
	closed               bool
	now                  func() time.Time
	snapshotChanged      map[string]int
	snapshotNextAt       map[string]time.Time
	orphanLogMu          sync.Mutex
	orphanLogAt          map[string]time.Time
	operationMu          sync.RWMutex
	operations           map[string]operationStatus
	apiUsageMu           sync.Mutex
	apiUsagePending      map[string]int64
	healthMu             sync.RWMutex
	healthSnapshots      map[string]healthSnapshot
	idleCPUMu            sync.Mutex
	idleCPUSamples       map[string][]idleCPUSample
	reconcileGetBox      func(context.Context, string) (store.BoxRecord, error)
	authLimitMu          sync.Mutex
	authIPAttempts       map[string][]time.Time
	authEmailFails       map[string][]time.Time
	nodeHostBasesMu      sync.Mutex
	nodeHostBases        []string
	nodeHostBasesExpires time.Time
}

type contextKey uint8

const (
	userContextKey contextKey = iota
	authTokenContextKey
	activityActorContextKey
)

const sessionCookieName = "boxes_session"

// New configures the HTTP API, node tunnel, and encrypted secret storage.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("server store is required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("server data directory is required")
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = "http://localhost:8080"
	}
	if cfg.Image == "" {
		cfg.Image = "ghcr.io/tony1908/boxes-box:latest"
	}
	if cfg.ToolsImage == "" {
		cfg.ToolsImage = "ghcr.io/tony1908/boxes-tools:latest"
	}
	if cfg.ToolsImage == "" {
		cfg.ToolsImage = "ghcr.io/tony1908/boxes-tools:latest"
	}
	if cfg.DesktopSessionTTL <= 0 {
		cfg.DesktopSessionTTL = 8 * time.Hour
	}
	if cfg.SnapshotInterval <= 0 {
		cfg.SnapshotInterval = time.Minute
	}
	if cfg.SnapshotBusyInterval <= 0 {
		cfg.SnapshotBusyInterval = 5 * time.Minute
	}
	if cfg.AdmissionMode == "" {
		cfg.AdmissionMode = "usage"
	}
	if cfg.AdmissionBufferPercent == 0 {
		cfg.AdmissionBufferPercent = 0.15
	}
	if cfg.AdmissionBufferMin == 0 {
		cfg.AdmissionBufferMin = 1 << 30
	}
	if cfg.AdmissionExpectedBox == 0 {
		cfg.AdmissionExpectedBox = 300 << 20
	}
	if cfg.AdmissionPSIMax == 0 {
		cfg.AdmissionPSIMax = 10
	}
	if cfg.AdmissionSwapAllowance == 0 {
		cfg.AdmissionSwapAllowance = 1 << 30
	}
	if cfg.MemoryOvercommit == 0 {
		cfg.MemoryOvercommit = 1.0
	}
	if cfg.MemoryOvercommit < 0 || math.IsNaN(cfg.MemoryOvercommit) || math.IsInf(cfg.MemoryOvercommit, 0) {
		return nil, errors.New("memory overcommit must be a finite positive number")
	}
	if err := ensureBootstrapNode(cfg); err != nil {
		return nil, err
	}
	secrets, err := openSecretBox(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	adminToken, err := openAdminToken(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	webhookCtx, webhookCancel := context.WithCancel(context.Background())
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	s := &Server{store: cfg.Store, hub: &tunnel.Hub{}, secrets: secrets, adminToken: adminToken, cfg: cfg,
		now:           time.Now,
		desktopRoutes: make(map[string]desktopRouteState), desktopSessions: make(map[string]desktopSession), eventPumps: make(map[string]eventPump),
		eventCursors: make(map[string]int64), eventPumpLogAt: make(map[string]time.Time),
		webhookQueue: make(chan webhookEventDelivery, 128), webhookCancel: webhookCancel,
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		operations: make(map[string]operationStatus), apiUsagePending: make(map[string]int64), authIPAttempts: make(map[string][]time.Time), authEmailFails: make(map[string][]time.Time),
		snapshotBusy: make(map[string]chan struct{}), snapshotChanged: make(map[string]int), snapshotNextAt: make(map[string]time.Time), pruneRetryAt: make(map[string]time.Time), pruneLastAt: make(map[string]time.Time), healthSnapshots: make(map[string]healthSnapshot), idleCPUSamples: make(map[string][]idleCPUSample)}
	if err := s.initializeStorage(cfg.StorageBootstrap); err != nil {
		return nil, err
	}
	if err := s.initializeDefaultImages(cfg.Image, cfg.ToolsImage); err != nil {
		return nil, err
	}
	if err := s.loadDesktopSessions(context.Background()); err != nil {
		return nil, err
	}
	s.agent = s.hub.Handler(s.authenticateNodeToken)
	s.hub.OnConnect = s.nodeConnected
	s.hub.OnDisconnect = s.nodeDisconnected
	s.api = s.routes()
	go s.runWebhookQueue(webhookCtx)
	s.eventWG.Add(1)
	go func() { defer s.eventWG.Done(); s.flushAPIKeyUsageLoop(lifecycleCtx) }()
	go s.runActivityPruner(lifecycleCtx)
	return s, nil
}

func ensureBootstrapNode(cfg Config) error {
	if cfg.BootstrapJoinToken == "" {
		return nil
	}
	hash := tokenHash(cfg.BootstrapJoinToken)
	nodes, err := cfg.Store.ListNodes(context.Background())
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if len(node.TokenHash) == len(hash) && subtle.ConstantTimeCompare([]byte(node.TokenHash), []byte(hash)) == 1 {
			return nil
		}
	}
	name := strings.TrimSpace(cfg.BootstrapNodeName)
	if name == "" {
		name = "local"
	}
	return cfg.Store.CreateNode(context.Background(), store.NodeRecord{
		Node:      types.Node{ID: types.NewID("nd_"), Name: name, HostBase: cfg.BootstrapHostBase, Status: "offline"},
		TokenHash: hash,
	})
}

// Handler serves the REST API and tunnel, falling back to the embedded web UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", http.StripPrefix("/api/v1", s.APIHandler()))
	mux.Handle("GET /agent/connect", s.AgentHandler())
	mux.Handle("GET /api/v1/agent/connect", s.AgentHandler())
	mux.HandleFunc("GET /install/traefik.yml", func(w http.ResponseWriter, r *http.Request) {
		serveInstallAsset(w, install.TraefikConfig)
	})
	mux.HandleFunc("GET /install/cli.sh", s.serveCLIInstallScript)
	mux.HandleFunc("GET /install/cli.ps1", s.serveCLIPowerShellInstallScript)
	mux.HandleFunc("GET /install/cli/{binary}", s.serveCLIBinary)
	mux.Handle("/", webui.Handler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, r, s.cfg.PublicURL, s.cachedNodeHostBases())
		mux.ServeHTTP(w, r)
	})
}

const nodeHostBasesCacheTTL = 30 * time.Second

func (s *Server) cachedNodeHostBases() []string {
	s.nodeHostBasesMu.Lock()
	defer s.nodeHostBasesMu.Unlock()

	now := s.now()
	if now.Before(s.nodeHostBasesExpires) {
		return append([]string(nil), s.nodeHostBases...)
	}

	nodes, err := s.store.ListNodes(context.Background())
	if err == nil {
		seen := make(map[string]struct{}, len(nodes))
		bases := make([]string, 0, len(nodes))
		for _, node := range nodes {
			base := sanitizeHostBase(node.Node.HostBase)
			if base == "" {
				continue
			}
			if _, ok := seen[base]; ok {
				continue
			}
			seen[base] = struct{}{}
			bases = append(bases, base)
		}
		s.nodeHostBases = bases
	}
	// Keep the most recently known bases on a transient store error, and avoid
	// querying SQLite for every static asset while it remains unavailable.
	s.nodeHostBasesExpires = now.Add(nodeHostBasesCacheTTL)
	return append([]string(nil), s.nodeHostBases...)
}

func (s *Server) serveCLIInstallScript(w http.ResponseWriter, _ *http.Request) {
	script := strings.ReplaceAll(string(install.CLIScript), "__BOXES_PUBLIC_URL__", strings.TrimRight(s.cfg.PublicURL, "/"))
	serveInstallAsset(w, []byte(script))
}

func (s *Server) serveCLIPowerShellInstallScript(w http.ResponseWriter, _ *http.Request) {
	script := strings.ReplaceAll(string(install.CLIPowerShellScript), "__BOXES_PUBLIC_URL__", strings.TrimRight(s.cfg.PublicURL, "/"))
	serveInstallAsset(w, []byte(script))
}

func (s *Server) serveCLIBinary(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("binary")
	switch name {
	case "boxes-darwin-arm64", "boxes-darwin-amd64", "boxes-linux-amd64", "boxes-linux-arm64", "boxes-windows-amd64.exe", "boxes-windows-arm64.exe":
	default:
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "cli", name))
	if errors.Is(err, os.ErrNotExist) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, "CLI binary %s is not installed on this server; ask the administrator to place it in %s/cli/.\n", name, s.cfg.DataDir)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cli_binary_unavailable", "CLI binary could not be read")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func serveInstallAsset(w http.ResponseWriter, asset []byte) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(asset)
}

// APIHandler returns the API mux mounted below /api/v1.
func (s *Server) APIHandler() http.Handler { return s.api }

// AgentHandler returns the authenticated reverse-tunnel WebSocket endpoint.
func (s *Server) AgentHandler() http.Handler { return s.agent }

// Close stops background work owned by the server.
func (s *Server) Close() {
	s.webhookCancel()
	s.platformMu.Lock()
	s.closed = true
	s.lifecycleCancel()
	pumps := make([]eventPump, 0, len(s.eventPumps))
	for boxID, pump := range s.eventPumps {
		delete(s.eventPumps, boxID)
		pumps = append(pumps, pump)
	}
	s.platformMu.Unlock()
	for _, pump := range pumps {
		pump.cancel()
	}
	s.eventWG.Wait()
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /provider-models", s.auth(http.HandlerFunc(s.providerModels)))
	mux.Handle("GET /agent/connect", s.AgentHandler())
	mux.HandleFunc("GET /auth/status", s.authStatus)
	mux.HandleFunc("POST /auth/signup", s.signup)
	mux.HandleFunc("POST /auth/login", s.login)
	mux.Handle("POST /auth/logout", s.auth(http.HandlerFunc(s.logout)))
	mux.Handle("GET /me", s.auth(http.HandlerFunc(s.me)))
	mux.Handle("GET /api-keys", s.auth(http.HandlerFunc(s.listAPIKeys)))
	mux.Handle("GET /api-keys/{id}/usage", s.auth(http.HandlerFunc(s.apiKeyUsage)))
	mux.Handle("POST /api-keys", s.auth(http.HandlerFunc(s.createAPIKey)))
	mux.Handle("DELETE /api-keys/{id}", s.auth(http.HandlerFunc(s.deleteAPIKey)))
	mux.Handle("GET /machine", s.auth(http.HandlerFunc(s.machine)))
	mux.Handle("GET /sandboxes", s.auth(http.HandlerFunc(s.listSandboxes)))
	mux.Handle("POST /sandboxes", s.auth(http.HandlerFunc(s.createSandbox)))
	mux.Handle("GET /sandboxes/{id}", s.auth(http.HandlerFunc(s.getSandbox)))
	mux.Handle("GET /operations/{id}", s.auth(http.HandlerFunc(s.getOperation)))
	mux.Handle("GET /sandboxes/{id}/usage", s.auth(http.HandlerFunc(s.sandboxUsage)))
	mux.Handle("GET /sandboxes/{id}/activity", s.auth(http.HandlerFunc(s.sandboxActivity)))
	mux.Handle("GET /settings/pricing", s.auth(http.HandlerFunc(s.getPricing)))
	mux.Handle("GET /settings/retention", s.auth(http.HandlerFunc(s.getRetention)))
	mux.Handle("PUT /settings/retention", s.auth(http.HandlerFunc(s.putRetention)))
	mux.Handle("PUT /settings/pricing", s.auth(http.HandlerFunc(s.putPricing)))
	mux.Handle("PATCH /sandboxes/{id}", s.auth(http.HandlerFunc(s.patchSandbox)))
	mux.Handle("DELETE /sandboxes/{id}", s.auth(http.HandlerFunc(s.deleteSandbox)))
	mux.Handle("POST /sandboxes/{id}/stop", s.auth(http.HandlerFunc(s.stopSandbox)))
	mux.Handle("POST /sandboxes/{id}/resume", s.auth(http.HandlerFunc(s.resumeSandbox)))
	mux.Handle("POST /sandboxes/{id}/restore", s.auth(http.HandlerFunc(s.restoreSandbox)))
	mux.Handle("POST /sandboxes/{id}/fork", s.auth(http.HandlerFunc(s.forkSandbox)))
	mux.Handle("POST /sandboxes/{id}/commands", s.auth(http.HandlerFunc(s.commands)))
	mux.Handle("GET /sandboxes/{id}/commands", s.auth(http.HandlerFunc(s.listCommands)))
	mux.Handle("GET /sandboxes/{id}/commands/{pid}", s.auth(http.HandlerFunc(s.commandResult)))
	mux.Handle("GET /sandboxes/{id}/commands/{pid}/logs", s.auth(http.HandlerFunc(s.commandLogs)))
	mux.Handle("POST /sandboxes/{id}/commands/{pid}/kill", s.auth(http.HandlerFunc(s.killCommand)))
	mux.Handle("POST /sandboxes/{id}/commands/{pid}/stdin", s.auth(http.HandlerFunc(s.commandStdin)))
	mux.Handle("GET /sandboxes/{id}/files", s.auth(http.HandlerFunc(s.getFile)))
	mux.Handle("PUT /sandboxes/{id}/files", s.auth(http.HandlerFunc(s.putFile)))
	mux.Handle("PUT /sandboxes/{id}/archive", s.auth(http.HandlerFunc(s.putArchive)))
	mux.Handle("GET /sandboxes/{id}/artifacts", s.auth(http.HandlerFunc(s.getArtifacts)))
	mux.Handle("GET /sandboxes/{id}/snapshots", s.auth(http.HandlerFunc(s.listSnapshots)))
	mux.Handle("GET /sandboxes/{id}/snapshots/latest", s.auth(http.HandlerFunc(s.latestSnapshot)))
	mux.Handle("GET /snapshots", s.auth(http.HandlerFunc(s.listAllSnapshots)))
	mux.Handle("GET /snapshots/{id}/download", s.auth(http.HandlerFunc(s.downloadSnapshot)))
	mux.Handle("DELETE /sandboxes/{id}/snapshots/{snapshotId}", s.auth(http.HandlerFunc(s.deleteSnapshot)))
	mux.Handle("GET /snapshots/{id}/tree", s.auth(http.HandlerFunc(s.snapshotTree)))
	mux.Handle("GET /snapshots/{id}/file", s.auth(http.HandlerFunc(s.snapshotFile)))
	mux.Handle("GET /named-snapshots", s.auth(http.HandlerFunc(s.listNamedSnapshots)))
	mux.Handle("POST /named-snapshots", s.auth(http.HandlerFunc(s.createNamedSnapshot)))
	mux.Handle("GET /named-snapshots/{name}", s.auth(http.HandlerFunc(s.getNamedSnapshot)))
	mux.Handle("DELETE /named-snapshots/{name}", s.auth(http.HandlerFunc(s.deleteNamedSnapshot)))
	mux.Handle("GET /drives", s.auth(http.HandlerFunc(s.listDrives)))
	mux.Handle("POST /drives", s.auth(http.HandlerFunc(s.createDrive)))
	mux.Handle("DELETE /drives/{name}", s.auth(http.HandlerFunc(s.deleteDrive)))
	mux.Handle("POST /drives/{name}/fork", s.auth(http.HandlerFunc(s.forkDrive)))
	mux.Handle("POST /sandboxes/{id}/desktop", s.auth(http.HandlerFunc(s.createDesktop)))
	mux.Handle("GET /sandboxes/{id}/desktop", s.auth(http.HandlerFunc(s.getDesktop)))
	mux.Handle("GET /sandboxes/{id}/hosts", s.auth(http.HandlerFunc(s.listHosts)))
	mux.Handle("POST /sandboxes/{id}/hosts", s.auth(http.HandlerFunc(s.createHost)))
	mux.Handle("DELETE /sandboxes/{id}/hosts/{port}", s.auth(http.HandlerFunc(s.deleteHost)))
	mux.Handle("POST /sandboxes/{id}/ssh-key", s.auth(http.HandlerFunc(s.addSSHKey)))
	mux.Handle("GET /sandboxes/{id}/tcp/22", s.auth(http.HandlerFunc(s.tcp22)))
	mux.Handle("GET /sandboxes/{id}/tcp/{port}", s.auth(http.HandlerFunc(s.tcpProxy)))
	mux.Handle("POST /sandboxes/{id}/prompt", s.auth(http.HandlerFunc(s.prompt)))
	mux.Handle("POST /sandboxes/{id}/steer", s.auth(http.HandlerFunc(s.steer)))
	mux.Handle("POST /sandboxes/{id}/interrupt", s.auth(http.HandlerFunc(s.interrupt)))
	mux.Handle("/sandboxes/{id}/boxd/{path...}", s.auth(http.HandlerFunc(s.proxyBoxd)))
	mux.Handle("GET /sandboxes/{id}/events", s.auth(http.HandlerFunc(s.listEvents)))
	mux.Handle("GET /sandboxes/{id}/conversations", s.auth(http.HandlerFunc(s.listConversations)))
	mux.Handle("GET /sandboxes/{id}/runs/{runId}", s.auth(http.HandlerFunc(s.getRun)))
	mux.Handle("GET /environments", s.auth(http.HandlerFunc(s.listEnvironments)))
	mux.Handle("POST /environments", s.auth(http.HandlerFunc(s.createEnvironment)))
	mux.Handle("GET /environments/{id}", s.auth(http.HandlerFunc(s.getEnvironment)))
	mux.Handle("PATCH /environments/{id}", s.auth(http.HandlerFunc(s.updateEnvironment)))
	mux.Handle("DELETE /environments/{id}", s.auth(http.HandlerFunc(s.deleteEnvironment)))
	mux.Handle("POST /environments/{id}/upgrade", s.auth(http.HandlerFunc(s.upgradeEnvironment)))
	mux.Handle("GET /webhooks", s.auth(http.HandlerFunc(s.listWebhooks)))
	mux.Handle("POST /webhooks", s.auth(http.HandlerFunc(s.createWebhook)))
	mux.Handle("POST /webhooks/{id}/rotate-secret", s.auth(http.HandlerFunc(s.rotateWebhookSecret)))
	mux.Handle("PATCH /webhooks/{id}", s.auth(http.HandlerFunc(s.updateWebhook)))
	mux.Handle("DELETE /webhooks/{id}", s.auth(http.HandlerFunc(s.deleteWebhook)))
	mux.Handle("GET /limits", s.auth(http.HandlerFunc(s.limits)))
	mux.Handle("GET /settings/storage", s.auth(http.HandlerFunc(s.storageSettings)))
	mux.Handle("PUT /settings/images", s.auth(http.HandlerFunc(s.updateImageSettings)))
	mux.HandleFunc("POST /admin/storage", s.setStorageFromAdmin)
	mux.HandleFunc("POST /admin/runtime/stop", s.stopRuntimeBoxesFromAdmin)
	mux.HandleFunc("POST /admin/runtime/resume", s.resumeRuntimeBoxesFromAdmin)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "API endpoint not found")
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) authenticateNodeToken(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	hash := tokenHash(token)
	nodes, err := s.store.ListNodes(context.Background())
	if err != nil {
		return "", false
	}
	for _, node := range nodes {
		if len(node.TokenHash) == len(hash) && subtle.ConstantTimeCompare([]byte(node.TokenHash), []byte(hash)) == 1 {
			return node.Node.ID, true
		}
	}
	return "", false
}

func (s *Server) nodeConnected(nodeID string) {
	s.platformMu.Lock()
	if s.closed {
		s.platformMu.Unlock()
		return
	}
	s.eventWG.Add(1)
	s.platformMu.Unlock()
	go func() { defer s.eventWG.Done(); s.reconcileConnectedNode(nodeID) }()
	go s.pollNodeHealth(nodeID)
}

func (s *Server) reconcileConnectedNode(nodeID string) {
	for s.lifecycleCtx.Err() == nil {
		client, ok := newAgentClient(s.hub, nodeID)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(s.lifecycleCtx, 10*time.Second)
		node, err := s.store.GetNode(ctx, nodeID)
		var health agentHealth
		if err == nil {
			err = client.json(ctx, http.MethodGet, "/v1/health", nil, &health)
		}
		if err == nil {
			node.Node.Status = "healthy"
			node.Node.CPUs = health.CPUs
			node.Node.MemoryBytes = health.MemoryBytes
			node.Node.FreeMemoryBytes = health.FreeMemoryBytes
			node.Node.MemAvailableBytes = health.MemAvailableBytes
			node.Node.SwapFreeBytes = health.SwapFreeBytes
			node.Node.MemoryPressureSomeAvg10 = health.MemoryPressureSomeAvg10
			s.healthMu.Lock()
			s.healthSnapshots[nodeID] = healthSnapshot{Health: health, At: s.now()}
			s.healthMu.Unlock()
			node.Node.DiskFreeBytes = health.DiskFreeBytes

			node.Node.LastSeen = time.Now().UTC()
			err = s.store.UpdateNode(ctx, node)
		}
		if err == nil {
			storage := s.storageConfig()
			if storage.Repository != "" {
				if storageErr := client.json(ctx, http.MethodPut, "/v1/storage", map[string]any{"repository": storage.Repository, "env": storage.Env}, nil); storageErr != nil {
					log.Printf("synchronize snapshot storage settings to node %s failed", nodeID)
				}
			}
			s.reconcileNode(ctx, nodeID, client, health)
			s.repushNodeRoutes(ctx, nodeID, client)
			cancel()
			return
		}
		cancel()
		if !sleepContext(s.lifecycleCtx, time.Second) {
			return
		}
	}
}

func (s *Server) pollNodeHealth(nodeID string) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.lifecycleCtx.Done():
			return
		case <-ticker.C:
			client, ok := newAgentClient(s.hub, nodeID)
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(s.lifecycleCtx, 5*time.Second)
			var h agentHealth
			err := client.json(ctx, http.MethodGet, "/v1/health", nil, &h)
			cancel()
			if err != nil {
				continue
			}
			s.healthMu.Lock()
			s.healthSnapshots[nodeID] = healthSnapshot{Health: h, At: s.now()}
			s.healthMu.Unlock()
			record, e := s.store.GetNode(s.lifecycleCtx, nodeID)
			if e == nil {
				record.Node.MemoryBytes = h.MemoryBytes
				record.Node.FreeMemoryBytes = h.MemAvailableBytes
				record.Node.MemAvailableBytes = h.MemAvailableBytes
				record.Node.SwapFreeBytes = h.SwapFreeBytes
				record.Node.MemoryPressureSomeAvg10 = h.MemoryPressureSomeAvg10
				record.Node.LastSeen = s.now().UTC()
				_ = s.store.UpdateNode(s.lifecycleCtx, record)
			}
		}
	}
}

func (s *Server) reconcileNode(ctx context.Context, nodeID string, client *agentClient, health agentHealth) {
	seen := make(map[string]struct{}, len(health.Boxes))
	for _, remote := range health.Boxes {
		if remote.BoxID == "" {
			continue
		}
		seen[remote.BoxID] = struct{}{}
		getBox := s.reconcileGetBox
		if getBox == nil {
			getBox = s.store.GetBox
		}
		record, err := getBox(ctx, remote.BoxID)
		if errors.Is(err, sql.ErrNoRows) {
			s.logOrphanContainer(nodeID, remote.BoxID)
			continue
		}
		if err != nil {
			log.Printf("reconcile box lookup failed box=%s node=%s: %v", remote.BoxID, nodeID, err)
			continue
		}
		if record.Box.NodeID != nodeID {
			remove, reason := s.mayFenceBox(ctx, record)
			if remove {
				log.Printf("fencing removal box=%s reported_node=%s recorded_node=%s reason=%s", remote.BoxID, nodeID, record.Box.NodeID, reason)
				_ = client.json(ctx, http.MethodDelete, agentPath("v1", "boxes", remote.BoxID), nil, nil)
			} else {
				log.Printf("fencing left box=%s reported_node=%s recorded_node=%s reason=%s", remote.BoxID, nodeID, record.Box.NodeID, reason)
			}
			continue
		}
		previousState := record.Box.State
		wasRunning := record.RunningSince != nil
		switch remote.State {
		case "running", "ready", "idle":
			record.Box.Error = ""
			record.Box.ErrorCode = ""
			if record.RunningSince == nil {
				now := time.Now().UTC()
				record.RunningSince = &now
			}
			if remote.State == "idle" {
				record.Box.State = types.StateIdle
			} else {
				record.Box.State = types.StateReady
			}
		case "stopped":
			record.Box.State = types.StateStopped
			now := time.Now().UTC()
			record.Box.StoppedAt = &now
			closeRunningInterval(&record, now)
		default:
			record.Box.State = types.StateError
			record.Box.ErrorCode = "agent_box_error"
			record.Box.Error = diagnostics.Sanitize("Agent reported box state: " + remote.State)
			closeRunningInterval(&record, time.Now().UTC())
		}
		if err := s.store.UpdateBox(ctx, record); err != nil {
			s.stopEventPump(record.Box.ID)
			continue
		}
		if (remote.State == "running" || remote.State == "ready" || remote.State == "idle") && !wasRunning && record.RunningSince != nil && previousState != types.StateReady && previousState != types.StateIdle {
			_ = s.store.OpenRunningInterval(ctx, record.Box.ID, *record.RunningSince)
			s.recordSystemActivity(ctx, record.Box.ID, "system", "lifecycle", "started/ready", map[string]any{"node": nodeID}, "ok")
		}
		if remote.State == "stopped" && previousState != types.StateStopped {
			now := time.Now().UTC()
			_ = s.store.CloseRunningInterval(ctx, record.Box.ID, now)
			s.recordSystemActivity(ctx, record.Box.ID, "system", "lifecycle", "stopped", nil, "ok")
			s.queueRetentionDelete(ctx, record)
		}
		if record.Box.State == types.StateError && previousState != types.StateError {
			_ = s.store.CloseRunningInterval(ctx, record.Box.ID, time.Now().UTC())
			s.recordSystemActivity(ctx, record.Box.ID, "system", "lifecycle", "error", map[string]any{"reason": record.Box.Error}, "error")
		}
		s.syncEventPump(record.Box)
	}
	boxes, err := s.store.ListBoxes(ctx, nodeID)
	if err != nil {
		return
	}
	for _, box := range boxes {
		if _, ok := seen[box.Box.ID]; ok {
			continue
		}
		if box.Box.State == types.StateReady || box.Box.State == types.StateIdle || box.Box.State == types.StateUnreachable {
			box.Box.State = types.StateError
			box.Box.ErrorCode = "box_missing"
			box.Box.Error = "The agent did not report this box as running."
			now := time.Now().UTC()
			closeRunningInterval(&box, now)
			if s.store.UpdateBox(ctx, box) == nil {
				_ = s.store.CloseRunningInterval(ctx, box.Box.ID, now)
				s.recordSystemActivity(ctx, box.Box.ID, "system", "lifecycle", "error", map[string]any{"reason": box.Box.Error}, "error")
			}
			s.stopEventPump(box.Box.ID)
		}
	}
}

func (s *Server) logOrphanContainer(nodeID, boxID string) {
	key := nodeID + "/" + boxID
	now := time.Now()
	s.orphanLogMu.Lock()
	if s.orphanLogAt == nil {
		s.orphanLogAt = make(map[string]time.Time)
	}
	last := s.orphanLogAt[key]
	if now.Sub(last) < 5*time.Minute {
		s.orphanLogMu.Unlock()
		return
	}
	s.orphanLogAt[key] = now
	s.orphanLogMu.Unlock()
	log.Printf("orphan container %s on node %s", boxID, nodeID)
}

func (s *Server) mayFenceBox(ctx context.Context, record store.BoxRecord) (bool, string) {
	if record.Box.State == types.StateStopped || record.Box.State == types.StateStopping {
		return true, "recorded box is stopped or stopping; data is in a snapshot"
	}
	client, ok := newAgentClient(s.hub, record.Box.NodeID)
	if !ok {
		return false, "recorded node is not connected"
	}
	var health agentHealth
	if err := client.json(ctx, http.MethodGet, "/v1/health", nil, &health); err != nil {
		return false, "recorded node health unavailable"
	}
	for _, remote := range health.Boxes {
		if remote.BoxID == record.Box.ID && (remote.State == "running" || remote.State == "ready" || remote.State == "idle") {
			return true, "recorded node health confirms box running"
		}
	}
	return false, "recorded node health does not confirm box running"
}

func (s *Server) nodeDisconnected(nodeID string) {
	ctx := context.Background()
	node, err := s.store.GetNode(ctx, nodeID)
	if err == nil {
		node.Node.Status = "offline"
		_ = s.store.UpdateNode(ctx, node)
	}
	boxes, err := s.store.ListBoxes(ctx, nodeID)
	if err != nil {
		return
	}
	for _, box := range boxes {
		if box.Box.State == types.StateReady || box.Box.State == types.StateIdle {
			box.Box.State = types.StateUnreachable
			if s.store.UpdateBox(ctx, box) == nil {
				s.recordSystemActivity(ctx, box.Box.ID, "system", "lifecycle", "error", map[string]any{"reason": "node disconnected"}, "error")
			}
			s.stopEventPump(box.Box.ID)
		}
	}
}

func tokenHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		token := ""
		cookieAuthenticated := false
		if strings.HasPrefix(header, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		} else if cookie, err := r.Cookie(sessionCookieName); err == nil {
			token = cookie.Value
			cookieAuthenticated = true
		}
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		if cookieAuthenticated && strings.HasPrefix(token, "bx_live_") {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid session cookie")
			return
		}
		user, ok := s.userForToken(r, token)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}
		if cookieAuthenticated && unsafeMethod(r.Method) && !csrfMatchesRequestHost(r) {
			writeError(w, http.StatusForbidden, "csrf_failed", "request origin does not match this host")
			return
		}
		ctx := context.WithValue(r.Context(), userContextKey, user)
		ctx = context.WithValue(ctx, authTokenContextKey, token)
		actor := activityActor{Type: "user", ID: user.ID, Label: user.Email}
		if strings.HasPrefix(token, "bx_live_") {
			if key, err := s.store.APIKeyByHash(r.Context(), tokenHash(token)); err == nil {
				actor = activityActor{Type: "api_key", ID: key.ID, Label: key.Name}
			}
		}
		ctx = context.WithValue(ctx, activityActorContextKey, actor)
		if actor.Type == "api_key" {
			s.apiUsageMu.Lock()
			s.apiUsagePending[actor.ID]++
			s.apiUsageMu.Unlock()
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func unsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func currentUser(r *http.Request) types.User {
	user, _ := r.Context().Value(userContextKey).(types.User)
	return user
}

func (s *Server) apiError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if strings.Contains(err.Error(), "offline") || strings.Contains(err.Error(), "disconnected") {
		writeError(w, http.StatusBadGateway, "node_unavailable", "node is not connected")
		return
	}
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		code := status.StatusCode()
		if code < 400 || code > 599 {
			code = http.StatusBadGateway
		}
		writeError(w, code, "agent_error", withAgentError("agent request failed", err))
		return
	}
	writeError(w, http.StatusInternalServerError, "internal_error", "request could not be completed")
}
