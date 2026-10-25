package keyguard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// A key moves from active to retiring (rotated, deleted after a grace period) to revoked. The
// sentinel is the key that keeps the account's last-key rule satisfied; nobody holds its secret.
const (
	StateActive   = "active"
	StateRetiring = "retiring"
	StateRevoked  = "revoked"
	StateSentinel = "sentinel"
)

var Defaults = map[string]string{
	"default_ttl":         "12h",
	"max_ttl":             "7d",
	"rotate_grace":        "5m",
	"rotate_before":       "10m",
	"env_var":             "META_API_KEY",
	"cost_center":         "off",
	"cost_center_command": "muse-cost",
	"team_id":             "",
	"project_id":          "",
}

var DurationSettings = []string{"default_ttl", "max_ttl", "rotate_grace", "rotate_before"}

// SettingNames lists the user-visible settings in order.
func SettingNames() []string {
	names := make([]string, 0, len(Defaults))
	for name := range Defaults {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Times are stored as ISO 8601 in UTC with second precision, for example 2026-10-25T17:19:29+00:00.
const timeLayout = "2006-01-02T15:04:05-07:00"

// now is replaceable in tests.
var now = func() time.Time { return time.Now().UTC() }

func ISO(t time.Time) string { return t.UTC().Format(timeLayout) }

func ParseTime(text string) (time.Time, bool) {
	if text == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, text)
	return t, err == nil
}

// Key is one row of the key store.
type Key struct {
	ID           int64
	Label        string
	MetaID       string
	MetaName     string
	Secret       string
	State        string
	CreatedAt    string
	ExpiresAt    string
	TTLSeconds   int64
	AutoRotate   bool
	DeleteAfter  string
	RevokedAt    string
	RevokeReason string
	LastError    string
}

// DueAt is when the key must be gone from dev.meta.ai; the sentinel and revoked keys have none.
func (k Key) DueAt() (time.Time, bool) {
	switch k.State {
	case StateActive:
		return ParseTime(k.ExpiresAt)
	case StateRetiring:
		return ParseTime(k.DeleteAfter)
	}
	return time.Time{}, false
}

func (k Key) OverdueAt(moment time.Time) bool {
	due, ok := k.DueAt()
	return ok && !due.After(moment)
}

func (k Key) Overdue() bool { return k.OverdueAt(now()) }

type Store struct{ db *sql.DB }

func OpenStore() (*Store, error) {
	if _, err := EnsureDataDir(); err != nil {
		return nil, err
	}
	path := DBPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		f.Close()
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`
    CREATE TABLE IF NOT EXISTS keys (
      id INTEGER PRIMARY KEY, label TEXT NOT NULL, meta_id TEXT, meta_name TEXT, secret TEXT,
      state TEXT NOT NULL, created_at TEXT, expires_at TEXT, ttl_seconds INTEGER,
      auto_rotate INTEGER DEFAULT 0, delete_after TEXT, revoked_at TEXT, revoke_reason TEXT,
      last_error TEXT);
    CREATE INDEX IF NOT EXISTS keys_label ON keys(label);
    CREATE UNIQUE INDEX IF NOT EXISTS keys_meta_id ON keys(meta_id) WHERE meta_id IS NOT NULL;
    CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT);
    -- Audit log. Secrets are never written here.
    CREATE TABLE IF NOT EXISTS events (
      id INTEGER PRIMARY KEY, at TEXT, kind TEXT, label TEXT, meta_id TEXT, detail TEXT);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Lock serialises operations that change keys, across the CLI and the bridge's reaper.
func Lock() (func(), error) {
	if _, err := EnsureDataDir(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func (s *Store) Setting(key string) string {
	var value sql.NullString
	if err := s.db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&value); err == nil && value.Valid {
		return value.String
	}
	return Defaults[key]
}

func (s *Store) DurationSetting(key string) (int64, error) {
	return ParseDuration(s.Setting(key))
}

// SetSetting stores a value, or removes the setting when value is nil.
func (s *Store) SetSetting(key string, value *string) error {
	if value == nil {
		_, err := s.db.Exec("DELETE FROM settings WHERE key=?", key)
		return err
	}
	_, err := s.db.Exec("INSERT INTO settings VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, *value)
	return err
}

func (s *Store) SetJSON(key string, value any) error {
	if value == nil {
		return s.SetSetting(key, nil)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	text := string(data)
	return s.SetSetting(key, &text)
}

// JSONSetting decodes a JSON setting into target and reports whether it was present.
func (s *Store) JSONSetting(key string, target any) bool {
	value := s.Setting(key)
	return value != "" && json.Unmarshal([]byte(value), target) == nil
}

// Log appends to the audit log. Callers never pass secrets.
func (s *Store) Log(kind, label, metaID string, detail map[string]any) {
	var detailJSON any
	if len(detail) > 0 {
		data, _ := json.Marshal(detail)
		detailJSON = string(data)
	}
	s.db.Exec("INSERT INTO events (at, kind, label, meta_id, detail) VALUES (?, ?, ?, ?, ?)",
		ISO(now()), kind, nullable(label), nullable(metaID), detailJSON)
}

type Event struct {
	At, Kind, Label, MetaID, Detail string
}

func (s *Store) Events(limit int) []Event {
	rows, err := s.db.Query("SELECT at, kind, label, meta_id, detail FROM events ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var at, kind, label, metaID, detail sql.NullString
		if rows.Scan(&at, &kind, &label, &metaID, &detail) == nil {
			events = append(events, Event{at.String, kind.String, label.String, metaID.String, detail.String})
		}
	}
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events
}

// Failure is the last operation that failed, shown by `status` and `doctor` until it clears.
type Failure struct {
	At      string `json:"at"`
	Op      string `json:"op"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Store) RecordFailure(op, code, message string) {
	s.SetJSON("last_failure", Failure{ISO(now()), op, code, message})
	s.Log("failure", "", "", map[string]any{"op": op, "code": code, "message": message})
}

func (s *Store) ClearFailure() { s.SetSetting("last_failure", nil) }

func (s *Store) LastFailure() *Failure {
	var f Failure
	if s.JSONSetting("last_failure", &f) {
		return &f
	}
	return nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

const keyColumns = "id, label, meta_id, meta_name, secret, state, created_at, expires_at, ttl_seconds, auto_rotate, " +
	"delete_after, revoked_at, revoke_reason, last_error"

func scanKeys(rows *sql.Rows) ([]Key, error) {
	defer rows.Close()
	var keys []Key
	for rows.Next() {
		var k Key
		var metaID, metaName, secret, createdAt, expiresAt, deleteAfter, revokedAt, revokeReason, lastError sql.NullString
		var ttl, autoRotate sql.NullInt64
		if err := rows.Scan(&k.ID, &k.Label, &metaID, &metaName, &secret, &k.State, &createdAt, &expiresAt, &ttl,
			&autoRotate, &deleteAfter, &revokedAt, &revokeReason, &lastError); err != nil {
			return nil, err
		}
		k.MetaID, k.MetaName, k.Secret = metaID.String, metaName.String, secret.String
		k.CreatedAt, k.ExpiresAt, k.DeleteAfter = createdAt.String, expiresAt.String, deleteAfter.String
		k.RevokedAt, k.RevokeReason, k.LastError = revokedAt.String, revokeReason.String, lastError.String
		k.TTLSeconds, k.AutoRotate = ttl.Int64, autoRotate.Int64 != 0
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *Store) query(where string, args ...any) ([]Key, error) {
	rows, err := s.db.Query("SELECT "+keyColumns+" FROM keys "+where, args...)
	if err != nil {
		return nil, err
	}
	return scanKeys(rows)
}

func (s *Store) one(where string, args ...any) (*Key, error) {
	keys, err := s.query(where+" LIMIT 1", args...)
	if err != nil || len(keys) == 0 {
		return nil, err
	}
	return &keys[0], nil
}

// LiveKeys are the keys that still exist on dev.meta.ai as far as the store knows.
func (s *Store) LiveKeys() ([]Key, error) {
	return s.query("WHERE state IN ('active', 'retiring', 'sentinel') ORDER BY id")
}

func (s *Store) AllKeys() ([]Key, error) { return s.query("ORDER BY id") }

func (s *Store) ActiveKey(label string) (*Key, error) {
	return s.one("WHERE label=? AND state='active' ORDER BY id DESC", label)
}

func (s *Store) ByMetaID(metaID string) (*Key, error) { return s.one("WHERE meta_id=?", metaID) }

func (s *Store) ByID(id int64) (*Key, error) { return s.one("WHERE id=?", id) }

func (s *Store) InsertKey(k Key) (int64, error) {
	result, err := s.db.Exec(`INSERT INTO keys (label, meta_id, meta_name, secret, state, created_at, expires_at,
      ttl_seconds, auto_rotate) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.Label, nullable(k.MetaID), nullable(k.MetaName), nullable(k.Secret), k.State, nullable(k.CreatedAt),
		nullable(k.ExpiresAt), nullableInt(k.TTLSeconds), boolInt(k.AutoRotate))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// UpdateKey sets columns by name; an empty string is stored as NULL.
func (s *Store) UpdateKey(id int64, fields map[string]string) error {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	assignments := make([]string, len(names))
	args := make([]any, 0, len(names)+1)
	for i, name := range names {
		assignments[i] = name + "=?"
		args = append(args, nullable(fields[name]))
	}
	args = append(args, id)
	_, err := s.db.Exec(fmt.Sprintf("UPDATE keys SET %s WHERE id=?", strings.Join(assignments, ", ")), args...)
	return err
}

// MarkRevoked records that a key no longer exists on dev.meta.ai, and forgets its secret.
func (s *Store) MarkRevoked(k Key, reason string) error {
	err := s.UpdateKey(k.ID, map[string]string{"state": StateRevoked, "secret": "", "revoked_at": ISO(now()),
		"revoke_reason": reason, "last_error": "", "delete_after": ""})
	if err == nil {
		s.Log("revoked", k.Label, k.MetaID, map[string]any{"reason": reason})
	}
	return err
}

func nullableInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
