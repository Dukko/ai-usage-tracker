package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/push"
)

const pushUsage = `Usage: ai-usage-tracker push opencode [flags]

Reads local OpenCode session usage and sends daily totals to the tracker.
Create an "OpenCode" account in the tracker UI first to get the token.

Flags:
`

func runPush(args []string) error {
	if len(args) == 0 || args[0] != "opencode" {
		fmt.Fprint(os.Stderr, pushUsage)
		return errors.New(`expected source "opencode"`)
	}
	fs := flag.NewFlagSet("push opencode", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, pushUsage); fs.PrintDefaults() }
	url := fs.String("url", os.Getenv("TRACKER_URL"), "tracker base URL, e.g. http://tracker:8080 (env TRACKER_URL)")
	token := fs.String("token", os.Getenv("TRACKER_PUSH_TOKEN"), "ingest token from the tracker UI (env TRACKER_PUSH_TOKEN)")
	dbPath := fs.String("db", push.DefaultOpenCodeDB(), "path to opencode.db")
	days := fs.Int("days", 30, "number of days to send")
	every := fs.Duration("every", 0, "keep running and push at this interval (e.g. 10m); 0 = push once")
	dryRun := fs.Bool("dry-run", false, "print the payload instead of sending it")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *days < 1 || *days > 366 {
		return errors.New("-days must be between 1 and 366")
	}
	if !*dryRun && (*url == "" || *token == "") {
		return errors.New("-url and -token are required (or set TRACKER_URL and TRACKER_PUSH_TOKEN)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	once := func() error {
		rows, err := push.ReadOpenCode(ctx, *dbPath, *days)
		if err != nil {
			return err
		}
		p := push.Payload{Source: "opencode", Rows: rows}
		if *dryRun {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(p)
		}
		if err := push.Send(ctx, *url, *token, p); err != nil {
			return err
		}
		slog.Info("pushed opencode usage", "rows", len(rows), "days", *days)
		return nil
	}

	if *every <= 0 {
		return once()
	}
	if *every < time.Minute {
		return errors.New("-every must be at least 1m")
	}
	t := time.NewTicker(*every)
	defer t.Stop()
	for {
		if err := once(); err != nil {
			slog.Error("push failed", "err", err) // keep going; next tick may succeed
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
