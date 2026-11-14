package providers

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"ai-usage-tracker/internal/core"
)

// anthropicAPI polls the Anthropic Admin API usage and cost reports.
// Requires an Admin API key (sk-ant-admin...), created in the Console under
// Settings → Admin keys.
type anthropicAPI struct{}

func init() { register(anthropicAPI{}) }

func (anthropicAPI) ID() string      { return "anthropic-api" }
func (anthropicAPI) Label() string   { return "Anthropic API" }
func (anthropicAPI) Kind() string    { return KindAPIKey }
func (anthropicAPI) KeyHint() string { return "Admin key: sk-ant-admin01-…" }

const anthropicBase = "https://api.anthropic.com"

func (a anthropicAPI) get(ctx context.Context, key, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicBase+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	return doJSON(req, out)
}

func (a anthropicAPI) Fetch(ctx context.Context, c *core.Credentials) (*core.Result, error) {
	start := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -30)
	res := &core.Result{}

	// Token usage, grouped by model, daily buckets.
	usage := map[[2]string]*core.DailyUsage{}
	page := ""
	for {
		q := url.Values{}
		q.Set("starting_at", start.Format(time.RFC3339))
		q.Set("bucket_width", "1d")
		q.Add("group_by[]", "model")
		q.Set("limit", "31")
		if page != "" {
			q.Set("page", page)
		}
		var r struct {
			Data []struct {
				StartingAt string `json:"starting_at"`
				Results    []struct {
					Uncached      int64 `json:"uncached_input_tokens"`
					CacheRead     int64 `json:"cache_read_input_tokens"`
					Output        int64 `json:"output_tokens"`
					CacheCreation struct {
						E1h int64 `json:"ephemeral_1h_input_tokens"`
						E5m int64 `json:"ephemeral_5m_input_tokens"`
					} `json:"cache_creation"`
					Model *string `json:"model"`
				} `json:"results"`
			} `json:"data"`
			HasMore  bool    `json:"has_more"`
			NextPage *string `json:"next_page"`
		}
		if err := a.get(ctx, c.APIKey, "/v1/organizations/usage_report/messages", q, &r); err != nil {
			return nil, err
		}
		for _, b := range r.Data {
			if len(b.StartingAt) < 10 {
				continue
			}
			day := b.StartingAt[:10]
			for _, x := range b.Results {
				model := "unknown"
				if x.Model != nil && *x.Model != "" {
					model = *x.Model
				}
				k := [2]string{day, model}
				u := usage[k]
				if u == nil {
					u = &core.DailyUsage{Day: day, Model: model}
					usage[k] = u
				}
				u.Input += x.Uncached
				u.Output += x.Output
				u.CacheRead += x.CacheRead
				u.CacheWrite += x.CacheCreation.E1h + x.CacheCreation.E5m
			}
		}
		if !r.HasMore || r.NextPage == nil || *r.NextPage == "" {
			break
		}
		page = *r.NextPage
	}
	for _, u := range usage {
		res.Usage = append(res.Usage, *u)
	}

	// Cost. Amounts are decimal strings in the lowest currency unit (cents).
	costs := map[string]float64{}
	page = ""
	for {
		q := url.Values{}
		q.Set("starting_at", start.Format(time.RFC3339))
		q.Set("bucket_width", "1d")
		q.Set("limit", "31")
		if page != "" {
			q.Set("page", page)
		}
		var r struct {
			Data []struct {
				StartingAt string `json:"starting_at"`
				Results    []struct {
					Amount   string `json:"amount"`
					Currency string `json:"currency"`
				} `json:"results"`
			} `json:"data"`
			HasMore  bool    `json:"has_more"`
			NextPage *string `json:"next_page"`
		}
		if err := a.get(ctx, c.APIKey, "/v1/organizations/cost_report", q, &r); err != nil {
			return nil, err
		}
		for _, b := range r.Data {
			if len(b.StartingAt) < 10 {
				continue
			}
			day := b.StartingAt[:10]
			for _, x := range b.Results {
				cents, err := strconv.ParseFloat(x.Amount, 64)
				if err == nil {
					costs[day] += cents / 100
				}
			}
		}
		if !r.HasMore || r.NextPage == nil || *r.NextPage == "" {
			break
		}
		page = *r.NextPage
	}
	for day, usd := range costs {
		res.Costs = append(res.Costs, core.DailyCost{Day: day, USD: usd})
	}
	return res, nil
}
