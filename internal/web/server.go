// Package web serves the dashboard and account management UI.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai-usage-tracker/internal/core"
	"ai-usage-tracker/internal/poller"
	"ai-usage-tracker/internal/providers"
	"ai-usage-tracker/internal/push"
	"ai-usage-tracker/internal/secrets"
	"ai-usage-tracker/internal/store"
)

//go:embed templates static
var assets embed.FS

type pending struct {
	providers.PendingAuth
	Provider string
	Name     string
	Created  time.Time
	Device   *providers.DeviceStart // set for device-code sign-ins
}

type Server struct {
	st        *store.Store
	box       *secrets.Box
	pl        *poller.Poller
	password  string
	publicURL string
	pages     map[string]*template.Template

	mu      sync.Mutex
	pending map[string]pending
}

func New(st *store.Store, box *secrets.Box, pl *poller.Poller, password string) (http.Handler, error) {
	s := &Server{st: st, box: box, pl: pl, password: password, pages: map[string]*template.Template{}, pending: map[string]pending{},
		publicURL: strings.TrimRight(os.Getenv("TRACKER_PUBLIC_URL"), "/")}
	for _, page := range []string{"dashboard", "new", "oauth", "account", "push", "device"} {
		t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/base.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		s.pages[page] = t
	}

	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /accounts/new", s.newAccount)
	mux.HandleFunc("POST /accounts/apikey", s.createAPIKey)
	mux.HandleFunc("POST /accounts/import", s.importCreds)
	mux.HandleFunc("POST /oauth/start", s.oauthStart)
	mux.HandleFunc("POST /oauth/finish", s.oauthFinish)
	mux.HandleFunc("GET /oauth/device/{pending}", s.devicePoll)
	mux.HandleFunc("GET /accounts/{id}", s.accountDetail)
	mux.HandleFunc("POST /accounts/{id}/poll", s.pollAccount)
	mux.HandleFunc("POST /accounts/{id}/delete", s.deleteAccount)
	mux.HandleFunc("POST /accounts/push", s.createPush)
	mux.HandleFunc("POST /accounts/{id}/token", s.regenerateToken)
	mux.HandleFunc("GET /api/accounts", s.apiAccounts)
	mux.HandleFunc("POST /api/ingest", s.ingest)

	return s.auth(http.NewCrossOriginProtection().Handler(mux)), nil
}

