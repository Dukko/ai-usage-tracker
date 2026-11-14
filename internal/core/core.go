// Package core holds the types shared by providers, storage and the web UI.
package core

// Credentials is everything needed to talk to a provider for one account.
// It is stored encrypted at rest as JSON.
type Credentials struct {
	APIKey       string `json:"api_key,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // unix seconds
	AccountID    string `json:"account_id,omitempty"`
	Plan         string `json:"plan,omitempty"`
	// TokenHash is the sha256 (hex) of a push account's ingest token.
	TokenHash string `json:"token_hash,omitempty"`
}

// DailyUsage is token usage for one model on one UTC day.
// Input excludes cached tokens; CacheRead/CacheWrite are reported separately.
type DailyUsage struct {
	Day        string // YYYY-MM-DD
	Model      string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Requests   int64
}

// DailyCost is total spend on one UTC day.
type DailyCost struct {
	Day string
	USD float64
}

// Quota is one subscription rate-limit window (e.g. the 5-hour or weekly limit).
type Quota struct {
	Window      string
	UsedPercent float64
	ResetsAt    int64  // unix seconds, 0 if unknown
	Detail      string // optional extra text, e.g. "$12.30 of $50.00"
}

// Result is what a provider returns from one poll.
type Result struct {
	Usage  []DailyUsage
	Costs  []DailyCost
	Quotas []Quota
	Plan   string
	// Balance is remaining prepaid credit in USD, if the provider reports one.
	Balance *float64
}
