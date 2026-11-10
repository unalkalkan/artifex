// Package store keeps Artifex's own data in SQLite: accounts, sessions,
// tokens, projects and their members, and threads. Project content is never
// here; it is in each project's git repository.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ErrNotFound is returned when a lookup matches nothing.
var ErrNotFound = errors.New("not found")

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (and creates or migrates) the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	// One connection: writes are rare and small, and SQLite serialises them
	// anyway. It also makes "database is locked" impossible inside this process.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Backup writes a consistent copy of the database to path.
func (s *Store) Backup(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path)
	return err
}

var migrations = []string{
	// 1: everything.
	`
CREATE TABLE accounts (
	id          INTEGER PRIMARY KEY,
	email       TEXT NOT NULL UNIQUE COLLATE NOCASE,
	name        TEXT NOT NULL DEFAULT '',
	role        TEXT NOT NULL CHECK (role IN ('member', 'admin')),
	status      TEXT NOT NULL CHECK (status IN ('pending', 'active', 'disabled')),
	created_at  INTEGER NOT NULL,
	verified_at INTEGER,
	last_seen_at INTEGER
);
CREATE TABLE sessions (
	id_hash    TEXT PRIMARY KEY,
	account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	csrf       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_account ON sessions(account_id);
CREATE TABLE tokens (
	id           INTEGER PRIMARY KEY,
	account_id   INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	label        TEXT NOT NULL,
	hash         TEXT NOT NULL UNIQUE,
	prefix       TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER,
	revoked_at   INTEGER
);
CREATE INDEX tokens_account ON tokens(account_id);
CREATE TABLE projects (
	id         TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	created_by INTEGER REFERENCES accounts(id) ON DELETE SET NULL
);
CREATE TABLE project_members (
	project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	role       TEXT NOT NULL CHECK (role IN ('commenter', 'editor', 'owner')),
	added_at   INTEGER NOT NULL,
	PRIMARY KEY (project_id, account_id)
);
CREATE INDEX project_members_account ON project_members(account_id);
CREATE TABLE project_invites (
	project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	email      TEXT NOT NULL COLLATE NOCASE,
	role       TEXT NOT NULL CHECK (role IN ('commenter', 'editor', 'owner')),
	invited_by INTEGER REFERENCES accounts(id) ON DELETE SET NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (project_id, email)
);
CREATE INDEX project_invites_email ON project_invites(email);
CREATE TABLE threads (
	id              INTEGER PRIMARY KEY,
	project_id      TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	path            TEXT NOT NULL,
	commit_id       TEXT NOT NULL,
	selector        TEXT NOT NULL DEFAULT '',
	element_text    TEXT NOT NULL DEFAULT '',
	x               REAL,
	y               REAL,
	ox              REAL,
	oy              REAL,
	view_width      INTEGER,
	state           TEXT NOT NULL CHECK (state IN ('open', 'resolved')),
	resolved_commit TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL
);
CREATE INDEX threads_project ON threads(project_id, updated_at);
CREATE TABLE thread_entries (
	id          INTEGER PRIMARY KEY,
	thread_id   INTEGER NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	kind        TEXT NOT NULL CHECK (kind IN ('comment', 'resolved', 'reopened')),
	account_id  INTEGER REFERENCES accounts(id) ON DELETE SET NULL,
	author_name TEXT NOT NULL,
	agent_label TEXT NOT NULL DEFAULT '',
	body        TEXT NOT NULL DEFAULT '',
	commit_id   TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL
);
CREATE INDEX thread_entries_thread ON thread_entries(thread_id, id);
`,
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) unix() int64 { return s.now().Unix() }

func fromUnix(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
