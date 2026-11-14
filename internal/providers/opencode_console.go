package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/core"
)

// opencodeConsole tracks an OpenCode Console workspace: the OpenCode Go
// subscription's rolling/weekly/monthly limits, and any Console spend budgets
// (which cover Zen pay-as-you-go spend).
//
// It reads the same endpoints OpenCode's own CLI uses for its usage view
// (packages/core/src/usage-limits.ts in anomalyco/opencode). Sign-in is the
// Console device-code flow the CLI uses; an OpenCode API key also works for the
// Go limits, but cannot read the plan name.
type opencodeConsole struct{}

func init() { register(opencodeConsole{}) }

var opencodeServer = strings.TrimRight(envOr("OPENCODE_CONSOLE_URL", "https://opencode.ai/console"), "/")

const opencodeClientID = "opencode-cli"

func (opencodeConsole) ID() string    { return "opencode-console" }
func (opencodeConsole) Label() string { return "OpenCode Go / Console" }
func (opencodeConsole) Kind() string  { return KindOAuth }
func (opencodeConsole) KeyHint() string {
	return "OpenCode API key: Go limits only; sign in instead to also get plan and budgets"
}

func opencodeOrigin() string {
	u, err := url.Parse(opencodeServer)
	if err != nil {
		return "https://opencode.ai"
	}
	return u.Scheme + "://" + u.Host
}

// ---- device sign-in ----

func (opencodeConsole) StartDevice(ctx context.Context) (*DeviceStart, error) {
	startURL := opencodeServer + "/auth/device/code"
	var r struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	err := postForm(ctx, startURL, url.Values{"client_id": {opencodeClientID}, "supports_org_scope": {"true"}}, &r)
	if err != nil {
		return nil, err
	}
	if r.DeviceCode == "" || r.UserCode == "" {
		return nil, errors.New("device sign-in response was missing its codes")
	}
	base, _ := url.Parse(startURL)
	var verify string
	for _, cand := range []string{r.VerificationURIComplete, r.VerificationURI} {
		if cand == "" {
			continue
		}
		// Same scheme as the configured Console (https unless overridden for testing).
		if u, err := base.Parse(cand); err == nil && u.Scheme == base.Scheme && u.Host != "" {
			verify = u.String()
			break
		}
	}
	if verify == "" {
		return nil, errors.New("device sign-in response had no usable verification URL")
	}
	if r.Interval < 2 {
		r.Interval = 5
	}
	if r.ExpiresIn <= 0 {
		r.ExpiresIn = 600
	}
	return &DeviceStart{DeviceCode: r.DeviceCode, UserCode: r.UserCode, VerifyURL: verify, Interval: r.Interval, ExpiresIn: r.ExpiresIn}, nil
}

type opencodeTokenResp struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresIn    float64 `json:"expires_in"`
	OrgID        string  `json:"org_id"`
	Error        string  `json:"error"`
}

func (t opencodeTokenResp) apply(c *core.Credentials) {
	c.AccessToken = t.AccessToken
	if t.RefreshToken != "" {
		c.RefreshToken = t.RefreshToken
	}
	if t.ExpiresIn > 0 {
		c.ExpiresAt = time.Now().Unix() + int64(t.ExpiresIn)
	}
	if t.OrgID != "" {
		c.AccountID = t.OrgID
	}
}

// tokenRequest posts to the device token endpoint. OAuth errors such as
// authorization_pending come back as non-2xx JSON bodies, so they are decoded too.
func opencodeTokenRequest(ctx context.Context, form url.Values) (opencodeTokenResp, error) {
	var tr opencodeTokenResp
	err := postForm(ctx, opencodeServer+"/auth/device/token", form, &tr)
	var he *HTTPError
	if errors.As(err, &he) {
		var body opencodeTokenResp
		if json.Unmarshal([]byte(he.Body), &body) == nil && body.Error != "" {
			return body, nil
		}
	}
	return tr, err
}

func (o opencodeConsole) PollDevice(ctx context.Context, deviceCode string) (*core.Credentials, string, error) {
	tr, err := opencodeTokenRequest(ctx, url.Values{
		"client_id":   {opencodeClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	})
	if err != nil {
		return nil, "", err
	}
	switch tr.Error {
	case "":
	case "authorization_pending", "slow_down":
		return nil, "", ErrAuthPending
	case "expired_token":
		return nil, "", errors.New("the sign-in code expired, start again")
	case "access_denied":
		return nil, "", errors.New("sign-in was denied")
	default:
		return nil, "", fmt.Errorf("sign-in failed: %s", tr.Error)
	}
	if tr.AccessToken == "" {
		return nil, "", errors.New("token response had no access_token")
	}
	if tr.OrgID == "" {
		return nil, "", errors.New("sign-in did not pick a workspace")
	}
	c := &core.Credentials{}
	tr.apply(c)

	var user struct {
		Email string `json:"email"`
	}
	_ = o.console(ctx, c, "/api/user", &user) // best-effort, only for the default name
	return c, user.Email, nil
}

func (o opencodeConsole) refresh(ctx context.Context, c *core.Credentials) error {
	tr, err := opencodeTokenRequest(ctx, url.Values{
		"client_id":     {opencodeClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.RefreshToken},
	})
	if err != nil {
		return err
	}
	if tr.Error != "" || tr.AccessToken == "" {
		return fmt.Errorf("refresh refused: %s", tr.Error)
	}
	tr.apply(c)
	return nil
}

// ---- reads ----

func token(c *core.Credentials) string {
	if c.AccessToken != "" {
		return c.AccessToken
	}
	return c.APIKey
}

// console GETs a Console API path as the signed-in member of the chosen workspace.
func (opencodeConsole) console(ctx context.Context, c *core.Credentials, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opencodeServer+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token(c))
	if c.AccountID != "" {
		req.Header.Set("x-org-id", c.AccountID)
	}
	return doJSON(req, out)
}