func (s *Server) auth(next http.Handler) http.Handler {
	if s.password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /api/ingest authenticates with its own per-account token.
		if r.URL.Path == "/healthz" || r.URL.Path == "/api/ingest" {
			next.ServeHTTP(w, r)
			return
		}
		_, pw, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pw), []byte(s.password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="ai-usage-tracker", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	var buf strings.Builder
	if err := s.pages[page].ExecuteTemplate(&buf, "base", data); err != nil {
		slog.Error("render", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(buf.String()))
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

// ---- pages ----

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.st.ListAccounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views := make([]accountView, 0, len(accounts))
	var total30 float64
	for _, a := range accounts {
		v, err := s.buildView(a, false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		total30 += v.Cost30
		views = append(views, v)
	}
	s.render(w, "dashboard", map[string]any{"Accounts": views, "Total30": total30})
}

func (s *Server) accountDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a, err := s.st.GetAccount(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	v, err := s.buildView(a, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "account", v)
}

type newPage struct {
	APIProviders    []providers.APIKeyProvider
	OAuthProviders  []providers.OAuthProvider // paste-code sign-in + CLI credential import
	SignInProviders []providers.Provider      // everything offered under "Connect"
	PushProviders   []providers.PushProvider
	Error           string
}

func (s *Server) newPageData(errMsg string) newPage {
	p := newPage{Error: errMsg}
	for _, prov := range providers.All() {
		// A provider can offer several ways in (e.g. OpenCode: device sign-in or API key).
		if x, ok := prov.(providers.OAuthProvider); ok {
			p.OAuthProviders = append(p.OAuthProviders, x)
			p.SignInProviders = append(p.SignInProviders, prov)
		}
		if _, ok := prov.(providers.DeviceProvider); ok {
			p.SignInProviders = append(p.SignInProviders, prov)
		}
		if x, ok := prov.(providers.PushProvider); ok {
			p.PushProviders = append(p.PushProviders, x)
		}
		if x, ok := prov.(providers.APIKeyProvider); ok {
			p.APIProviders = append(p.APIProviders, x)
		}
	}
	return p
}

func (s *Server) newAccount(w http.ResponseWriter, r *http.Request) {
	s.render(w, "new", s.newPageData(""))
}

func (s *Server) newAccountError(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, "new", s.newPageData(msg))
}

// ---- account creation ----

func (s *Server) saveAccount(ctx context.Context, name string, prov providers.Provider, c *core.Credentials) (int64, error) {
	if name = strings.TrimSpace(name); name == "" {
		name = prov.Label()
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return 0, err
	}
	sealed, err := s.box.Seal(plain)
	if err != nil {
		return 0, err
	}
	id, err := s.st.CreateAccount(name, prov.ID(), sealed)
	if err != nil {
		return 0, err
	}
	// Poll right away so the dashboard has data (or shows the error) immediately.
	pollCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	_ = s.pl.Poll(pollCtx, id)
	return id, nil
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	prov, ok := providers.Get(r.FormValue("provider"))
	if _, takesKey := prov.(providers.APIKeyProvider); !ok || !takesKey {
		s.newAccountError(w, "Unknown API provider.")
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		s.newAccountError(w, "API key is required.")
		return
	}
	id, err := s.saveAccount(r.Context(), r.FormValue("name"), prov, &core.Credentials{APIKey: key})
	if err != nil {
		s.newAccountError(w, err.Error())
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/accounts/%d", id), http.StatusSeeOther)
}

func (s *Server) importCreds(w http.ResponseWriter, r *http.Request) {
	prov, ok := providers.Get(r.FormValue("provider"))
	op, isOAuth := prov.(providers.OAuthProvider)
	if !ok || !isOAuth {
		s.newAccountError(w, "Unknown subscription provider.")
		return
	}
	c, err := op.ImportJSON([]byte(r.FormValue("json")))
	if err != nil {
		s.newAccountError(w, "Could not read credentials: "+err.Error())
		return
	}
	id, err := s.saveAccount(r.Context(), r.FormValue("name"), prov, c)
	if err != nil {
		s.newAccountError(w, err.Error())
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/accounts/%d", id), http.StatusSeeOther)
}

type oauthPage struct {
	PendingID    string
	Label        string
	AuthURL      string
	Instructions string
	Error        string
}

func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	prov, ok := providers.Get(r.FormValue("provider"))
	if dp, isDevice := prov.(providers.DeviceProvider); ok && isDevice {
		s.deviceStart(w, r, dp)
		return
	}
	op, isOAuth := prov.(providers.OAuthProvider)
	if !ok || !isOAuth {
		s.newAccountError(w, "Unknown subscription provider.")
		return
	}
	pa := providers.NewPendingAuth()
	s.mu.Lock()
	for k, v := range s.pending {
		if time.Since(v.Created) > 30*time.Minute {
			delete(s.pending, k)
		}
	}
	s.pending[pa.State] = pending{PendingAuth: pa, Provider: prov.ID(), Name: r.FormValue("name"), Created: time.Now()}
	s.mu.Unlock()

	s.render(w, "oauth", oauthPage{PendingID: pa.State, Label: prov.Label(), AuthURL: op.AuthURL(pa), Instructions: op.Instructions()})
}

func (s *Server) oauthFinish(w http.ResponseWriter, r *http.Request) {
	pid := r.FormValue("pending")
	s.mu.Lock()
	p, ok := s.pending[pid]
	s.mu.Unlock()
	if !ok {
		s.newAccountError(w, "That login attempt expired. Please start again.")
		return
	}
	prov, _ := providers.Get(p.Provider)
	op := prov.(providers.OAuthProvider)

	c, err := op.Exchange(r.Context(), r.FormValue("code"), p.PendingAuth)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "oauth", oauthPage{PendingID: pid, Label: prov.Label(), AuthURL: op.AuthURL(p.PendingAuth),
			Instructions: op.Instructions(), Error: err.Error()})
		return
	}
	s.mu.Lock()
	delete(s.pending, pid)
	s.mu.Unlock()

	id, err := s.saveAccount(r.Context(), p.Name, prov, c)
	if err != nil {
		s.newAccountError(w, err.Error())
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/accounts/%d", id), http.StatusSeeOther)
}

// ---- account actions ----

func (s *Server) pollAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	_ = s.pl.Poll(ctx, id) // errors are recorded on the account and shown in the UI
	back := r.FormValue("back")
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.st.DeleteAccount(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---- JSON API ----

type apiQuota struct {
	Window      string  `json:"window"`
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at,omitempty"`
	Detail      string  `json:"detail,omitempty"`
}

type apiAccount struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Provider  string     `json:"provider"`
	Plan      string     `json:"plan,omitempty"`
	LastPoll  int64      `json:"last_poll"`
	LastError string     `json:"last_error,omitempty"`
	Quotas    []apiQuota `json:"quotas,omitempty"`
	CostToday *float64   `json:"cost_today_usd,omitempty"`
	Cost30d   *float64   `json:"cost_30d_usd,omitempty"`
	Tokens30d *int64     `json:"tokens_30d,omitempty"`
	Balance   *float64   `json:"balance_usd,omitempty"`
}

func (s *Server) apiAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.st.ListAccounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]apiAccount, 0, len(accounts))
	for _, a := range accounts {
		v, err := s.buildView(a, false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		item := apiAccount{ID: a.ID, Name: a.Name, Provider: a.Provider, Plan: a.Plan, LastPoll: a.LastPoll,
			LastError: a.LastError, Balance: a.Balance}
		for _, q := range v.Quotas {
			item.Quotas = append(item.Quotas, apiQuota{Window: q.Window, UsedPercent: q.UsedPercent, ResetsAt: q.ResetsAt, Detail: q.Detail})
		}
		if !v.IsOAuth {
			item.CostToday, item.Cost30d, item.Tokens30d = &v.CostToday, &v.Cost30, &v.Tokens30
		}
		out = append(out, item)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// ---- push accounts ----

type pushPage struct {
	ID      int64
	Name    string
	Label   string
	Command string
	Token   string
	URL     string
}

// newPushToken returns a token of the form "<account id>.<secret>" and the
// credentials holding the secret's hash. Only the hash is stored.
func newPushToken(id int64) (string, *core.Credentials, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	secret := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(secret))
	return fmt.Sprintf("%d.%s", id, secret), &core.Credentials{TokenHash: hex.EncodeToString(sum[:])}, nil
}

func (s *Server) baseURL(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// setPushToken issues a fresh token for the account and renders the setup page.
func (s *Server) setPushToken(w http.ResponseWriter, r *http.Request, a store.Account, prov providers.PushProvider) {
	token, creds, err := newPushToken(a.ID)
	if err == nil {
		var plain, sealed []byte
		if plain, err = json.Marshal(creds); err == nil {
			if sealed, err = s.box.Seal(plain); err == nil {
				err = s.st.UpdateSecret(a.ID, sealed)
			}
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "push", pushPage{ID: a.ID, Name: a.Name, Label: prov.Label(), Command: prov.PushCommand(), Token: token, URL: s.baseURL(r)})
}

func (s *Server) createPush(w http.ResponseWriter, r *http.Request) {
	prov, ok := providers.Get(r.FormValue("provider"))
	pp, isPush := prov.(providers.PushProvider)
	if !ok || !isPush {
		s.newAccountError(w, "Unknown local tool.")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = prov.Label()
	}
	// Placeholder secret; replaced with the real token hash once the id is known.
	id, err := s.st.CreateAccount(name, prov.ID(), []byte{})
	if err != nil {
		s.newAccountError(w, err.Error())
		return
	}
	a, err := s.st.GetAccount(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.setPushToken(w, r, a, pp)
}

func (s *Server) regenerateToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a, err := s.st.GetAccount(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	prov, _ := providers.Get(a.Provider)
	pp, isPush := prov.(providers.PushProvider)
	if !isPush {
		http.Error(w, "not a push account", http.StatusBadRequest)
		return
	}
	s.setPushToken(w, r, a, pp)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ingest accepts usage from push clients. Auth: "Authorization: Bearer <id>.<secret>".
func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	idStr, secret, ok2 := strings.Cut(token, ".")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if !ok || !ok2 || err != nil || secret == "" {
		jsonError(w, http.StatusUnauthorized, "missing or malformed token")
		return
	}
	a, err := s.st.GetAccount(id)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if prov, ok := providers.Get(a.Provider); !ok || prov.Kind() != providers.KindPush {
		jsonError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	var creds core.Credentials
	plain, err := s.box.Open(a.Secret)
	if err == nil {
		err = json.Unmarshal(plain, &creds)
	}
	sum := sha256.Sum256([]byte(secret))
	if err != nil || creds.TokenHash == "" ||
		subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(creds.TokenHash)) != 1 {
		jsonError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	var p push.Payload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&p); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(p.Rows) > 100_000 {
		jsonError(w, http.StatusRequestEntityTooLarge, "too many rows")
		return
	}
	res := &core.Result{}
	costs := map[string]float64{}
	for i, row := range p.Rows {
		if _, err := time.Parse(time.DateOnly, row.Day); err != nil {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("row %d: bad day %q", i, row.Day))
			return
		}
		if row.Model == "" || len(row.Model) > 300 || row.Input < 0 || row.Output < 0 ||
			row.CacheRead < 0 || row.CacheWrite < 0 || row.Requests < 0 || row.CostUSD < 0 {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("row %d: invalid values", i))
			return
		}
		res.Usage = append(res.Usage, core.DailyUsage{Day: row.Day, Model: row.Model, Input: row.Input, Output: row.Output,
			CacheRead: row.CacheRead, CacheWrite: row.CacheWrite, Requests: row.Requests})
		costs[row.Day] += row.CostUSD
	}
	for day, usd := range costs {
		res.Costs = append(res.Costs, core.DailyCost{Day: day, USD: usd})
	}
	if err := s.st.SaveResult(a.ID, res); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "rows": len(p.Rows)})
}

