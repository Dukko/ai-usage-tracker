package providers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"ai-usage-tracker/internal/core"
)

// openRouter tracks OpenRouter spend.
//
// With a management (provisioning) key it reads /activity: 30 days of per-model
// usage and cost. With a regular API key, /activity is not allowed, so it falls
// back to /key, which reports today's spend and the key's own limit; history then
// builds up one day at a time as the tracker polls. /credits (remaining balance)
// is read on a best-effort basis.
type openRouter struct{}

func init() { register(openRouter{}) }

var openRouterBase = envOr("OPENROUTER_API_URL", "https://openrouter.ai/api/v1")

func (openRouter) ID() string    { return "openrouter" }
func (openRouter) Label() string { return "OpenRouter" }
func (openRouter) Kind() string  { return KindAPIKey }
func (openRouter) KeyHint() string {
	return "management key for full history, or any API key: sk-or-v1-…"
}

func (o openRouter) get(ctx context.Context, key, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openRouterBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return doJSON(req, out)
}

func (o openRouter) Fetch(ctx context.Context, c *core.Credentials) (*core.Result, error) {
	res := &core.Result{}

	err := o.activity(ctx, c.APIKey, res)
	if isUnauthorized(err) {
		// Not a management key: fall back to what a regular key can see.
		err = o.keyInfo(ctx, c.APIKey, res)
	}
	if err != nil {
		return nil, err
	}

	var cr struct {
		Data struct {
			TotalCredits float64 `json:"total_credits"`
			TotalUsage   float64 `json:"total_usage"`
		} `json:"data"`
	}
	if o.get(ctx, c.APIKey, "/credits", &cr) == nil && cr.Data.TotalCredits > 0 {
		bal := cr.Data.TotalCredits - cr.Data.TotalUsage
		res.Balance = &bal
	}
	return res, nil
}

func (o openRouter) activity(ctx context.Context, key string, res *core.Result) error {
	var r struct {
		Data []struct {
			Date             string  `json:"date"`
			Model            string  `json:"model"`
			Usage            float64 `json:"usage"`
			Requests         int64   `json:"requests"`
			PromptTokens     int64   `json:"prompt_tokens"`
			CompletionTokens int64   `json:"completion_tokens"`
			ReasoningTokens  int64   `json:"reasoning_tokens"`
		} `json:"data"`
	}
	if err := o.get(ctx, key, "/activity", &r); err != nil {
		return err
	}
	usage := map[[2]string]*core.DailyUsage{}
	costs := map[string]float64{}
	for _, x := range r.Data {
		if len(x.Date) < 10 {
			continue
		}
		day := x.Date[:10]
		model := x.Model
		if model == "" {
			model = "unknown"
		}
		k := [2]string{day, model}
		u := usage[k]
		if u == nil {
			u = &core.DailyUsage{Day: day, Model: model}
			usage[k] = u
		}
		u.Input += x.PromptTokens
		u.Output += x.CompletionTokens + x.ReasoningTokens
		u.Requests += x.Requests
		costs[day] += x.Usage
	}
	for _, u := range usage {
		res.Usage = append(res.Usage, *u)
	}
	for day, usd := range costs {
		res.Costs = append(res.Costs, core.DailyCost{Day: day, USD: usd})
	}
	return nil
}

func (o openRouter) keyInfo(ctx context.Context, key string, res *core.Result) error {
	var r struct {
		Data struct {
			Label          string   `json:"label"`
			Limit          *float64 `json:"limit"`
			LimitRemaining *float64 `json:"limit_remaining"`
			LimitReset     *string  `json:"limit_reset"`
			UsageDaily     float64  `json:"usage_daily"`
			IsFreeTier     bool     `json:"is_free_tier"`
		} `json:"data"`
	}
	if err := o.get(ctx, key, "/key", &r); err != nil {
		return err
	}
	// usage_daily is spend for the current UTC day.
	res.Costs = append(res.Costs, core.DailyCost{Day: dayOf(time.Now()), USD: r.Data.UsageDaily})
	if r.Data.IsFreeTier {
		res.Plan = "free tier"
	}
	if d := r.Data; d.Limit != nil && *d.Limit > 0 && d.LimitRemaining != nil {
		q := core.Quota{Window: "key limit", UsedPercent: (*d.Limit - *d.LimitRemaining) / *d.Limit * 100}
		if d.LimitReset != nil && *d.LimitReset != "" {
			q.Window = fmt.Sprintf("key limit (%s)", *d.LimitReset)
			q.ResetsAt = nextReset(*d.LimitReset, time.Now().UTC())
		}
		res.Quotas = append(res.Quotas, q)
	}
	return nil
}

// nextReset returns when an OpenRouter key limit with the given period resets
// (limits reset at midnight UTC; weekly limits on Monday).
func nextReset(period string, now time.Time) int64 {
	midnight := now.Truncate(24 * time.Hour)
	switch period {
	case "daily":
		return midnight.AddDate(0, 0, 1).Unix()
	case "weekly":
		days := (8 - int(midnight.Weekday())) % 7
		if days == 0 {
			days = 7
		}
		return midnight.AddDate(0, 0, days).Unix()
	case "monthly":
		return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Unix()
	}
	return 0
}
