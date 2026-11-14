package push

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestReadOpenCodeV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	old := time.Now().AddDate(0, 0, -60).UnixMilli()
	for _, q := range []string{
		`CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, seq INT, time_created INT, time_updated INT, data TEXT)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ins := `INSERT INTO session_message VALUES (?, 's', ?, 0, ?, ?, ?)`
	msg := `{"model":{"id":"m1","providerID":"openrouter"},"cost":0.5,"tokens":{"input":10,"output":5,"reasoning":2,"cache":{"read":100,"write":3}}}`
	rows := [][]any{
		{"1", "assistant", now, now, msg},
		{"2", "assistant", now, now, msg},
		{"3", "assistant", old, old, msg},                                               // outside window
		{"4", "user", now, now, `{"tokens":{"input":999}}`},                             // not assistant
		{"5", "assistant", now, now, `{"model":{"id":"m1","providerID":"openrouter"}}`}, // in progress, no tokens
	}
	for _, r := range rows {
		if _, err := db.Exec(ins, r...); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	got, err := ReadOpenCode(context.Background(), path, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %+v", got)
	}
	r := got[0]
	if r.Model != "openrouter/m1" || r.Requests != 2 || r.Input != 20 || r.Output != 14 || r.CacheRead != 200 || r.CacheWrite != 6 || r.CostUSD != 1 {
		t.Errorf("unexpected row: %+v", r)
	}
}
