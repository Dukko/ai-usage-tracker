package providers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/core"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parsePasted extracts the authorization code and state from what the user pasted:
// a full redirect URL, "code#state", or a bare code.
func parsePasted(s string) (code, state string) {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Query().Get("code") != "" {
		return u.Query().Get("code"), u.Query().Get("state")
	}
	if i := strings.Index(s, "#"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

func checkPasted(pasted string, p PendingAuth) (string, error) {
	code, state := parsePasted(pasted)
	if code == "" {
		return "", errors.New("no authorization code found in the pasted value")
	}
	if state != "" && state != p.State {
		return "", errors.New("state mismatch: this code belongs to a different login attempt, start again")
	}
	return code, nil
}

// withRefresh runs fetch, refreshing the access token first if it is about to
// expire and once more if the provider rejects it.
func withRefresh(ctx context.Context, c *core.Credentials,
	refresh func(context.Context, *core.Credentials) error,
	fetch func(context.Context, *core.Credentials) (*core.Result, error),
) (*core.Result, error) {
	if c.AccessToken == "" {
		return nil, errors.New("no access token stored")
	}
	if c.RefreshToken != "" && c.ExpiresAt != 0 && time.Now().Unix() > c.ExpiresAt-120 {
		if err := refresh(ctx, c); err != nil {
			return nil, fmt.Errorf("refresh token: %w (reconnect the account)", err)
		}
	}
	res, err := fetch(ctx, c)
	if isUnauthorized(err) && c.RefreshToken != "" {
		if rerr := refresh(ctx, c); rerr != nil {
			return nil, fmt.Errorf("refresh token: %w (reconnect the account)", rerr)
		}
		res, err = fetch(ctx, c)
	}
	return res, err
}
