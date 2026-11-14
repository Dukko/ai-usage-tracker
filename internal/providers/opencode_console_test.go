package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/core"
)

type fakeConsole struct {
	noGo      bool
	polls     atomic.Int32
	refreshed atomic.Int32
	validTok  atomic.Value // string
}

func (f *fakeConsole) start(t *testing.T) {
	f.validTok.Store("acc1")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /console/auth/device/code", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("client_id") != "opencode-cli" || r.FormValue("supports_org_scope") != "true" {
			http.Error(w, "bad", 400)
			return
		}
		w.Write([]byte(`{"device_code":"dev1","user_code":"ABCD-EFGH","verification_uri":"/console/device","expires_in":600,"interval":5}`))
	})
	mux.HandleFunc("POST /console/auth/device/token", func(w http.ResponseWriter, r *http.Request) {
		switch r.FormValue("grant_type") {
		case "urn:ietf:params:oauth:grant-type:device_code":
			if f.polls.Add(1) == 1 {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			w.Write([]byte(`{"access_token":"acc1","refresh_token":"ref1","expires_in":3600,"org_id":"wrk_1"}`))
		case "refresh_token":
			f.refreshed.Add(1)
			f.validTok.Store("acc2")
			w.Write([]byte(`{"access_token":"acc2","refresh_token":"ref2","expires_in":3600}`))
		}
	})
	auth := func(w http.ResponseWriter, r *http.Request, orgHeader string) bool {
		if r.Header.Get("Authorization") != "Bearer "+f.validTok.Load().(string) && r.Header.Get("Authorization") != "Bearer key1" {
			w.WriteHeader(401)
			w.Write([]byte(`{"type":"error","error":{"type":"AuthError","message":"Unauthorized"}}`))
			return false
		}
		if r.Header.Get("Authorization") != "Bearer key1" && r.Header.Get(orgHeader) != "wrk_1" {
			http.Error(w, "missing org", 400)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /console/api/user", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r, "x-org-id") {
			w.Write([]byte(`{"id":"u1","email":"me@example.com"}`))
		}
	})
	mux.HandleFunc("GET /zen/go/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r, "x-opencode-org-id") {
			return
		}
		if f.noGo {
			w.WriteHeader(403)
			w.Write([]byte(`{"type":"error","error":{"type":"EntitlementError","message":"no go"}}`))
			return
		}
		placeholder := time.Now().Add(5 * time.Hour).UTC().Format(time.RFC3339)
		w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":0,"resetsAt":"` + placeholder + `"},
			"weekly":{"status":"ok","percent":8,"resetsAt":"2030-07-13T00:00:00.662Z"},
			"monthly":{"status":"rate-limited","percent":100,"resetsAt":"2030-08-04T11:18:32Z"}}}`))
	})
	mux.HandleFunc("GET /console/api/go/status", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r, "x-org-id") {
			w.Write([]byte(`{"product":"go-plus"}`))
		}
	})
	mux.HandleFunc("GET /console/api/policies/current-budgets", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r, "x-org-id") {
			w.Write([]byte(`[{"ruleID":"r1","limitDollars":50,"spentDollars":12.3,"scope":"combined","per":"set","coverage":"all",
				"models":[],"vendors":[],"picked":false,"shared":true,"resetsAt":"2030-12-01T00:00:00Z"}]`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	old := opencodeServer
	opencodeServer = srv.URL + "/console"
	t.Cleanup(func() { opencodeServer = old })
}

func TestOpenCodeDeviceSignInAndFetch(t *testing.T) {
	f := &fakeConsole{}
	f.start(t)
	ctx := context.Background()
	p := opencodeConsole{}

	st, err := p.StartDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.UserCode != "ABCD-EFGH" || st.VerifyURL != opencodeServer+"/device" {
		t.Fatalf("start = %+v", st)
	}
	if _, _, err := p.PollDevice(ctx, st.DeviceCode); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("first poll: want pending, got %v", err)
	}
	c, label, err := p.PollDevice(ctx, st.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccountID != "wrk_1" || c.RefreshToken != "ref1" || label != "me@example.com" {
		t.Fatalf("creds = %+v label=%q", c, label)
	}

	res, err := p.Fetch(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan != "Go Plus" {
		t.Errorf("plan = %q", res.Plan)
	}
	got := map[string]core.Quota{}
	for _, q := range res.Quotas {
		got[q.Window] = q
	}
	if q := got["5h"]; q.UsedPercent != 0 || q.ResetsAt != 0 {
		t.Errorf("untouched rolling window should drop placeholder reset: %+v", q)
	}
	if q := got["monthly"]; q.UsedPercent != 100 || q.Detail != "rate-limited" {
		t.Errorf("monthly = %+v", q)
	}
	if q := got["budget (team)"]; q.UsedPercent < 24.5 || q.UsedPercent > 24.7 || q.Detail != "$12.30 of $50.00" || q.ResetsAt == 0 {
		t.Errorf("budget = %+v (all: %+v)", q, res.Quotas)
	}

	// Server-side token rotation: next fetch gets 401, refreshes, retries.
	f.validTok.Store("acc-new")
	f.refreshed.Store(0)
	c.ExpiresAt = time.Now().Add(time.Hour).Unix()
	if _, err := p.Fetch(ctx, c); err != nil {
		t.Fatal(err)
	}
	if f.refreshed.Load() != 1 || c.AccessToken != "acc2" || c.RefreshToken != "ref2" || c.AccountID != "wrk_1" {
		t.Errorf("refresh not applied: refreshed=%d creds=%+v", f.refreshed.Load(), c)
	}
}

func TestOpenCodeAPIKeyAndNoGo(t *testing.T) {
	f := &fakeConsole{}
	f.start(t)
	res, err := opencodeConsole{}.Fetch(context.Background(), &core.Credentials{APIKey: "key1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan != "" || len(res.Quotas) != 4 { // 3 Go windows + 1 budget; key can't read plan
		t.Errorf("api key result: plan=%q quotas=%+v", res.Plan, res.Quotas)
	}

	f.noGo = true
	res, err = opencodeConsole{}.Fetch(context.Background(), &core.Credentials{APIKey: "key1"})
	if err != nil || len(res.Quotas) != 1 {
		t.Errorf("no Go but a budget should still succeed: %v %+v", err, res)
	}
}

func TestBudgetLabel(t *testing.T) {
	b := opencodeBudget{Coverage: "models", Per: "model", Scope: "each"}
	for _, n := range []string{"A", "B", "C", "D"} {
		b.Models = append(b.Models, struct {
			Name string `json:"name"`
		}{n})
	}
	if got := budgetLabel(b); got != "budget: A, B, C, +1 (per model)" {
		t.Errorf("label = %q", got)
	}
}
