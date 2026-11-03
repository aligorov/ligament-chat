package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aligorov/ligament-chat/internal/api"
	"github.com/aligorov/ligament-chat/internal/store"
)

var (
	Version   = "v0.1.10"
	BuildDate = "2026-11-04"
)

func main() {
	listenAddr := flag.String("listen", "0.0.0.0:8097", "HTTP/WS listen address")
	dataDir := flag.String("data-dir", "/var/lib/ligament-chat", "Directory for attachments and media")
	dbSocket := flag.String("db-socket", "/var/run/postgresql", "PostgreSQL Unix socket directory")
	dbUser := flag.String("db-user", "chat", "PostgreSQL database user")
	dbName := flag.String("db-name", "ligament_chat", "PostgreSQL database name")
	dbConnStr := flag.String("db-conn", "", "Explicit PostgreSQL connection string (overrides socket)")
	recorderURL := flag.String("recorder-url", "http://127.0.0.1:8095", "Ligament Recorder base URL")
	coreURL := flag.String("core-url", "http://127.0.0.1:8080", "Ligament 2FA Core base URL")
	flag.Parse()

	resolvedCore := *coreURL
	if envCore := os.Getenv("CORE_URL"); envCore != "" && *coreURL == "http://127.0.0.1:8080" {
		resolvedCore = envCore
	} else if envTwoFA := os.Getenv("TWOFA_URL"); envTwoFA != "" && *coreURL == "http://127.0.0.1:8080" {
		resolvedCore = envTwoFA
	}

	resolvedRecorder := *recorderURL
	if envRec := os.Getenv("RECORDER_URL"); envRec != "" && *recorderURL == "http://127.0.0.1:8095" {
		resolvedRecorder = envRec
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	slog.Info("ligament-chat starting", "version", Version, "build_date", BuildDate, "listen", *listenAddr, "core_url", resolvedCore)

	if err := os.MkdirAll(filepath.Join(*dataDir, "attachments"), 0755); err != nil {
		slog.Error("failed to create data directory", "err", err)
		os.Exit(1)
	}

	connStr := *dbConnStr
	if connStr == "" {
		if envDSN := os.Getenv("CHAT_DB_DSN"); envDSN != "" {
			connStr = envDSN
		} else if envDB := os.Getenv("DATABASE_URL"); envDB != "" {
			connStr = envDB
		} else {
			connStr = fmt.Sprintf("postgres://%s@/%s?host=%s", *dbUser, *dbName, *dbSocket)
		}
	}

	st := store.NewStore()
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := st.AttachPostgres(ctx, connStr); err != nil {
		cancel()
		slog.Error("failed to attach to postgres", "err", err, "conn", connStr)
		os.Exit(1)
	}
	cancel()
	slog.Info("connected to postgres and verified schema migrations")

	hub := api.NewHub()
	stopHub := make(chan struct{})
	go hub.Run(stopHub)

	srv := api.NewServer(st, hub, *dataDir, resolvedRecorder, resolvedCore)

	httpServer := &http.Server{
		Addr:              *listenAddr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("chat server listening", "addr", *listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down gracefully...")

	close(stopHub)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "err", err)
	}
	slog.Info("ligament-chat stopped")
}
