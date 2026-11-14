package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ai-usage-tracker/internal/core"
)

// claudeSub tracks a Claude Pro/Max subscription's rate-limit windows using the
// same OAuth client and usage endpoint as Claude Code. These endpoints are not
// a documented public API and may change; URLs can be overridden by env vars.
type claudeSub struct{}

func init() { register(claudeSub{}) }

var (
	claudeClientID     = envOr("CLAUDE_OAUTH_CLIENT_ID", "9d1c250a-e61b-44d9-88ed-5944d1962f5e")
	claudeAuthorizeURL = envOr("CLAUDE_OAUTH_AUTHORIZE_URL", "https://claude.ai/oauth/authorize")
	claudeTokenURL     = envOr("CLAUDE_OAUTH_TOKEN_URL", "https://console.anthropic.com/v1/oauth/token")
	claudeRedirectURI  = envOr("CLAUDE_OAUTH_REDIRECT_URI", "https://console.anthropic.com/oauth/code/callback")
	claudeUsageURL     = envOr("CLAUDE_USAGE_URL", "https://api.anthropic.com/api/oauth/usage")
	claudeProfileURL   = envOr("CLAUDE_PROFILE_URL", "https://api.anthropic.com/api/oauth/profile")
)

const claudeScopes = "org:create_api_key user:profile user:inference"

func (claudeSub) ID() string    { return "claude-sub" }
func (claudeSub) Label() string { return "Claude subscription" }
func (claudeSub) Kind() string  { return KindOAuth }

func (claudeSub) AuthURL(p PendingAuth) string {
	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", claudeClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", claudeRedirectURI)
	q.Set("scope", claudeScopes)
	q.Set("code_challenge", pkceChallenge(p.Verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("state", p.State)
	return claudeAuthorizeURL + "?" + q.Encode()
}

func (claudeSub) Instructions() string {
	return "Sign in and approve access. Claude then shows an authentication code — copy the whole code and paste it below."
}

func (claudeSub) ImportHint() string {
	return `Contents of ~/.claude/.credentials.json (the {"claudeAiOauth": {...}} object).`
}

type claudeTokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (t claudeTokenResp) apply(c *core.Credentials) {
	c.AccessToken = t.AccessToken
	if t.RefreshToken != "" {
		c.RefreshToken = t.RefreshToken
	}
	if t.ExpiresIn > 0 {
		c.ExpiresAt = time.Now().Unix() + t.ExpiresIn
	}
}

func (claudeSub) Exchange(ctx context.Context, pasted string, p PendingAuth) (*core.Credentials, error) {
	code, err := checkPasted(pasted, p)
	if err != nil {
		return nil, err
	}
	var tr claudeTokenResp
	err = postJSON(ctx, claudeTokenURL, map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"state":         p.State,
		"client_id":     claudeClientID,
		"redirect_uri":  claudeRedirectURI,
		"code_verifier": p.Verifier,
	}, &tr)
	if err != nil {
		return nil, err
	}
	if tr.AccessToken == "" {
		return nil, errors.New("token response had no access_token")
	}
	c := &core.Credentials{}
	tr.apply(c)
	return c, nil
}

func (claudeSub) ImportJSON(raw []byte) (*core.Credentials, error) {
	type inner struct {
		AccessToken      string `json:"accessToken"`
		RefreshToken     string `json:"refreshToken"`
		ExpiresAt        int64  `json:"expiresAt"` // milliseconds
		SubscriptionType string `json:"subscriptionType"`
	}
	var wrapped struct {
		ClaudeAiOauth *inner `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, err
	}
	in := wrapped.ClaudeAiOauth
	if in == nil {
		in = &inner{}
		if err := json.Unmarshal(raw, in); err != nil {
			return nil, err
		}
	}
	if in.AccessToken == "" {
		return nil, errors.New("no accessToken found")
	}
	return &core.Credentials{
		AccessToken:  in.AccessToken,
		RefreshToken: in.RefreshToken,
		ExpiresAt:    in.ExpiresAt / 1000,
		Plan:         in.SubscriptionType,
	}, nil
}

func (s claudeSub) refresh(ctx context.Context, c *core.Credentials) error {
	var tr claudeTokenResp
	err := postJSON(ctx, claudeTokenURL, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": c.RefreshToken,
		"client_id":     claudeClientID,
	}, &tr)
	if err != nil {
		return err
	}
	if tr.AccessToken == "" {
		return errors.New("refresh response had no access_token")
	}
	tr.apply(c)
	return nil
}

func (s claudeSub) Fetch(ctx context.Context, c *core.Credentials) (*core.Result, error) {
	return withRefresh(ctx, c, s.refresh, s.fetch)
}

func (s claudeSub) authed(ctx context.Context, c *core.Credentials, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	return doJSON(req, out)
}

func (s claudeSub) fetch(ctx context.Context, c *core.Credentials) (*core.Result, error) {
	var raw map[string]json.RawMessage
	if err := s.authed(ctx, c, claudeUsageURL, &raw); err != nil {
		return nil, err
	}
	res := &core.Result{}
	for key, v := range raw {
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		}
		if json.Unmarshal(v, &w) != nil || w.Utilization == nil {
			continue
		}
		q := core.Quota{Window: claudeWindowName(key), UsedPercent: *w.Utilization}
		if w.ResetsAt != nil {
			if t, err := time.Parse(time.RFC3339, *w.ResetsAt); err == nil {
				q.ResetsAt = t.Unix()
			}
		}
		res.Quotas = append(res.Quotas, q)
	}

	if c.Plan == "" {
		c.Plan = s.plan(ctx, c)
	}
	res.Plan = c.Plan
	return res, nil
}

// plan makes a best-effort guess at the subscription tier from the profile endpoint.
func (s claudeSub) plan(ctx context.Context, c *core.Credentials) string {
	var p struct {
		Account struct {
			HasClaudeMax bool `json:"has_claude_max"`
			HasClaudePro bool `json:"has_claude_pro"`
		} `json:"account"`
		Organization struct {
			Type string `json:"organization_type"`
		} `json:"organization"`
	}
	if s.authed(ctx, c, claudeProfileURL, &p) != nil {
		return ""
	}
	switch {
	case p.Account.HasClaudeMax:
		return "max"
	case p.Account.HasClaudePro:
		return "pro"
	default:
		return strings.TrimPrefix(p.Organization.Type, "claude_")
	}
}

func claudeWindowName(key string) string {
	r := strings.NewReplacer("five_hour", "5h", "seven_day", "7d", "_", " ")
	return r.Replace(key)
}
