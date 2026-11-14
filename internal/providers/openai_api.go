package providers

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/core"
)

// openaiAPI polls the OpenAI organization Usage and Costs APIs.
// Requires an Admin key (sk-admin-...), created under Organization → Admin keys.
type openaiAPI struct{}

func init() { register(openaiAPI{}) }

func (openaiAPI) ID() string      { return "openai-api" }
func (openaiAPI) Label() string   { return "OpenAI API" }
func (openaiAPI) Kind() string    { return KindAPIKey }
func (openaiAPI) KeyHint() string { return "Admin key: sk-admin-…" }

const openaiBase = "https://api.openai.com"

func (o openaiAPI) get(ctx context.Context, key, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openaiBase+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return doJSON(req, out)
}

func (o openaiAPI) Fetch(ctx context.Context, c *core.Credentials) (*core.Result, error) {
	start := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -30)
	startUnix := strconv.FormatInt(start.Unix(), 10)
	res := &core.Result{}

	usage := map[[2]string]*core.DailyUsage{}
	page := ""
	for {
		q := url.Values{}
		q.Set("start_time", startUnix)
		q.Set("bucket_width", "1d")
		q.Set("group_by", "model")
		q.Set("limit", "31")
		if page != "" {
			q.Set("page", page)
		}
		var r struct {
			Data []struct {
				StartTime int64 `json:"start_time"`
				Results   []struct {
					Input    int64   `json:"input_tokens"`
					Cached   int64   `json:"input_cached_tokens"`
					Output   int64   `json:"output_tokens"`
					Requests int64   `json:"num_model_requests"`
					Model    *string `json:"model"`
				} `json:"results"`
			} `json:"data"`
			HasMore  bool    `json:"has_more"`
			NextPage *string `json:"next_page"`
		}
		if err := o.get(ctx, c.APIKey, "/v1/organization/usage/completions", q, &r); err != nil {
			return nil, err
		}
		for _, b := range r.Data {
			day := dayOf(time.Unix(b.StartTime, 0))
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
				u.Input += x.Input - x.Cached // input_tokens includes cached tokens
				u.CacheRead += x.Cached
				u.Output += x.Output
				u.Requests += x.Requests
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

	costs := map[string]float64{}
	page = ""
	for {
		q := url.Values{}
		q.Set("start_time", startUnix)
		q.Set("bucket_width", "1d")
		q.Set("limit", "31")
		if page != "" {
			q.Set("page", page)
		}
		var r struct {
			Data []struct {
				StartTime int64 `json:"start_time"`
				Results   []struct {
					Amount struct {
						Value    float64 `json:"value"`
						Currency string  `json:"currency"`
					} `json:"amount"`
				} `json:"results"`
			} `json:"data"`
			HasMore  bool    `json:"has_more"`
			NextPage *string `json:"next_page"`
		}
		if err := o.get(ctx, c.APIKey, "/v1/organization/costs", q, &r); err != nil {
			return nil, err
		}
		for _, b := range r.Data {
			day := dayOf(time.Unix(b.StartTime, 0))
			for _, x := range b.Results {
				costs[day] += x.Amount.Value
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
