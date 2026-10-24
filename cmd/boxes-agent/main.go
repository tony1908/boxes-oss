package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/tony1908/boxes-oss/internal/agent"
	"github.com/tony1908/boxes-oss/internal/dockercli"
	"github.com/tony1908/boxes-oss/internal/snapshot"
	"github.com/tony1908/boxes-oss/internal/tunnel"
)

func main() {
	serverURL := flag.String("server-url", envOr("BOXES_SERVER_URL", ""), "boxes server URL")
	nodeToken := flag.String("node-token", envOr("BOXES_NODE_TOKEN", ""), "node authentication token")
	nodeID := flag.String("node-id", envOr("BOXES_NODE_ID", ""), "node ID")
	runtimeName := flag.String("runtime", envOr("BOXES_RUNTIME", "sysbox-runc"), "Docker runtime")
	traefikDir := flag.String("traefik-dir", envOr("BOXES_TRAEFIK_DIR", "/etc/boxes/traefik/dynamic"), "Traefik dynamic configuration directory")
	networkDir := flag.String("network-dir", envOr("BOXES_NETWORK_DIR", "/var/lib/boxes/network"), "directory for saved per-box network policies")
	forwardAuthAddr := envOr("BOXES_FORWARD_AUTH_ADDR", "127.0.0.1:7001")
	forwardAuthURL := envOr("BOXES_FORWARD_AUTH_URL", "http://127.0.0.1:7001/auth/forward")
	storagePath := flag.String("storage-file", envOr("BOXES_STORAGE_FILE", "/etc/boxes/storage.json"), "private snapshot storage settings file")
	repository := flag.String("restic-repository", envOr("RESTIC_REPOSITORY", ""), "Restic repository")
	password := flag.String("restic-password", envOr("RESTIC_PASSWORD", ""), "Restic password")
	flag.Parse()
	storageEnv := map[string]string{}
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_DEFAULT_REGION"} {
		if value := os.Getenv(key); value != "" {
			storageEnv[key] = value
		}
	}
	if storedRepository, storedPassword, storedEnv, loadErr := agent.LoadStorage(*storagePath); loadErr == nil {
		*repository, *password = storedRepository, storedPassword
		storageEnv = storedEnv
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		log.Fatalf("load persisted snapshot storage settings: %v", loadErr)
	}
	if err := validateConfig(*serverURL, *nodeToken, *repository); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	dockerExecutor := agent.ExecExecutor{}
	docker := dockercli.New(dockerExecutor)
	engine := snapshot.New(dockerExecutor, dockerExecutor, *repository, *password)
	engine.Env = storageEnv
	if err := docker.EnsureNetwork(ctx); err != nil {
		log.Fatalf("ensure Docker network: %v", err)
	}
	if err := engine.Init(ctx); err != nil {
		log.Fatalf("initialize snapshot repository: %v", err)
	}

	service := agent.New(agent.Config{NodeID: *nodeID, Runtime: *runtimeName, TraefikDir: *traefikDir, NetworkDir: *networkDir, ForwardAuthURL: forwardAuthURL, StoragePath: *storagePath}, docker, engine)
	service.RestoreNetworkPolicies(ctx)
	go service.RunNetworkRefresh(ctx)
	forwardAuth := &http.Server{Addr: forwardAuthAddr, Handler: service.ForwardAuthHandler(), ReadHeaderTimeout: 5 * time.Second}
	forwardAuthErrors := make(chan error, 1)
	go func() {
		if err := forwardAuth.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			forwardAuthErrors <- err
		}
	}()
	defer func() { _ = forwardAuth.Shutdown(context.Background()) }()

	errCh := make(chan error, 1)
	go func() { errCh <- tunnel.RunForever(ctx, tunnelURL(*serverURL), *nodeToken, service.Handler()) }()
	select {
	case err := <-forwardAuthErrors:
		log.Fatalf("start local forward-auth server: %v", err)
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			log.Fatal(err)
		}
	case <-ctx.Done():
	}
}

func validateConfig(serverURL, nodeToken, repository string) error {
	if serverURL == "" || nodeToken == "" || repository == "" {
		return errors.New("BOXES_SERVER_URL, BOXES_NODE_TOKEN, and RESTIC_REPOSITORY are required")
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func tunnelURL(server string) string {
	parsed, err := url.Parse(server)
	if err != nil {
		return server
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	}
	switch {
	case strings.HasSuffix(parsed.Path, "/api/v1/agent/connect"):
	case strings.HasSuffix(parsed.Path, "/api/v1"):
		parsed.Path = path.Join(parsed.Path, "agent/connect")
	default:
		parsed.Path = path.Join(parsed.Path, "api/v1/agent/connect")
	}
	return parsed.String()
}
