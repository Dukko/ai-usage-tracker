package web

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/store"
)

type themeCard struct {
	theme
	Selected bool
}

type accountToggle struct {
	ID       int64
	Name     string
	Provider string
	Badge    any
	Initials string
	Shown    bool
}

type sessionRow struct {
	store.Session
	Current bool
}

type settingsPage struct {
	Me          store.User
	IsAdmin     bool
	DarkThemes  []themeCard
	LightThemes []themeCard
	Light, Dark string // labels used when following the system setting
	Accounts    []accountToggle
	Sessions    []sessionRow
	Users       []store.User
	Tokens      []store.APIToken
	NewToken    string
	Set         store.Settings
	PollOptions []int
	KeepOptions []int
	Error       string // shown at the top of the section named by ErrorAt
	ErrorAt     string
}

func (s *Server) settingsData(r *http.Request) (settingsPage, error) {
	id, _ := currentUser(r)
	u := id.User
	p := settingsPage{Me: u, IsAdmin: u.IsAdmin(), Set: s.settings(),
		PollOptions: []int{5, 10, 15, 30, 60}, KeepOptions: []int{30, 90, 365}}
	p.Set.PollMinutes = int(s.pl.Interval() / time.Minute)
	if !slices.Contains(p.PollOptions, p.Set.PollMinutes) { // e.g. set via TRACKER_POLL_INTERVAL
		p.PollOptions = append(p.PollOptions, p.Set.PollMinutes)
		slices.Sort(p.PollOptions)
	}
	cur := themeOrDefault(u.Prefs.Theme)
	for _, t := range themes {
		c := themeCard{t, t.ID == cur.ID}
		if t.Dark {
			p.DarkThemes = append(p.DarkThemes, c)
		} else {
			p.LightThemes = append(p.LightThemes, c)
		}
	}
	light, dark := systemPair(cur.ID)
	p.Light, p.Dark = light.Label, dark.Label

	accounts, err := s.st.ListAccounts()
	if err != nil {
		return p, err
	}
	for _, a := range accounts {
		v, _ := s.buildView(a, viewOpts{days: 7})
		p.Accounts = append(p.Accounts, accountToggle{ID: a.ID, Name: a.Name, Provider: v.ProviderLabel,
			Badge: v.Badge, Initials: v.Initials, Shown: !u.Prefs.IsHidden(a.ID)})
	}
	sessions, err := s.st.ListSessions(u.ID)
	if err != nil {
		return p, err
	}
	for _, x := range sessions {
		p.Sessions = append(p.Sessions, sessionRow{x, x.IDHash == id.SessionHash})
	}
	if p.IsAdmin {
		if p.Users, err = s.st.ListUsers(); err != nil {
			return p, err
		}
		if p.Tokens, err = s.st.ListAPITokens(); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, func(*settingsPage) {})
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, edit func(*settingsPage)) {
	p, err := s.settingsData(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	edit(&p)
	if p.Error != "" {
		w.WriteHeader(http.StatusBadRequest)
	}
	s.render(w, r, "settings", "Settings", "settings", p)
}

func (s *Server) settingsError(w http.ResponseWriter, r *http.Request, section, msg string) {
	s.renderSettings(w, r, func(p *settingsPage) { p.Error, p.ErrorAt = msg, section })
}

func back(w http.ResponseWriter, r *http.Request, anchor, flash string) {
	u := "/settings"
	if flash != "" {
		u += "?ok=" + url.QueryEscape(flash)
	}
	http.Redirect(w, r, u+"#"+anchor, http.StatusSeeOther)
}

// ---- per-user ----

func (s *Server) savePrefs(r *http.Request, edit func(*store.Prefs)) error {
	u := me(r)
	p := u.Prefs
	edit(&p)
	return s.st.SetPrefs(u.ID, p)
}

// saveAppearance applies whichever control was pressed: a theme card,
// the follow-system switch or a density button.
func (s *Server) saveAppearance(w http.ResponseWriter, r *http.Request) {
	err := s.savePrefs(r, func(p *store.Prefs) {
		if t, ok := themeByID(r.FormValue("theme")); ok {
			p.Theme = t.ID
		}
		if r.Form.Has("follow") {
			p.FollowSystem = r.FormValue("follow") == "on"
		}
		if r.Form.Has("density") {
			p.Compact = r.FormValue("density") == "compact"
		}
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	back(w, r, "appearance", "")
}

func (s *Server) cycleTheme(w http.ResponseWriter, r *http.Request) {
	err := s.savePrefs(r, func(p *store.Prefs) {
		p.Theme = nextTheme(themeOrDefault(p.Theme).ID)
		p.FollowSystem = false // an explicit pick overrides the device setting
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, safeNext(r.FormValue("back")), http.StatusSeeOther)
}

func (s *Server) saveDashboard(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.st.ListAccounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = r.ParseForm()
	shown := r.Form["show"]
	err = s.savePrefs(r, func(p *store.Prefs) {
		if n, _ := strconv.Atoi(r.FormValue("range")); n == 7 || n == 30 || n == 90 {
			p.Range = n
		}
		if f := r.FormValue("first"); f == "subs" || f == "api" {
			p.ShowFirst = f
		}
		p.Hidden = nil
		for _, a := range accounts {
			if !slices.Contains(shown, strconv.FormatInt(a.ID, 10)) {
				p.Hidden = append(p.Hidden, a.ID)
			}
		}
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	back(w, r, "dashboard", "saved")
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	id, _ := currentUser(r)
	if !checkPassword(id.User.PwHash, r.FormValue("current")) {
		s.settingsError(w, r, "security", "Your current password isn't right.")
		return
	}
	pw := r.FormValue("new")
	if err := validPassword(pw); err != nil {
		s.settingsError(w, r, "security", err.Error())
		return
	}
	if pw != r.FormValue("confirm") {
		s.settingsError(w, r, "security", "The new passwords don't match.")
		return
	}
	h, err := hashPassword(pw)
	if err == nil {
		err = s.st.SetPassword(id.User.ID, h)
	}
	if err == nil {
		// A new password should also end any session it may have leaked into.
		err = s.st.DeleteUserSessions(id.User.ID, id.SessionHash)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	back(w, r, "security", "password")
}

func (s *Server) signOutOthers(w http.ResponseWriter, r *http.Request) {
	id, _ := currentUser(r)
	if err := s.st.DeleteUserSessions(id.User.ID, id.SessionHash); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	back(w, r, "security", "signedout")
}

// ---- admin ----

func (s *Server) saveChecks(w http.ResponseWriter, r *http.Request) {
	poll, _ := strconv.Atoi(r.FormValue("poll"))
	keep, _ := strconv.Atoi(r.FormValue("keep"))
	warn, _ := strconv.Atoi(r.FormValue("warn"))
	crit, _ := strconv.Atoi(r.FormValue("crit"))
	switch {
	case poll < 1 || poll > 24*60:
		s.settingsError(w, r, "checks", "Pick how often to check.")
		return
	case keep < 7 || keep > 3650:
		s.settingsError(w, r, "checks", "Pick how long to keep history.")
		return
	case warn < 1 || warn > 100 || crit < 1 || crit > 100:
		s.settingsError(w, r, "checks", "Thresholds are percentages from 1 to 100.")
		return
	case crit <= warn:
		s.settingsError(w, r, "checks", "The critical level has to be above the warning level.")
		return
	}
	set := store.Settings{PollMinutes: poll, RetentionDays: keep, Warn: warn, Crit: crit}
	if err := s.st.SaveSettings(set); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.set.Store(&set)
	s.pl.SetInterval(time.Duration(poll) * time.Minute)
	back(w, r, "checks", "saved")
}

func (s *Server) addUser(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("username"))
	role := r.FormValue("role")
	if role != store.RoleAdmin {
		role = store.RoleViewer
	}
	if err := validUsername(name); err != nil {
		s.settingsError(w, r, "users", err.Error())
		return
	}
	pw := r.FormValue("password")
	if err := validPassword(pw); err != nil {
		s.settingsError(w, r, "users", err.Error())
		return
	}
	if _, err := s.st.GetUserByName(name); err == nil {
		s.settingsError(w, r, "users", fmt.Sprintf("There's already a user called %s.", name))
		return
	}
	h, err := hashPassword(pw)
	if err == nil {
		_, err = s.st.CreateUser(name, h, role)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	back(w, r, "users", "user-added")
}

func (s *Server) resetUserPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	pw := r.FormValue("password")
	if err := validPassword(pw); err != nil {
		s.settingsError(w, r, "users", err.Error())
		return
	}
	h, err := hashPassword(pw)
	if err == nil {
		err = s.st.SetPassword(id, h)
	}
	if err == nil {
		err = s.st.DeleteUserSessions(id, "")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	back(w, r, "users", "user-reset")
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if id == me(r).ID {
		s.settingsError(w, r, "users", "You can't remove yourself.")
		return
	}
	u, err := s.st.GetUser(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if u.IsAdmin() {
		if n, err := s.st.CountAdmins(); err != nil || n <= 1 {
			s.settingsError(w, r, "users", "Keep at least one admin.")
			return
		}
	}
	if err := s.st.DeleteUser(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	back(w, r, "users", "user-gone")
}

func (s *Server) createAPIToken(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > 80 {
		s.settingsError(w, r, "tokens", "Give the token a name, like Home Assistant.")
		return
	}
	token := "aut_" + randomToken(24)
	if err := s.st.CreateAPIToken(name, sha256Hex(token)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderSettings(w, r, func(p *settingsPage) { p.NewToken = token })
}

func (s *Server) deleteAPIToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.st.DeleteAPIToken(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	back(w, r, "tokens", "revoked")
}
