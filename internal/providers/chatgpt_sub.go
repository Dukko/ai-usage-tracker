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

// chatgptSub tracks a ChatGPT Plus/Pro subscription's Codex rate-limit windows
// using the same OAuth client and usage endpoint as the Codex CLI. These are not
// documented public APIs and may change; URLs can be overridden by env vars.
type chatgptSub struct{}

func init() { register(chatgptSub{}) }

var (
	chatgptClientID    = envOr("CHATGPT_OAUTH_CLIENT_ID", "app_EMoamEEZ73f0CkXaXp7hrann")
	chatgptIssuer      = envOr("CHATGPT_OAUTH_ISSUER", "https://auth.openai.com")
	chatgptRedirectURI = envOr("CHATGPT_OAUTH_REDIRECT_URI", "http://localhost:1455/auth/callback")
	chatgptUsageURL    = envOr("CHATGPT_USAGE_URL", "https://chatgpt.com/backend-api/wham/usage")
)

func (chatgptSub) ID() string    { return "chatgpt-sub" }
func (chatgptSub) Label() string { return "ChatGPT subscription (Codex)" }
func (chatgptSub) Kind() string  { return KindOAuth }

func (chatgptSub) AuthURL(p PendingAuth) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", chatgptClientID)
	q.Set("redirect_uri", chatgptRedirectURI)
	q.Set("scope", "openid profile email offline_access")
	q.Set("code_challenge", pkceChallenge(p.Verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", p.State)
	return chatgptIssuer + "/oauth/authorize?" + q.Encode()
}

func (chatgptSub) Instructions() string {
	return "Sign in and approve access. Your browser is then sent to a localhost:1455 page that fails to load — that's expected. Copy the full URL from the address bar and paste it below."
}

func (chatgptSub) ImportHint() string {
	return "Contents of ~/.codex/auth.json (after `codex login`)."
}

type chatgptTokenResp struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (t chatgptTokenResp) apply(c *core.Credentials) {
	if t.AccessToken != "" {
		c.AccessToken = t.AccessToken
	}
	if t.RefreshToken != "" {
		c.RefreshToken = t.RefreshToken
	}
	if t.IDToken != "" {
		c.IDToken = t.IDToken
	}
	fillFromJWTs(c)
	if c.ExpiresAt == 0 && t.ExpiresIn > 0 {
		c.ExpiresAt = time.Now().Unix() + t.ExpiresIn
	}
}

// fillFromJWTs derives expiry, account id and plan from the token claims.
func fillFromJWTs(c *core.Credentials) {
	if claims := jwtClaims(c.AccessToken); claims != nil {
		if exp, ok := claims["exp"].(float64); ok {
			c.ExpiresAt = int64(exp)
		}
	}
	for _, tok := range []string{c.IDToken, c.AccessToken} {
		claims := jwtClaims(tok)
		auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
		if auth == nil {
			continue
		}
		if id, _ := auth["chatgpt_account_id"].(string); id != "" && c.AccountID == "" {
			c.AccountID = id
		}
		if plan, _ := auth["chatgpt_plan_type"].(string); plan != "" && c.Plan == "" {
			c.Plan = plan
		}
	}
}

func (chatgptSub) Exchange(ctx context.Context, pasted string, p PendingAuth) (*core.Credentials, error) {
	code, err := checkPasted(pasted, p)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", chatgptRedirectURI)
	form.Set("client_id", chatgptClientID)
	form.Set("code_verifier", p.Verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptIssuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tr chatgptTokenResp
	if err := doJSON(req, &tr); err != nil {
		return nil, err
	}
	if tr.AccessToken == "" {
		return nil, errors.New("token response had no access_token")
	}
	c := &core.Credentials{}
	tr.apply(c)
	return c, nil
}

func (chatgptSub) ImportJSON(raw []byte) (*core.Credentials, error) {
	var f struct {
		Tokens *struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if f.Tokens == nil || f.Tokens.AccessToken == "" {
		return nil, errors.New(`no "tokens.access_token" found (is this ~/.codex/auth.json from a ChatGPT login?)`)
	}
	c := &core.Credentials{
		IDToken:      f.Tokens.IDToken,
		AccessToken:  f.Tokens.AccessToken,
		RefreshToken: f.Tokens.RefreshToken,
		AccountID:    f.Tokens.AccountID,
	}
	fillFromJWTs(c)
	return c, nil
}

func (s chatgptSub) refresh(ctx context.Context, c *core.Credentials) error {
	var tr chatgptTokenResp
	err := postJSON(ctx, chatgptIssuer+"/oauth/token", map[string]string{
		"client_id":     chatgptClientID,
		"grant_type":    "refresh_token",
		"refresh_token": c.RefreshToken,
		"scope":         "openid profile email",
	}, &tr)
	if err != nil {
		return err
	}
	if tr.AccessToken == "" {
		return errors.New("refresh response had no access_token")
	}
	c.ExpiresAt = 0
	tr.apply(c)
	return nil
}

func (s chatgptSub) Fetch(ctx context.Context, c *core.Credentials) (*core.Result, error) {
	return withRefresh(ctx, c, s.refresh, s.fetch)
}

type chatgptWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

func (s chatgptSub) fetch(ctx context.Context, c *core.Credentials) (*core.Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chatgptUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	if c.AccountID != "" {
		req.Header.Set("chatgpt-account-id", c.AccountID)
	}
	var r struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			Primary   *chatgptWindow `json:"primary_window"`
			Secondary *chatgptWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := doJSON(req, &r); err != nil {
		return nil, err
	}
	res := &core.Result{Plan: r.PlanType}
	if res.Plan == "" {
		res.Plan = c.Plan
	}
	if r.RateLimit != nil {
		now := time.Now().Unix()
		for _, w := range []*chatgptWindow{r.RateLimit.Primary, r.RateLimit.Secondary} {
			if w == nil {
				continue
			}
			q := core.Quota{Window: windowName(w.LimitWindowSeconds), UsedPercent: w.UsedPercent, ResetsAt: w.ResetAt}
			if q.ResetsAt == 0 && w.ResetAfterSeconds > 0 {
				q.ResetsAt = now + w.ResetAfterSeconds
			}
			res.Quotas = append(res.Quotas, q)
		}
	}
	return res, nil
}
