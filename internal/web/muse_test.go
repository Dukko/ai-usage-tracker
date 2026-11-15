package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func musePost(t *testing.T, e *env, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/usage/muse", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func TestMuseIngest(t *testing.T) {
	const token = "6f7a2f0c9e1b4d8a"
	e := newEnv(t, Config{AdminPassword: "correct horse", MuseToken: token})
	recorded := time.Now().UTC().Truncate(time.Second)
	resets := recorded.Add(5 * 24 * time.Hour)
	full := fmt.Sprintf(`{"source":"muse","recorded_at":%q,"plan":"free",
		"weekly_limit":{"percent_used":12,"resets_at":%q},
		"additional_tokens":{"percent_used":1,"tokens_remaining":1900000000}}`,
		recorded.Format(time.RFC3339), resets.Format(time.RFC3339))

	for _, tc := range []struct {
		name, token, body string
		want              int
	}{
		{"no token", "", full, http.StatusUnauthorized},
		{"wrong token", "nope", full, http.StatusUnauthorized},
		{"malformed JSON", token, `{"source":`, http.StatusBadRequest},
		{"wrong source", token, `{"source":"other","recorded_at":"2026-11-15T23:36:00Z"}`, http.StatusBadRequest},
		{"missing recorded_at", token, `{"source":"muse"}`, http.StatusBadRequest},
		{"bad recorded_at", token, `{"source":"muse","recorded_at":"yesterday"}`, http.StatusBadRequest},
		{"percent out of range", token, `{"source":"muse","recorded_at":"2026-11-15T23:36:00Z","weekly_limit":{"percent_used":101}}`, http.StatusBadRequest},
		{"negative tokens", token, `{"source":"muse","recorded_at":"2026-11-15T23:36:00Z","additional_tokens":{"tokens_remaining":-1}}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := musePost(t, e, tc.token, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if w.Header().Get("Content-Type") != "application/json" {
				t.Errorf("content type %q", w.Header().Get("Content-Type"))
			}
		})
	}

	w := musePost(t, e, token, full)
	if w.Code != http.StatusCreated {
		t.Fatalf("valid push: %d %s", w.Code, w.Body.String())
	}
	for _, want := range []string{`"source":"muse"`, `"plan":"free"`, `"percent_used":12`,
		`"tokens_remaining":1900000000`, recorded.Format(time.RFC3339)} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("response missing %q: %s", want, w.Body.String())
		}
	}

	a, err := e.st.FindAccountByProvider("muse")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "Muse" || a.Provider != "muse" || a.Plan != "free" {
		t.Errorf("muse account = %+v", a)
	}
	quotas, err := e.st.LatestQuotas(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(quotas) != 2 {
		t.Fatalf("want weekly and additional windows, got %+v", quotas)
	}
	byWindow := map[string]int{}
	for _, q := range quotas {
		byWindow[q.Window] = int(q.UsedPercent)
	}
	if byWindow["weekly"] != 12 || byWindow["additional tokens"] != 1 {
		t.Errorf("quotas = %+v", quotas)
	}

	// A retry of the same reading is accepted but not stored twice.
	if w := musePost(t, e, token, full); w.Code != http.StatusOK {
		t.Errorf("duplicate push: %d", w.Code)
	}
	if hist, _ := e.st.QuotaHistory(a.ID, "weekly", time.Unix(0, 0)); len(hist) != 1 {
		t.Errorf("duplicate stored %d weekly points", len(hist))
	}

	// A partial push keeps the weekly limit and only updates what it sent.
	later := recorded.Add(6 * time.Hour)
	partial := fmt.Sprintf(`{"source":"muse","recorded_at":%q,"plan":"power",
		"additional_tokens":{"percent_used":2,"tokens_remaining":1800000000}}`, later.Format(time.RFC3339))
	if w := musePost(t, e, token, partial); w.Code != http.StatusCreated {
		t.Fatalf("partial push: %d %s", w.Code, w.Body.String())
	}
	quotas, err = e.st.LatestQuotas(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	byWindow = map[string]int{}
	for _, q := range quotas {
		byWindow[q.Window] = int(q.UsedPercent)
		if q.Window == "weekly" && q.ResetsAt != resets.Unix() {
			t.Errorf("weekly reset lost: %+v", q)
		}
		if q.Window == "additional tokens" && !strings.Contains(q.Detail, "1.80B tokens left (approximate)") {
			t.Errorf("additional detail = %q", q.Detail)
		}
	}
	if byWindow["weekly"] != 12 || byWindow["additional tokens"] != 2 {
		t.Errorf("quotas after partial push = %+v", quotas)
	}
	if a, _ := e.st.FindAccountByProvider("muse"); a.Plan != "power" {
		t.Errorf("plan not updated: %+v", a)
	}

	// It shows up on the dashboard and account page like any subscription.
	c := e.login("admin", "correct horse", "10.0.0.1")
	for _, path := range []string{"/", fmt.Sprintf("/accounts/%d", a.ID), "/accounts", "/settings"} {
		w := e.do("GET", path, "10.0.0.1", c, nil)
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		if m := templateErr.FindString(w.Body.String()); m != "" {
			t.Errorf("GET %s: found %q", path, m)
		}
	}
	body := e.do("GET", "/", "10.0.0.1", c, nil).Body.String()
	for _, want := range []string{"Muse", "Weekly", "Additional tokens", "1.80B tokens left (approximate)", "resets on"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	acct := e.do("GET", fmt.Sprintf("/accounts/%d", a.ID), "10.0.0.1", c, nil).Body.String()
	for _, want := range []string{"Limit windows", "Last push", "Additional tokens"} {
		if !strings.Contains(acct, want) {
			t.Errorf("account page missing %q", want)
		}
	}
	if strings.Contains(acct, "New push token") || strings.Contains(acct, "Refresh now") {
		t.Error("snapshot account should not offer token or poll actions")
	}
}

func TestMuseIngestWithoutTokenConfigured(t *testing.T) {
	e := newEnv(t, Config{AdminPassword: "correct horse"})
	w := musePost(t, e, "anything", `{"source":"muse","recorded_at":"2026-11-15T23:36:00Z"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured ingest: %d %s", w.Code, w.Body.String())
	}
	if _, err := e.st.FindAccountByProvider("muse"); err == nil {
		t.Error("rejected push created an account")
	}
}