// ---- device-code sign-in ----

type devicePage struct {
	PendingID string
	Label     string
	UserCode  string
	VerifyURL string
	Refresh   int
	Error     string
}

func (s *Server) addPending(p pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.pending {
		if time.Since(v.Created) > 30*time.Minute {
			delete(s.pending, k)
		}
	}
	s.pending[p.State] = p
}

func (s *Server) deviceStart(w http.ResponseWriter, r *http.Request, dp providers.DeviceProvider) {
	start, err := dp.StartDevice(r.Context())
	if err != nil {
		s.newAccountError(w, "Could not start sign-in: "+err.Error())
		return
	}
	pa := providers.NewPendingAuth()
	s.addPending(pending{PendingAuth: pa, Provider: dp.ID(), Name: r.FormValue("name"), Created: time.Now(), Device: start})
	s.render(w, "device", devicePage{PendingID: pa.State, Label: dp.Label(), UserCode: start.UserCode,
		VerifyURL: start.VerifyURL, Refresh: start.Interval})
}

// devicePoll is hit by the waiting page's meta refresh; each visit polls the token endpoint once.
func (s *Server) devicePoll(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pending")
	s.mu.Lock()
	p, ok := s.pending[pid]
	s.mu.Unlock()
	if !ok || p.Device == nil || time.Since(p.Created) > time.Duration(p.Device.ExpiresIn)*time.Second {
		s.newAccountError(w, "That sign-in expired. Please start again.")
		return
	}
	prov, _ := providers.Get(p.Provider)
	dp := prov.(providers.DeviceProvider)
	page := devicePage{PendingID: pid, Label: prov.Label(), UserCode: p.Device.UserCode, VerifyURL: p.Device.VerifyURL, Refresh: p.Device.Interval}

	creds, label, err := dp.PollDevice(r.Context(), p.Device.DeviceCode)
	if errors.Is(err, providers.ErrAuthPending) {
		s.render(w, "device", page)
		return
	}
	s.mu.Lock()
	delete(s.pending, pid)
	s.mu.Unlock()
	if err != nil {
		s.newAccountError(w, err.Error())
		return
	}
	name := p.Name
	if strings.TrimSpace(name) == "" && label != "" {
		name = prov.Label() + " (" + label + ")"
	}
	id, err := s.saveAccount(r.Context(), name, prov, creds)
	if err != nil {
		s.newAccountError(w, err.Error())
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/accounts/%d", id), http.StatusSeeOther)
}
