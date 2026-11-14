// Package web serves the dashboard and account management UI.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai-usage-tracker/internal/core"
	"ai-usage-tracker/internal/poller"
	"ai-usage-tracker/internal/providers"
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
}

type Server struct {
	st       *store.Store
	box      *secrets.Box
	pl       *poller.Poller
	password string
	pages    map[string]*template.Template

	mu      sync.Mutex
	pending map[string]pending
}

func New(st *store.Store, box *secrets.Box, pl *poller.Poller, password string) (http.Handler, error) {
	s := &Server{st: st, box: box, pl: pl, password: password, pages: map[string]*template.Template{}, pending: map[string]pending{}}
	for _, page := range []string{"dashboard", "new", "oauth", "account"} {
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
	mux.HandleFunc("GET /accounts/{id}", s.accountDetail)
	mux.HandleFunc("POST /accounts/{id}/poll", s.pollAccount)
	mux.HandleFunc("POST /accounts/{id}/delete", s.deleteAccount)
	mux.HandleFunc("GET /api/accounts", s.apiAccounts)

	return s.auth(http.NewCrossOriginProtection().Handler(mux)), nil
}

func (s *Server) auth(next http.Handler) http.Handler {
	if s.password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
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
	APIProviders   []providers.APIKeyProvider
	OAuthProviders []providers.OAuthProvider
	Error          string
}

func (s *Server) newPageData(errMsg string) newPage {
	p := newPage{Error: errMsg}
	for _, prov := range providers.All() {
		switch x := prov.(type) {
		case providers.OAuthProvider:
			p.OAuthProviders = append(p.OAuthProviders, x)
		case providers.APIKeyProvider:
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
	if !ok || prov.Kind() != providers.KindAPIKey {
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
		item := apiAccount{ID: a.ID, Name: a.Name, Provider: a.Provider, Plan: a.Plan, LastPoll: a.LastPoll, LastError: a.LastError}
		if v.IsOAuth {
			for _, q := range v.Quotas {
				item.Quotas = append(item.Quotas, apiQuota{Window: q.Window, UsedPercent: q.UsedPercent, ResetsAt: q.ResetsAt})
			}
		} else {
			item.CostToday, item.Cost30d, item.Tokens30d = &v.CostToday, &v.Cost30, &v.Tokens30
		}
		out = append(out, item)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
