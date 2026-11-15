package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/core"
)

func TestSaveAndAggregate(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.CreateAccount("a", "anthropic-api", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format(time.DateOnly)
	res := &core.Result{
		Usage: []core.DailyUsage{
			{Day: today, Model: "m1", Input: 10, Output: 5},
			{Day: today, Model: "m2", Input: 1, CacheRead: 100},
		},
		Costs:  []core.DailyCost{{Day: today, USD: 1.5}},
		Quotas: []core.Quota{{Window: "5h", UsedPercent: 42}},
		Plan:   "max",
	}
	// Saving twice must upsert, not double count.
	for range 2 {
		if err := st.SaveResult(id, res); err != nil {
			t.Fatal(err)
		}
	}
	days, err := st.DailyTotals(id, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 30 {
		t.Fatalf("want 30 days, got %d", len(days))
	}
	last := days[29]
	if last.Day != today || last.USD != 1.5 || last.Tokens() != 116 {
		t.Errorf("unexpected today total: %+v", last)
	}
	q, err := st.LatestQuotas(id)
	if err != nil || len(q) != 1 || q[0].UsedPercent != 42 {
		t.Errorf("quotas = %+v, %v", q, err)
	}
	a, _ := st.GetAccount(id)
	if a.Plan != "max" || a.LastPoll == 0 {
		t.Errorf("account not updated: %+v", a)
	}
	if err := st.DeleteAccount(id); err != nil {
		t.Fatal(err)
	}
	if days, _ := st.DailyTotals(id, 1); days[0].Tokens() != 0 {
		t.Error("cascade delete failed")
	}
}

func TestMuseSnapshotAppendAndMerge(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.CreateAccount("Muse", "muse", []byte{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	pct12, pct1, pct2, pct20 := 12.0, 1.0, 2.0, 20.0
	left := int64(1_900_000_000)
	reset := now.Add(5 * 24 * time.Hour).Unix()
	full := core.MuseSnapshot{Source: "muse", RecordedAt: now.Unix(), Plan: "free", WeeklyPercent: &pct12,
		WeeklyResetsAt: reset, AdditionalPercent: &pct1, AdditionalTokensRemaining: &left}
	got, created, err := st.SaveMuseSnapshot(id, full)
	if err != nil || !created {
		t.Fatalf("first push: created=%v err=%v", created, err)
	}
	if got.RecordedAt != now.Unix() || got.Plan != "free" {
		t.Errorf("stored snapshot = %+v", got)
	}
	check := func(step string, wantWeekly, wantAdditional float64, wantDetail bool) {
		t.Helper()
		quotas, err := st.LatestQuotas(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(quotas) != 2 {
			t.Fatalf("%s: want 2 quota windows, got %+v", step, quotas)
		}
		var weekly, additional core.Quota
		for _, q := range quotas {
			switch q.Window {
			case "weekly":
				weekly = q
			case "additional tokens":
				additional = q
			}
		}
		if weekly.UsedPercent != wantWeekly || weekly.ResetsAt != reset {
			t.Errorf("%s: weekly = %+v", step, weekly)
		}
		if additional.UsedPercent != wantAdditional {
			t.Errorf("%s: additional = %+v", step, additional)
		}
		if wantDetail != strings.Contains(additional.Detail, "1.90B tokens left (approximate)") {
			t.Errorf("%s: detail = %q", step, additional.Detail)
		}
	}
	check("first push", 12, 1, true)

	// The same reading again is not stored twice.
	_, created, err = st.SaveMuseSnapshot(id, full)
	if err != nil || created {
		t.Fatalf("duplicate push: created=%v err=%v", created, err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM muse_snapshots WHERE account_id = ?`, id).Scan(&n); err != nil || n != 1 {
		t.Errorf("after duplicate: %d snapshots, %v", n, err)
	}
	check("after duplicate", 12, 1, true)

	// A partial push keeps the last known weekly window and token figure.
	later := now.Add(6 * time.Hour)
	partial := core.MuseSnapshot{Source: "muse", RecordedAt: later.Unix(), Plan: "power",
		AdditionalPercent: &pct2}
	if _, created, err := st.SaveMuseSnapshot(id, partial); err != nil || !created {
		t.Fatalf("partial push: created=%v err=%v", created, err)
	}
	check("partial push", 12, 2, true)

	// A new weekly value without a new reset time keeps the old reset.
	latest := later.Add(6 * time.Hour)
	if _, _, err := st.SaveMuseSnapshot(id, core.MuseSnapshot{Source: "muse", RecordedAt: latest.Unix(),
		WeeklyPercent: &pct20}); err != nil {
		t.Fatal(err)
	}
	check("weekly update", 20, 2, true)
	hist, err := st.QuotaHistory(id, "weekly", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 || hist[0].UsedPercent != 12 || hist[2].UsedPercent != 20 {
		t.Errorf("weekly history = %+v", hist)
	}
	if err := st.DeleteAccount(id); err != nil {
		t.Fatal(err)
	}
	if quotas, _ := st.LatestQuotas(id); len(quotas) != 0 {
		t.Error("muse snapshots were not removed with the account")
	}
}
