package web

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/core"
	"github.com/Dukko/ai-usage-tracker/internal/store"
)

const museProvider = "muse"

// museIn is the JSON body Muse pushes. Optional fields are pointers so a
// missing line stays missing instead of becoming zero.
type museIn struct {
	Source           string            `json:"source"`
	RecordedAt       string            `json:"recorded_at"`
	Plan             string            `json:"plan"`
	WeeklyLimit      *museInWeekly     `json:"weekly_limit"`
	AdditionalTokens *museInAdditional `json:"additional_tokens"`
}

type museInWeekly struct {
	PercentUsed *float64 `json:"percent_used"`
	ResetsAt    *string  `json:"resets_at"`
}

type museInAdditional struct {
	PercentUsed     *float64 `json:"percent_used"`
	TokensRemaining *int64   `json:"tokens_remaining"`
}

// museOut mirrors the contract, with times normalised to UTC.
type museOut struct {
	Source           string             `json:"source"`
	RecordedAt       string             `json:"recorded_at"`
	Plan             string             `json:"plan,omitempty"`
	WeeklyLimit      *museOutWeekly     `json:"weekly_limit,omitempty"`
	AdditionalTokens *museOutAdditional `json:"additional_tokens,omitempty"`
}

type museOutWeekly struct {
	PercentUsed *float64 `json:"percent_used,omitempty"`
	ResetsAt    string   `json:"resets_at,omitempty"`
}

type museOutAdditional struct {
	PercentUsed     *float64 `json:"percent_used,omitempty"`
	TokensRemaining *int64   `json:"tokens_remaining,omitempty"`
}

// museIngest accepts a usage snapshot pushed by Muse. Auth is a bearer token
// compared against MUSE_INGEST_TOKEN. Each push is appended; a repeated
// (source, recorded_at) is returned from storage rather than stored twice.
func (s *Server) museIngest(w http.ResponseWriter, r *http.Request) {
	if !s.museAuthorized(r) {
		jsonError(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}

	var in museIn
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		jsonError(w, http.StatusBadRequest, "unexpected data after the JSON object")
		return
	}

	if !strings.EqualFold(strings.TrimSpace(in.Source), museProvider) {
		jsonError(w, http.StatusBadRequest, `source must be "muse"`)
		return
	}
	if strings.TrimSpace(in.RecordedAt) == "" {
		jsonError(w, http.StatusBadRequest, "recorded_at is required")
		return
	}
	recordedAt, err := museTime(in.RecordedAt)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "recorded_at must be RFC 3339: "+err.Error())
		return
	}
	snap := core.MuseSnapshot{
		Source:     museProvider,
		RecordedAt: recordedAt,
		Plan:       strings.TrimSpace(in.Plan),
		ReceivedAt: time.Now().Unix(),
	}
	if len(snap.Plan) > 100 {
		jsonError(w, http.StatusBadRequest, "plan is too long")
		return
	}
	if in.WeeklyLimit != nil {
		if p := in.WeeklyLimit.PercentUsed; p != nil {
			if err := validMusePercent(*p); err != nil {
				jsonError(w, http.StatusBadRequest, "weekly_limit.percent_used "+err.Error())
				return
			}
			snap.WeeklyPercent = p
		}
		if in.WeeklyLimit.ResetsAt != nil {
			if strings.TrimSpace(*in.WeeklyLimit.ResetsAt) == "" {
				jsonError(w, http.StatusBadRequest, "weekly_limit.resets_at must not be empty")
				return
			}
			t, err := museTime(*in.WeeklyLimit.ResetsAt)
			if err != nil {
				jsonError(w, http.StatusBadRequest, "weekly_limit.resets_at must be RFC 3339: "+err.Error())
				return
			}
			snap.WeeklyResetsAt = t
		}
	}
	if in.AdditionalTokens != nil {
		if p := in.AdditionalTokens.PercentUsed; p != nil {
			if err := validMusePercent(*p); err != nil {
				jsonError(w, http.StatusBadRequest, "additional_tokens.percent_used "+err.Error())
				return
			}
			snap.AdditionalPercent = p
		}
		if n := in.AdditionalTokens.TokensRemaining; n != nil {
			if *n < 0 {
				jsonError(w, http.StatusBadRequest, "additional_tokens.tokens_remaining must not be negative")
				return
			}
			snap.AdditionalTokensRemaining = n
		}
	}

	a, err := s.museAccount()
	if err != nil {
		slog.Error("open muse account", "err", err)
		jsonError(w, http.StatusInternalServerError, "could not open the Muse account")
		return
	}
	stored, created, err := s.st.SaveMuseSnapshot(a.ID, snap)
	if err != nil {
		slog.Error("save muse snapshot", "err", err)
		jsonError(w, http.StatusInternalServerError, "could not store the snapshot")
		return
	}

	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(museRecord(stored))
}

func (s *Server) museAuthorized(r *http.Request) bool {
	if s.museToken == "" {
		return false
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	tok = strings.TrimSpace(tok)
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.museToken)) == 1
}

// museAccount returns Muse's account, creating it when the first snapshot lands.
func (s *Server) museAccount() (store.Account, error) {
	a, err := s.st.FindAccountByProvider(museProvider)
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return a, err
	}
	s.mu.Lock() // two first pushes must not create two accounts
	defer s.mu.Unlock()
	a, err = s.st.FindAccountByProvider(museProvider)
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return a, err
	}
	id, err := s.st.CreateAccount("Muse", museProvider, []byte{})
	if err != nil {
		return a, err
	}
	return s.st.GetAccount(id)
}

func museTime(raw string) (int64, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return 0, err
	}
	if t.IsZero() {
		return 0, errors.New("time is zero")
	}
	return t.UTC().Unix(), nil
}

func validMusePercent(p float64) error {
	if p < 0 || p > 100 {
		return errors.New("must be between 0 and 100")
	}
	return nil
}

func museRecord(m core.MuseSnapshot) museOut {
	out := museOut{
		Source:     m.Source,
		RecordedAt: time.Unix(m.RecordedAt, 0).UTC().Format(time.RFC3339),
		Plan:       m.Plan,
	}
	if m.WeeklyPercent != nil || m.WeeklyResetsAt != 0 {
		w := &museOutWeekly{PercentUsed: m.WeeklyPercent}
		if m.WeeklyResetsAt != 0 {
			w.ResetsAt = time.Unix(m.WeeklyResetsAt, 0).UTC().Format(time.RFC3339)
		}
		out.WeeklyLimit = w
	}
	if m.AdditionalPercent != nil || m.AdditionalTokensRemaining != nil {
		out.AdditionalTokens = &museOutAdditional{
			PercentUsed:     m.AdditionalPercent,
			TokensRemaining: m.AdditionalTokensRemaining,
		}
	}
	return out
}