func (o opencodeConsole) Fetch(ctx context.Context, c *core.Credentials) (*core.Result, error) {
	if c.AccessToken == "" {
		if c.APIKey == "" {
			return nil, errors.New("no credentials stored")
		}
		return o.fetch(ctx, c)
	}
	return withRefresh(ctx, c, o.refresh, o.fetch)
}

type opencodeMeter struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt"`
}

type opencodeBudget struct {
	RuleID       string  `json:"ruleID"`
	LimitDollars float64 `json:"limitDollars"`
	SpentDollars float64 `json:"spentDollars"`
	Scope        string  `json:"scope"`    // each | combined
	Per          string  `json:"per"`      // set | model
	Coverage     string  `json:"coverage"` // all | models
	Models       []struct {
		Name string `json:"name"`
	} `json:"models"`
	Vendors []struct {
		Name string `json:"name"`
	} `json:"vendors"`
	ResetsAt string `json:"resetsAt"`
}

func (o opencodeConsole) fetch(ctx context.Context, c *core.Credentials) (*core.Result, error) {
	res := &core.Result{}
	now := time.Now()

	// Go limits. 403 EntitlementError = this workspace has no Go subscription.
	noGo := false
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opencodeOrigin()+"/zen/go/v1/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token(c))
	if c.AccountID != "" {
		req.Header.Set("x-opencode-org-id", c.AccountID)
	}
	var usage struct {
		Usage struct {
			Rolling *opencodeMeter `json:"rolling"`
			Weekly  *opencodeMeter `json:"weekly"`
			Monthly *opencodeMeter `json:"monthly"`
		} `json:"usage"`
	}
	err = doJSON(req, &usage)
	var he *HTTPError
	switch {
	case errors.As(err, &he) && he.Status == http.StatusForbidden && strings.Contains(he.Body, "EntitlementError"):
		noGo = true
	case err != nil:
		return nil, err // 401 here lets withRefresh renew the token
	default:
		for _, m := range []struct {
			name   string
			period time.Duration
			meter  *opencodeMeter
		}{
			{"5h", 5 * time.Hour, usage.Usage.Rolling},
			{"weekly", 0, usage.Usage.Weekly},
			{"monthly", 0, usage.Usage.Monthly},
		} {
			if m.meter == nil {
				continue
			}
			q := core.Quota{Window: m.name, UsedPercent: math.Max(0, math.Min(100, m.meter.Percent))}
			if t, err := time.Parse(time.RFC3339, m.meter.ResetsAt); err == nil {
				q.ResetsAt = t.Unix()
				// An untouched rolling window reports "now + 5h", re-derived on every request.
				// That isn't a real reset, so drop it (as OpenUsage does).
				if m.period > 0 && m.meter.Percent == 0 && math.Abs(t.Sub(now).Seconds()-m.period.Seconds()) <= 5 {
					q.ResetsAt = 0
				}
			}
			if m.meter.Status == "rate-limited" {
				q.Detail = "rate-limited"
			}
			res.Quotas = append(res.Quotas, q)
		}
	}

	// Console spend budgets (cover Zen pay-as-you-go spend). 403 = none for this member/key.
	var budgets []opencodeBudget
	if err := o.console(ctx, c, "/api/policies/current-budgets", &budgets); err == nil {
		for _, b := range budgets {
			if b.LimitDollars <= 0 {
				continue
			}
			q := core.Quota{
				Window:      budgetLabel(b),
				UsedPercent: b.SpentDollars / b.LimitDollars * 100,
				Detail:      fmt.Sprintf("$%.2f of $%.2f", b.SpentDollars, b.LimitDollars),
			}
			if t, err := time.Parse(time.RFC3339, b.ResetsAt); err == nil {
				q.ResetsAt = t.Unix()
			}
			res.Quotas = append(res.Quotas, q)
		}
	}

	// The plan name needs a Console sign-in; an API key can't read it.
	if c.AccessToken != "" && !noGo {
		var st struct {
			Product string `json:"product"`
		}
		if o.console(ctx, c, "/api/go/status", &st) == nil {
			switch st.Product {
			case "go":
				res.Plan = "Go"
			case "go-plus":
				res.Plan = "Go Plus"
			case "go-teams":
				res.Plan = "Go Teams"
			}
		}
	}

	if noGo && len(res.Quotas) == 0 {
		return nil, errors.New("this workspace has no OpenCode Go subscription and no Console budgets to track")
	}
	return res, nil
}

// budgetLabel names a budget stably (it is also the history key), without amounts.
func budgetLabel(b opencodeBudget) string {
	label := "budget"
	if b.Coverage == "models" {
		var names []string
		for _, m := range b.Models {
			names = append(names, m.Name)
		}
		for _, v := range b.Vendors {
			names = append(names, v.Name)
		}
		if len(names) > 3 {
			names = append(names[:3], fmt.Sprintf("+%d", len(names)-3))
		}
		if len(names) > 0 {
			label += ": " + strings.Join(names, ", ")
		}
	}
	if b.Per == "model" {
		label += " (per model)"
	}
	if b.Scope == "combined" {
		label += " (team)"
	}
	return label
}
