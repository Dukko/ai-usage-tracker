package web

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/store"
)

const (
	sessionCookie   = "aut_session"
	sessionShort    = 12 * time.Hour
	sessionLong     = 30 * 24 * time.Hour
	pbkdf2Iter      = 600_000
	maxFailures     = 5
	lockoutDuration = 15 * time.Minute
	minPasswordLen  = 8
)

// ---- password hashing ----

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func checkPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash keeps the timing of unknown-user logins close to real ones.
var dummyHash, _ = hashPassword("not-a-real-password")

func validPassword(pw string) error {
	if len([]rune(pw)) < minPasswordLen {
		return fmt.Errorf("Use at least %d characters.", minPasswordLen)
	}
	return nil
}

func validUsername(name string) error {
	if name == "" || len(name) > 64 {
		return errors.New("Usernames are 1 to 64 characters.")
	}
	for _, r := range name {
		if !(r == '-' || r == '_' || r == '.' || r == '@' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return errors.New("Usernames can use letters, numbers and - _ . @ only.")
		}
	}
	return nil
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---- login rate limiting ----

type limiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func newLimiter() *limiter { return &limiter{failures: map[string][]time.Time{}} }

func (l *limiter) recent(key string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range l.failures[key] {
		if now.Sub(t) < lockoutDuration {
			keep = append(keep, t)
		}
	}
	if keep == nil {
		delete(l.failures, key)
	} else {
		l.failures[key] = keep
	}
	return keep
}

// lockedFor returns how long the key stays locked, or 0.
func (l *limiter) lockedFor(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	r := l.recent(key, now)
	if len(r) < maxFailures {
		return 0
	}
	return lockoutDuration - now.Sub(r[len(r)-maxFailures])
}

// fail records a failure and returns how many attempts are left before lockout.
func (l *limiter) fail(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	r := append(l.recent(key, now), now)
	l.failures[key] = r
	return max(0, maxFailures-len(r))
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

// clientIP is the address used for rate limiting. Proxy headers are only
// trusted when TRACKER_TRUST_PROXY is set, since anyone can send them.
func (s *Server) clientIP(r *http.Request) string {
	if s.trustProxy {
		if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
			return ip
		}
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- sessions and request identity ----

type ctxKey struct{}

type identity struct {
	User        store.User
	SessionHash string
}

func currentUser(r *http.Request) (identity, bool) {
	id, ok := r.Context().Value(ctxKey{}).(identity)
	return id, ok
}

func (s *Server) isHTTPS(r *http.Request) bool {
	return r.TLS != nil || (s.trustProxy && r.Header.Get("X-Forwarded-Proto") == "https") ||
		strings.HasPrefix(s.publicURL, "https://")
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u store.User, remember bool) error {
	token := randomToken(32)
	now := time.Now()
	life := sessionShort
	if remember {
		life = sessionLong
	}
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if err := s.st.CreateSession(store.Session{IDHash: sha256Hex(token), UserID: u.ID, CreatedAt: now.Unix(), LastSeen: now.Unix(),
		ExpiresAt: now.Add(life).Unix(), UserAgent: ua, IP: s.clientIP(r)}); err != nil {
		return err
	}
	_ = s.st.TouchLogin(u.ID)
	c := &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r)}
	if remember {
		c.MaxAge = int(life.Seconds())
	}
	http.SetCookie(w, c)
	return nil
}

func (s *Server) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r)})
}

func (s *Server) sessionFromRequest(r *http.Request) (identity, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return identity{}, false
	}
	h := sha256Hex(c.Value)
	sess, u, err := s.st.GetSession(h)
	if err != nil {
		return identity{}, false
	}
	if now := time.Now().Unix(); now-sess.LastSeen > 60 {
		_ = s.st.TouchSession(h, now, s.clientIP(r))
	}
	return identity{User: u, SessionHash: h}, true
}

// apiTokenOK checks "Authorization: Bearer <read-only token>".
func (s *Server) apiTokenOK(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return false
	}
	found, err := s.st.UseAPIToken(sha256Hex(strings.TrimSpace(tok)))
	return err == nil && found
}

var publicPaths = map[string]bool{"/healthz": true, "/login": true, "/setup": true, "/api/ingest": true, "/themes.css": true}

// authenticate requires a signed-in user for everything except public paths.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPaths[r.URL.Path] || strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		if id, ok := s.sessionFromRequest(r); ok {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
			return
		}
		if r.URL.Path == "/api/accounts" && r.Method == http.MethodGet && s.apiTokenOK(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || r.Method != http.MethodGet {
			jsonError(w, http.StatusUnauthorized, "sign in required")
			return
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	})
}

