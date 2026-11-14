// Package poller periodically fetches usage for every account.
package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"ai-usage-tracker/internal/core"
	"ai-usage-tracker/internal/providers"
	"ai-usage-tracker/internal/secrets"
	"ai-usage-tracker/internal/store"
)

type Poller struct {
	st       *store.Store
	box      *secrets.Box
	interval time.Duration
	mu       sync.Mutex // serializes polls so token refreshes never race
}

func New(st *store.Store, box *secrets.Box, interval time.Duration) *Poller {
	return &Poller{st: st, box: box, interval: interval}
}

func (p *Poller) Run(ctx context.Context) {
	p.PollAll(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.PollAll(ctx)
		}
	}
}

func (p *Poller) PollAll(ctx context.Context) {
	accounts, err := p.st.ListAccounts()
	if err != nil {
		slog.Error("list accounts", "err", err)
		return
	}
	for _, a := range accounts {
		if ctx.Err() != nil {
			return
		}
		if err := p.Poll(ctx, a.ID); err != nil {
			slog.Warn("poll failed", "account", a.Name, "provider", a.Provider, "err", err)
		}
	}
}

// Poll fetches one account now and records the outcome on the account.
func (p *Poller) Poll(ctx context.Context, id int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	a, err := p.st.GetAccount(id)
	if err != nil {
		return err
	}
	err = p.poll(ctx, a)
	if err != nil {
		_ = p.st.RecordError(a.ID, err.Error())
	}
	return err
}

func (p *Poller) poll(ctx context.Context, a store.Account) error {
	prov, ok := providers.Get(a.Provider)
	if !ok {
		return fmt.Errorf("unknown provider %q", a.Provider)
	}
	plain, err := p.box.Open(a.Secret)
	if err != nil {
		return err
	}
	var creds core.Credentials
	if err := json.Unmarshal(plain, &creds); err != nil {
		return fmt.Errorf("decode credentials: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, fetchErr := prov.Fetch(ctx, &creds)

	// Persist refreshed tokens even if the fetch itself failed afterwards,
	// otherwise a rotated refresh token would be lost.
	if updated, err := json.Marshal(creds); err == nil && !bytes.Equal(updated, plain) {
		if sealed, err := p.box.Seal(updated); err == nil {
			if err := p.st.UpdateSecret(a.ID, sealed); err != nil {
				slog.Error("save refreshed credentials", "account", a.Name, "err", err)
			}
		}
	}
	if fetchErr != nil {
		return fetchErr
	}
	return p.st.SaveResult(a.ID, res)
}
