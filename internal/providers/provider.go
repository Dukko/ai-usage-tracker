// Package providers fetches usage data from AI vendors.
//
// Two kinds of account are supported:
//   - api_key: an organization admin key, polled for daily token usage and cost.
//   - oauth:   a consumer subscription (Claude Pro/Max, ChatGPT Plus/Pro) connected via
//     OAuth, polled for rate-limit window utilization.
package providers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"ai-usage-tracker/internal/core"
)

const (
	KindAPIKey = "api_key"
	KindOAuth  = "oauth"
	KindPush   = "push"
)

type Provider interface {
	ID() string
	Label() string
	Kind() string
	// Fetch polls the provider. It may update creds in place (e.g. after an OAuth
	// token refresh); the caller persists any changes even when an error is returned.
	Fetch(ctx context.Context, creds *core.Credentials) (*core.Result, error)
}

// DeviceStart is the first step of an OAuth device-code sign-in.
type DeviceStart struct {
	DeviceCode string
	UserCode   string
	VerifyURL  string
	Interval   int // seconds between polls
	ExpiresIn  int // seconds
}

// ErrAuthPending means the user hasn't approved the device sign-in yet.
var ErrAuthPending = errors.New("authorization pending")

// DeviceProvider signs in with the OAuth device-code flow.
type DeviceProvider interface {
	Provider
	StartDevice(ctx context.Context) (*DeviceStart, error)
	// PollDevice returns ErrAuthPending until the user approves. label is a
	// suggested account name (e.g. the account email), possibly empty.
	PollDevice(ctx context.Context, deviceCode string) (creds *core.Credentials, label string, err error)
}

type APIKeyProvider interface {
	Provider
	KeyHint() string
}

// PendingAuth is an in-progress OAuth authorization.
type PendingAuth struct {
	State    string
	Verifier string
}

type OAuthProvider interface {
	Provider
	AuthURL(p PendingAuth) string
	// Instructions tell the user what to paste back after authorizing.
	Instructions() string
	// Exchange turns whatever the user pasted (code, code#state, or redirect URL) into tokens.
	Exchange(ctx context.Context, pasted string, p PendingAuth) (*core.Credentials, error)
	// ImportJSON accepts the credentials file written by the vendor's own CLI.
	ImportJSON(raw []byte) (*core.Credentials, error)
	ImportHint() string
}

var registry = map[string]Provider{}

func register(p Provider) { registry[p.ID()] = p }

func Get(id string) (Provider, bool) {
	p, ok := registry[id]
	return p, ok
}

func All() []Provider {
	out := make([]Provider, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label() < out[j].Label() })
	return out
}

func NewPendingAuth() PendingAuth {
	return PendingAuth{State: randomString(32), Verifier: randomString(32)}
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ---- HTTP helpers ----

var httpClient = &http.Client{Timeout: 30 * time.Second}

const userAgent = "ai-usage-tracker/0.1"

type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

func isUnauthorized(err error) bool {
	he, ok := err.(*HTTPError)
	return ok && (he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden)
}

func doJSON(req *http.Request, out any) error {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return &HTTPError{Status: resp.StatusCode, Body: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", req.URL.Path, err)
	}
	return nil
}

func postForm(ctx context.Context, u string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doJSON(req, out)
}

func postJSON(ctx context.Context, url string, payload, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doJSON(req, out)
}

// jwtClaims decodes the payload of a JWT without verifying it.
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func dayOf(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// windowName turns a window length into a label like "5h" or "7d".
func windowName(seconds int64) string {
	switch {
	case seconds <= 0:
		return "window"
	case seconds%86400 == 0:
		return fmt.Sprintf("%dd", seconds/86400)
	case seconds%3600 == 0:
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		return fmt.Sprintf("%dm", seconds/60)
	}
}
