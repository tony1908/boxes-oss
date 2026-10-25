package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tony1908/boxes-oss/internal/boxd"
	"github.com/tony1908/boxes-oss/internal/computer"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	return runWithOutput(args, os.Stdout)
}

func runWithOutput(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: boxd <serve|mcp|setup>")
	}
	if args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(stdout, "Usage: boxd <serve|mcp|setup>")
		return err
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "mcp":
		return boxd.RunMCP(context.Background(), computer.New(commandExecutor{}))
	case "setup":
		return setup(args[1:])
	default:
		return fmt.Errorf("unknown boxd command %q", args[0])
	}
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", ":7070", "HTTP listen address")
	dbPath := flags.String("db", "/config/.boxd/boxd.db", "outbox SQLite path")
	workDir := flags.String("work-dir", "/config", "box configuration directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if token := os.Getenv("BOXD_TOKEN"); token == "" {
		return errors.New("BOXD_TOKEN is required")
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0700); err != nil {
		return err
	}
	service, err := boxd.New(boxd.Config{DBPath: *dbPath, Token: os.Getenv("BOXD_TOKEN"), WorkDir: *workDir})
	if err != nil {
		return err
	}
	defer func() { _ = service.Close() }()
	server := &http.Server{Addr: *addr, Handler: service.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func setup(args []string) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	root := flags.String("root", "/config", "box config directory")
	setupFile := flags.String("file", "/run/boxes/setup.json", "setup configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return boxd.RunSetup(context.Background(), boxd.SetupOptions{Root: *root, SetupFile: *setupFile})
}

type commandExecutor struct{}

func (commandExecutor) Run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return -1, err
}
