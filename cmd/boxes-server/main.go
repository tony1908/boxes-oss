package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/boxes-oss/boxes/internal/server"
	"github.com/boxes-oss/boxes/internal/store"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "storage" {
		if len(os.Args) < 3 || os.Args[2] != "set" {
			log.Fatal("usage: boxes-server storage set [--data-dir DIR] --s3-endpoint URL --s3-bucket NAME [--s3-prefix P] [--s3-region R]")
		}
		adminURL := os.Getenv("BOXES_SERVER_ADMIN_URL")
		if adminURL == "" {
			adminURL = "http://127.0.0.1:8080/api/v1/admin/storage"
		}
		if err := runStorageSet(os.Args[3:], adminURL, os.Getenv("BOXES_S3_ACCESS_KEY"), os.Getenv("BOXES_S3_SECRET_KEY"), os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dataDir := flag.String("data-dir", "/var/lib/boxes", "server data directory")
	publicURL := flag.String("public-url", "http://localhost:8080", "public server URL")
	image := flag.String("image", "boxes/box:latest", "default box image")
	snapshotInterval := flag.Duration("snapshot-interval", envDuration("BOXES_SNAPSHOT_INTERVAL", time.Minute), "periodic snapshot interval")
	desktopSessionTTL := flag.Duration("desktop-session-ttl", envDuration("BOXES_DESKTOP_SESSION_TTL", 8*time.Hour), "desktop session lifetime")
	snapshotBusyInterval := flag.Duration("snapshot-busy-interval", envDuration("BOXES_SNAPSHOT_BUSY_INTERVAL", 5*time.Minute), "minimum interval after repeated changed snapshots")
	memoryOvercommit := flag.Float64("memory-overcommit", 1.0, "memory reservation headroom factor (1.0 admits reservations up to 90% of node RAM)")
	admissionMode := flag.String("admission", "usage", "admission mode: usage or reservation")
	bufferPercent := flag.Float64("admission-buffer-percent", 0.15, "fraction of RAM reserved for host headroom")
	bufferMin := flag.Int64("admission-buffer-min", 1<<30, "minimum host memory buffer in bytes")
	expectedBox := flag.Int64("admission-expected-box", 300<<20, "minimum expected box memory in bytes")
	psiMax := flag.Float64("admission-psi-max", 10, "maximum memory PSI some avg10")
	swapAllowance := flag.Int64("admission-swap-allowance", 1<<30, "swap use allowed before reducing usage admission capacity, in bytes")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	db, err := store.Open(filepath.Join(*dataDir, "boxes.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	srv, err := server.New(server.Config{
		Store: db, DataDir: *dataDir, PublicURL: *publicURL, Image: *image, ToolsImage: os.Getenv("BOXES_TOOLS_IMAGE"), SnapshotInterval: *snapshotInterval, SnapshotBusyInterval: *snapshotBusyInterval, DesktopSessionTTL: *desktopSessionTTL, MemoryOvercommit: *memoryOvercommit, AdmissionMode: *admissionMode, AdmissionBufferPercent: *bufferPercent, AdmissionBufferMin: *bufferMin, AdmissionExpectedBox: *expectedBox, AdmissionPSIMax: *psiMax, AdmissionSwapAllowance: *swapAllowance,
		BootstrapJoinToken: os.Getenv("BOXES_BOOTSTRAP_JOIN_TOKEN"),
		BootstrapNodeName:  os.Getenv("BOXES_BOOTSTRAP_NODE_NAME"),
		BootstrapHostBase:  os.Getenv("BOXES_BOOTSTRAP_HOST_BASE"),
		StorageBootstrap: server.StorageBootstrap{
			Repository: os.Getenv("BOXES_STORAGE_REPOSITORY"), Endpoint: os.Getenv("BOXES_STORAGE_ENDPOINT"),
			Bucket: os.Getenv("BOXES_STORAGE_BUCKET"), Prefix: os.Getenv("BOXES_STORAGE_PREFIX"),
			Region: os.Getenv("BOXES_STORAGE_REGION"), AccessKey: os.Getenv("BOXES_STORAGE_ACCESS_KEY"),
			SecretKey: os.Getenv("BOXES_STORAGE_SECRET_KEY"), Password: os.Getenv("BOXES_STORAGE_RESTIC_PASSWORD"),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.RunReaper(ctx)
	go srv.RunSnapshotLoop(ctx)
	go srv.RunAutoscaling(ctx)
	httpServer := &http.Server{Addr: *addr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	log.Printf("boxes-server listening on %s", *addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		log.Fatalf("invalid %s: %v", name, err)
	}
	return duration
}
