package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
	if len(os.Args) > 1 && os.Args[1] == "push" {
		if err := runPush(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "push:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	addr := env("TRACKER_ADDR", ":8080")
	dataDir := env("TRACKER_DATA_DIR", "./data")
	password := os.Getenv("TRACKER_PASSWORD")
	trustProxy, _ := strconv.ParseBool(os.Getenv("TRACKER_TRUST_PROXY"))
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

	// An interval saved in Settings wins over the environment.
	if saved, err := st.HasSetting("poll_minutes"); err == nil && saved {
		if set, err := st.GetSettings(); err == nil {
			interval = time.Duration(set.PollMinutes) * time.Minute
		}
	}
	pl := poller.New(st, box, interval)

	handler, err := web.New(st, box, pl, web.Config{AdminUser: os.Getenv("TRACKER_USERNAME"), AdminPassword: password, TrustProxy: trustProxy})
	if err != nil {
		return err
	}
	go pl.Run(ctx)
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("listening", "addr", addr, "data", dataDir, "poll_interval", interval)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
