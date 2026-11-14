package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/poller"
	"github.com/Dukko/ai-usage-tracker/internal/secrets"
	"github.com/Dukko/ai-usage-tracker/internal/store"
	"github.com/Dukko/ai-usage-tracker/internal/web"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `ai-usage-tracker: self-hosted dashboard for AI subscription limits and API spend.

Usage:
  ai-usage-tracker                 run the server (configure with TRACKER_* env vars, see README)
  ai-usage-tracker push <tool>     send local usage to a tracker (run "push -h" for flags)
  ai-usage-tracker healthcheck     exit 0 if the local server answers /healthz
  ai-usage-tracker version         print the version
`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "push":
			if err := runPush(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "push:", err)
				os.Exit(1)
			}
			return
		case "version", "-version", "--version":
			fmt.Println("ai-usage-tracker", version)
			return
		case "healthcheck":
			if err := healthcheck(); err != nil {
				fmt.Fprintln(os.Stderr, "unhealthy:", err)
				os.Exit(1)
			}
			return
		case "help", "-h", "-help", "--help":
			fmt.Print(usage)
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
			os.Exit(2)
		}
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

	slog.Info("listening", "version", version, "addr", addr, "data", dataDir, "poll_interval", interval)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// healthcheck probes the server's /healthz on the configured port, for
// container health checks in images without curl or a shell.
func healthcheck() error {
	addr := env("TRACKER_ADDR", ":8080")
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
