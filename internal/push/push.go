// Package push implements the client side of push accounts: it reads usage
// from local tools and sends it to the tracker's /api/ingest endpoint.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Row is one day × model of usage, as sent to /api/ingest.
type Row struct {
	Day        string  `json:"day"` // YYYY-MM-DD, UTC
	Model      string  `json:"model"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Requests   int64   `json:"requests"`
	CostUSD    float64 `json:"cost_usd"`
}

type Payload struct {
	Source string `json:"source"`
	Rows   []Row  `json:"rows"`
}

// Send POSTs the payload to <baseURL>/api/ingest.
func Send(ctx context.Context, baseURL, token string, p Payload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	url := strings.TrimRight(baseURL, "/") + "/api/ingest"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
