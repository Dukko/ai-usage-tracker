package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-usage-tracker/internal/core"
)

func fakeOpenRouter(t *testing.T, mgmt bool) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity":
			if !mgmt {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":{"message":"management key required"}}`))
				return
			}
			w.Write([]byte(`{"data":[
				{"date":"2026-11-13","model":"a/m1","usage":0.5,"requests":3,"prompt_tokens":100,"completion_tokens":20,"reasoning_tokens":5},
				{"date":"2026-11-13","model":"a/m1","usage":0.25,"requests":1,"prompt_tokens":10,"completion_tokens":2,"reasoning_tokens":0},
				{"date":"2026-11-14","model":"b/m2","usage":1,"requests":2,"prompt_tokens":1,"completion_tokens":1,"reasoning_tokens":0}]}`))
		case "/key":
			w.Write([]byte(`{"data":{"label":"k","limit":10,"limit_remaining":7.5,"limit_reset":"daily","usage_daily":2.5,"is_free_tier":false}}`))
		case "/credits":
			w.Write([]byte(`{"data":{"total_credits":20,"total_usage":12.5}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := openRouterBase
	openRouterBase = srv.URL
	t.Cleanup(func() { openRouterBase = old })
}

func TestOpenRouterManagementKey(t *testing.T) {
	fakeOpenRouter(t, true)
	res, err := openRouter{}.Fetch(context.Background(), &core.Credentials{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Usage) != 2 || len(res.Costs) != 2 {
		t.Fatalf("usage=%d costs=%d", len(res.Usage), len(res.Costs))
	}
	for _, u := range res.Usage {
		if u.Model == "a/m1" && (u.Input != 110 || u.Output != 27 || u.Requests != 4) {
			t.Errorf("a/m1 not aggregated: %+v", u)
		}
	}
	for _, c := range res.Costs {
		if c.Day == "2026-11-13" && c.USD != 0.75 {
			t.Errorf("cost 11-13 = %v", c.USD)
		}
	}
	if res.Balance == nil || *res.Balance != 7.5 {
		t.Errorf("balance = %v", res.Balance)
	}
}

func TestOpenRouterRegularKeyFallback(t *testing.T) {
	fakeOpenRouter(t, false)
	res, err := openRouter{}.Fetch(context.Background(), &core.Credentials{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Costs) != 1 || res.Costs[0].USD != 2.5 || res.Costs[0].Day != dayOf(time.Now()) {
		t.Errorf("costs = %+v", res.Costs)
	}
	if len(res.Quotas) != 1 || res.Quotas[0].UsedPercent != 25 || res.Quotas[0].ResetsAt == 0 {
		t.Errorf("quotas = %+v", res.Quotas)
	}
}

func TestNextReset(t *testing.T) {
	wed := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC) // a Wednesday
	if got := time.Unix(nextReset("weekly", wed), 0).UTC(); got != time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC) {
		t.Errorf("weekly from Wed = %v", got)
	}
	mon := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	if got := time.Unix(nextReset("weekly", mon), 0).UTC(); got != time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC) {
		t.Errorf("weekly from Mon = %v", got)
	}
	if got := time.Unix(nextReset("monthly", time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC)), 0).UTC(); got != time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) {
		t.Errorf("monthly = %v", got)
	}
}
