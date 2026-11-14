package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

const authSchema = `
CREATE TABLE IF NOT EXISTS users (
	id         INTEGER PRIMARY KEY,
	username   TEXT    NOT NULL UNIQUE COLLATE NOCASE,
	pw_hash    TEXT    NOT NULL,
	role       TEXT    NOT NULL,
	prefs      TEXT    NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL,
	last_login INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS sessions (
	id_hash    TEXT    PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	user_agent TEXT    NOT NULL DEFAULT '',
	ip         TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS api_tokens (
	id         INTEGER PRIMARY KEY,
	name       TEXT    NOT NULL,
	token_hash TEXT    NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	last_used  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

var ErrNotFound = errors.New("not found")

// Roles.
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// Prefs are per-user display preferences.
type Prefs struct {
	Theme        string  `json:"theme,omitempty"`
	FollowSystem bool    `json:"follow_system,omitempty"`
	Compact      bool    `json:"compact,omitempty"`
	Range        int     `json:"range,omitempty"`      // default chart range in days
	ShowFirst    string  `json:"show_first,omitempty"` // "subs" or "api"
	Hidden       []int64 `json:"hidden,omitempty"`     // account ids hidden from the dashboard
}

func (p Prefs) IsHidden(id int64) bool {
	for _, h := range p.Hidden {
		if h == id {
			return true
		}
	}
	return false
}

type User struct {
	ID        int64
	Username  string
	PwHash    string
	Role      string
	Prefs     Prefs
	CreatedAt int64
	LastLogin int64
}

func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

const userCols = `id, username, pw_hash, role, prefs, created_at, last_login`

func scanUser(r interface{ Scan(...any) error }) (User, error) {
	var u User
	var prefs string
	if err := r.Scan(&u.ID, &u.Username, &u.PwHash, &u.Role, &prefs, &u.CreatedAt, &u.LastLogin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return u, ErrNotFound
		}
		return u, err
	}
	_ = json.Unmarshal([]byte(prefs), &u.Prefs)
	return u, nil
}

func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(username, pwHash, role string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO users(username, pw_hash, role, created_at) VALUES (?, ?, ?, ?)`,
		username, pwHash, role, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetUser(id int64) (User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (s *Store) GetUserByName(name string) (User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ?`, name))
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY username COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&n)
	return n, err
}

func (s *Store) SetPassword(id int64, pwHash string) error {
	_, err := s.db.Exec(`UPDATE users SET pw_hash = ? WHERE id = ?`, pwHash, id)
	return err
}

func (s *Store) SetPrefs(id int64, p Prefs) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE users SET prefs = ? WHERE id = ?`, string(b), id)
	return err
}

func (s *Store) TouchLogin(id int64) error {
	_, err := s.db.Exec(`UPDATE users SET last_login = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

func (s *Store) DeleteUser(id int64) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

// ---- sessions ----

type Session struct {
	IDHash    string
	UserID    int64
	CreatedAt int64
	LastSeen  int64
	ExpiresAt int64
	UserAgent string
	IP        string
}

func (s *Store) CreateSession(sess Session) error {
	_, err := s.db.Exec(`INSERT INTO sessions(id_hash, user_id, created_at, last_seen, expires_at, user_agent, ip) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.IDHash, sess.UserID, sess.CreatedAt, sess.LastSeen, sess.ExpiresAt, sess.UserAgent, sess.IP)
	return err
}

// GetSession returns a live session and its user.
func (s *Store) GetSession(idHash string) (Session, User, error) {
	var sess Session
	row := s.db.QueryRow(`SELECT id_hash, user_id, created_at, last_seen, expires_at, user_agent, ip
		FROM sessions WHERE id_hash = ? AND expires_at > ?`, idHash, time.Now().Unix())
	if err := row.Scan(&sess.IDHash, &sess.UserID, &sess.CreatedAt, &sess.LastSeen, &sess.ExpiresAt, &sess.UserAgent, &sess.IP); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sess, User{}, ErrNotFound
		}
		return sess, User{}, err
	}
	u, err := s.GetUser(sess.UserID)
	return sess, u, err
}

func (s *Store) TouchSession(idHash string, lastSeen int64, ip string) error {
	_, err := s.db.Exec(`UPDATE sessions SET last_seen = ?, ip = ? WHERE id_hash = ?`, lastSeen, ip, idHash)
	return err
}

func (s *Store) DeleteSession(idHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id_hash = ?`, idHash)
	return err
}

// DeleteUserSessions signs a user out everywhere except the given session ("" for all).
func (s *Store) DeleteUserSessions(userID int64, except string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND id_hash != ?`, userID, except)
	return err
}

func (s *Store) ListSessions(userID int64) ([]Session, error) {
	rows, err := s.db.Query(`SELECT id_hash, user_id, created_at, last_seen, expires_at, user_agent, ip
		FROM sessions WHERE user_id = ? AND expires_at > ? ORDER BY last_seen DESC`, userID, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var x Session
		if err := rows.Scan(&x.IDHash, &x.UserID, &x.CreatedAt, &x.LastSeen, &x.ExpiresAt, &x.UserAgent, &x.IP); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) PruneSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, time.Now().Unix())
	return err
}

// ---- read-only API tokens ----

type APIToken struct {
	ID        int64
	Name      string
	CreatedAt int64
	LastUsed  int64
}

func (s *Store) CreateAPIToken(name, hash string) error {
	_, err := s.db.Exec(`INSERT INTO api_tokens(name, token_hash, created_at) VALUES (?, ?, ?)`, name, hash, time.Now().Unix())
	return err
}

func (s *Store) ListAPITokens() ([]APIToken, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at, last_used FROM api_tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAPIToken(id int64) error {
	_, err := s.db.Exec(`DELETE FROM api_tokens WHERE id = ?`, id)
	return err
}

// UseAPIToken reports whether the hash belongs to a token, recording its use.
func (s *Store) UseAPIToken(hash string) (bool, error) {
	res, err := s.db.Exec(`UPDATE api_tokens SET last_used = ? WHERE token_hash = ?`, time.Now().Unix(), hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ---- site settings ----

// Settings are admin-controlled and apply to everyone.
type Settings struct {
	PollMinutes   int
	RetentionDays int
	Warn          int
	Crit          int
}

var DefaultSettings = Settings{PollMinutes: 10, RetentionDays: 90, Warn: 70, Crit: 90}

func (s *Store) GetSettings() (Settings, error) {
	out := DefaultSettings
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return out, err
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		switch k {
		case "poll_minutes":
			out.PollMinutes = n
		case "retention_days":
			out.RetentionDays = n
		case "warn":
			out.Warn = n
		case "crit":
			out.Crit = n
		}
	}
	return out, rows.Err()
}

// HasSetting reports whether a setting was saved explicitly (vs. defaulted).
func (s *Store) HasSetting(key string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = ?`, key).Scan(&n)
	return n > 0, err
}

func (s *Store) SaveSettings(x Settings) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range map[string]int{"poll_minutes": x.PollMinutes, "retention_days": x.RetentionDays, "warn": x.Warn, "crit": x.Crit} {
		if _, err := tx.Exec(`INSERT INTO settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			k, strconv.Itoa(v)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.retentionDays.Store(int64(x.RetentionDays))
	return nil
}
