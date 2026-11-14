package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ai-usage-tracker/internal/poller"
	"ai-usage-tracker/internal/secrets"
	"ai-usage-tracker/internal/store"
	"ai-usage-tracker/internal/web"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	addr := env("TRACKER_ADDR", ":8080")
	dataDir := env("TRACKER_DATA_DIR", "./data")
	password := os.Getenv("TRACKER_PASSWORD")
	interval, err := time.ParseDuration(env("TRACKER_POLL_INTERVAL", "10m"))
	if err != nil || interval < time.Minute {
		return errors.New("TRACKER_POLL_INTERVAL must be a duration of at least 1m (e.g. 10m)")
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	box, err := secrets.Load(dataDir)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dataDir, "tracker.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pl := poller.New(st, box, interval)
	go pl.Run(ctx)

	handler, err := web.New(st, box, pl, password)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if password == "" {
		slog.Warn("TRACKER_PASSWORD is not set: the dashboard is unauthenticated, only expose it on a trusted network")
	}
	slog.Info("listening", "addr", addr, "data", dataDir, "poll_interval", interval)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
