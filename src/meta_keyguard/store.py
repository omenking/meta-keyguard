"""The local key store: keys, settings, and an audit log of everything Keyguard did."""
import fcntl
import json
import os
import sqlite3
from contextlib import contextmanager
from datetime import datetime, timezone

from . import durations, paths

# A key moves from active to retiring (rotated, deleted after a grace period) to revoked.
# The sentinel is the key that keeps the account's last-key rule satisfied; nobody holds its secret.
STATES = ('active', 'retiring', 'revoked', 'sentinel')

DEFAULTS = {
    'default_ttl': '12h',
    'max_ttl': '7d',
    'rotate_grace': '5m',
    'rotate_before': '10m',
    'env_var': 'META_API_KEY',
    'cost_center': 'off',
    'cost_center_command': 'muse-cost',
    'team_id': '',
    'project_id': '',
}
DURATION_SETTINGS = ('default_ttl', 'max_ttl', 'rotate_grace', 'rotate_before')


def now():
    return datetime.now(timezone.utc)


def iso(moment):
    return moment.astimezone(timezone.utc).isoformat(timespec='seconds')


def parse_time(text):
    return datetime.fromisoformat(text) if text else None


def connect(path=None):
    path = path or paths.db_path()
    paths.ensure_data_dir()
    if not path.exists():
        os.close(os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600))
    db = sqlite3.connect(path, timeout=30, check_same_thread=False)
    db.row_factory = sqlite3.Row
    db.execute('PRAGMA journal_mode=WAL')
    db.executescript('''
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
      id INTEGER PRIMARY KEY, at TEXT, kind TEXT, label TEXT, meta_id TEXT, detail TEXT);
    ''')
    return db


@contextmanager
def lock():
    """Serialise operations that change keys, across the CLI and the bridge's reaper."""
    paths.ensure_data_dir()
    with open(paths.lock_path(), 'a') as handle:
        fcntl.flock(handle, fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(handle, fcntl.LOCK_UN)


def setting(db, key):
    row = db.execute('SELECT value FROM settings WHERE key=?', (key,)).fetchone()
    return row[0] if row else DEFAULTS.get(key)


def duration_setting(db, key):
    return durations.parse(setting(db, key))


def set_setting(db, key, value):
    with db:
        if value is None:
            db.execute('DELETE FROM settings WHERE key=?', (key,))
        else:
            db.execute('INSERT INTO settings VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value',
                       (key, value))


def json_setting(db, key):
    value = setting(db, key)
    return json.loads(value) if value else None


def log(db, kind, label=None, meta_id=None, **detail):
    with db:
        db.execute('INSERT INTO events (at, kind, label, meta_id, detail) VALUES (?, ?, ?, ?, ?)',
                   (iso(now()), kind, label, meta_id, json.dumps(detail) if detail else None))


def events(db, limit=50):
    rows = db.execute('SELECT * FROM events ORDER BY id DESC LIMIT ?', (limit,)).fetchall()
    return [dict(r) for r in reversed(rows)]


def record_failure(db, op, code, message):
    set_setting(db, 'last_failure', json.dumps({'at': iso(now()), 'op': op, 'code': code, 'message': message}))
    log(db, 'failure', op=op, code=code, message=message)


def clear_failure(db):
    set_setting(db, 'last_failure', None)


def insert_key(db, **fields):
    columns = ', '.join(fields)
    marks = ', '.join(':' + k for k in fields)
    with db:
        return db.execute(f'INSERT INTO keys ({columns}) VALUES ({marks})', fields).lastrowid


def update_key(db, key_id, **fields):
    assignments = ', '.join(f'{k}=:{k}' for k in fields)
    with db:
        db.execute(f'UPDATE keys SET {assignments} WHERE id=:id', {**fields, 'id': key_id})


def mark_revoked(db, row, reason):
    """Record that a key no longer exists on dev.meta.ai, and forget its secret."""
    update_key(db, row['id'], state='revoked', secret=None, revoked_at=iso(now()), revoke_reason=reason,
               last_error=None, delete_after=None)
    log(db, 'revoked', row['label'], row['meta_id'], reason=reason)


def live_keys(db):
    """Keys that still exist on dev.meta.ai as far as the store knows."""
    return [dict(r) for r in db.execute(
        "SELECT * FROM keys WHERE state IN ('active', 'retiring', 'sentinel') ORDER BY id")]


def all_keys(db):
    return [dict(r) for r in db.execute('SELECT * FROM keys ORDER BY id')]


def active_key(db, label):
    row = db.execute("SELECT * FROM keys WHERE label=? AND state='active' ORDER BY id DESC LIMIT 1",
                     (label,)).fetchone()
    return dict(row) if row else None


def by_meta_id(db, meta_id):
    row = db.execute('SELECT * FROM keys WHERE meta_id=?', (meta_id,)).fetchone()
    return dict(row) if row else None


def due_at(row):
    """When this key must be gone from dev.meta.ai, or None for the sentinel."""
    if row['state'] == 'active':
        return parse_time(row['expires_at'])
    if row['state'] == 'retiring':
        return parse_time(row['delete_after'])
    return None


def is_overdue(row, moment=None):
    due = due_at(row)
    return due is not None and due <= (moment or now())