// adminOnly wraps handlers that change accounts or site settings.
func (s *Server) adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id, ok := currentUser(r); !ok || !id.User.IsAdmin() {
			http.Error(w, "Only admins can do that.", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") ||
		strings.HasPrefix(next, "/login") || strings.HasPrefix(next, "/setup") {
		return "/"
	}
	return next
}

// ---- first run ----

// bootstrap creates the first admin from TRACKER_PASSWORD, or prints a
// one-time setup code so that only someone with access to the logs can
// claim a fresh install.
func (s *Server) bootstrap(username, password string) error {
	n, err := s.st.CountUsers()
	if err != nil || n > 0 {
		return err
	}
	if password != "" {
		if username == "" {
			username = "admin"
		}
		h, err := hashPassword(password)
		if err != nil {
			return err
		}
		if _, err := s.st.CreateUser(username, h, store.RoleAdmin); err != nil {
			return err
		}
		slog.Info("created admin user from TRACKER_PASSWORD", "username", username)
		return nil
	}
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	code := make([]byte, 0, 9)
	for i, x := range b {
		if i == 4 {
			code = append(code, '-')
		}
		code = append(code, alphabet[int(x)%len(alphabet)])
	}
	s.setupCode = string(code)
	slog.Warn("no users yet: open the dashboard and create the admin account with this setup code", "setup_code", s.setupCode)
	return nil
}

func (s *Server) needsSetup() bool {
	n, err := s.st.CountUsers()
	return err == nil && n == 0
}

// ---- pages ----

type loginPage struct {
	Next      string
	Username  string
	Error     string
	Locked    bool
	LockedFor string
	Setup     bool
	SiteTheme string
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if s.needsSetup() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if _, ok := s.sessionFromRequest(r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	p := loginPage{Next: r.URL.Query().Get("next")}
	if d := s.lim.lockedFor(s.clientIP(r)); d > 0 {
		p.Locked, p.LockedFor = true, human(d)
	}
	s.renderBare(w, r, "login", p)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	p := loginPage{Next: r.FormValue("next"), Username: strings.TrimSpace(r.FormValue("username"))}
	if d := s.lim.lockedFor(ip); d > 0 {
		p.Locked, p.LockedFor = true, human(d)
		w.WriteHeader(http.StatusTooManyRequests)
		s.renderBare(w, r, "login", p)
		return
	}
	u, err := s.st.GetUserByName(p.Username)
	hash := dummyHash
	if err == nil {
		hash = u.PwHash
	}
	if !checkPassword(hash, r.FormValue("password")) || err != nil {
		left := s.lim.fail(ip)
		slog.Warn("failed sign-in", "username", p.Username, "ip", ip)
		if left == 0 {
			p.Locked, p.LockedFor = true, human(lockoutDuration)
		} else {
			p.Error = fmt.Sprintf("Wrong username or password. %d %s left before sign-in is paused for 15 minutes.",
				left, plural(left, "try", "tries"))
		}
		w.WriteHeader(http.StatusUnauthorized)
		s.renderBare(w, r, "login", p)
		return
	}
	s.lim.reset(ip)
	if err := s.startSession(w, r, u, r.FormValue("remember") != ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, safeNext(p.Next), http.StatusSeeOther)
}

func (s *Server) setupForm(w http.ResponseWriter, r *http.Request) {
	if !s.needsSetup() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.renderBare(w, r, "login", loginPage{Setup: true, Username: "admin"})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	s.setupMu.Lock() // one claim at a time
	defer s.setupMu.Unlock()
	if !s.needsSetup() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ip := s.clientIP(r)
	p := loginPage{Setup: true, Username: strings.TrimSpace(r.FormValue("username"))}
	fail := func(msg string) {
		p.Error = msg
		w.WriteHeader(http.StatusBadRequest)
		s.renderBare(w, r, "login", p)
	}
	if d := s.lim.lockedFor(ip); d > 0 {
		p.Locked, p.LockedFor = true, human(d)
		w.WriteHeader(http.StatusTooManyRequests)
		s.renderBare(w, r, "login", p)
		return
	}
	code := strings.ToUpper(strings.TrimSpace(r.FormValue("code")))
	if s.setupCode == "" || subtle.ConstantTimeCompare([]byte(code), []byte(s.setupCode)) != 1 {
		s.lim.fail(ip)
		fail("That setup code doesn't match. It's printed in the server log at startup.")
		return
	}
	if err := validUsername(p.Username); err != nil {
		fail(err.Error())
		return
	}
	pw := r.FormValue("password")
	if err := validPassword(pw); err != nil {
		fail(err.Error())
		return
	}
	if pw != r.FormValue("confirm") {
		fail("The passwords don't match.")
		return
	}
	h, err := hashPassword(pw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, err := s.st.CreateUser(p.Username, h, store.RoleAdmin)
	if err != nil {
		fail(err.Error())
		return
	}
	s.setupCode = ""
	u, err := s.st.GetUser(id)
	if err == nil {
		err = s.startSession(w, r, u, true)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if id, ok := currentUser(r); ok {
		_ = s.st.DeleteSession(id.SessionHash)
	}
	s.clearCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
