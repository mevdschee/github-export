// Package store is the primary source of truth for synced GitHub data. The sync
// layer upserts parsed API payloads here; the exporter reads them back to render
// markdown on demand. Each entity keeps typed columns (for querying and change
// detection) alongside a raw_json blob (for full-fidelity rendering).
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"
)

// schemaVersion is bumped whenever the schema changes; migrate() applies the
// full schema for a fresh DB and is the hook point for future migrations.
const schemaVersion = 1

const schemaSQL = `
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT
);

CREATE TABLE repository (
  id       INTEGER PRIMARY KEY CHECK (id = 1),
  owner    TEXT NOT NULL,
  name     TEXT NOT NULL,
  raw_json TEXT NOT NULL
);

CREATE TABLE labels (
  name        TEXT PRIMARY KEY,
  color       TEXT,
  description TEXT,
  raw_json    TEXT NOT NULL,
  ord         INTEGER
);

CREATE TABLE milestones (
  number      INTEGER PRIMARY KEY,
  title       TEXT,
  state       TEXT,
  description TEXT,
  due_on      TEXT,
  closed_at   TEXT,
  raw_json    TEXT NOT NULL,
  ord         INTEGER
);

CREATE TABLE issues (
  number          INTEGER PRIMARY KEY,
  is_pull_request INTEGER NOT NULL DEFAULT 0,
  title           TEXT,
  state           TEXT,
  state_reason    TEXT,
  draft           INTEGER NOT NULL DEFAULT 0,
  locked          INTEGER NOT NULL DEFAULT 0,
  merged          INTEGER NOT NULL DEFAULT 0,
  created_at      TEXT,
  updated_at      TEXT,
  closed_at       TEXT,
  author          TEXT,
  milestone       TEXT,
  body            TEXT,
  raw_json        TEXT NOT NULL,
  pr_json         TEXT,
  timeline_json   TEXT
);

CREATE TABLE issue_labels (
  issue_number INTEGER NOT NULL,
  label        TEXT NOT NULL,
  ord          INTEGER,
  PRIMARY KEY (issue_number, label)
);

CREATE TABLE issue_assignees (
  issue_number INTEGER NOT NULL,
  login        TEXT NOT NULL,
  ord          INTEGER,
  PRIMARY KEY (issue_number, login)
);

CREATE TABLE issue_projects (
  issue_number INTEGER NOT NULL,
  project      TEXT NOT NULL,
  ord          INTEGER,
  PRIMARY KEY (issue_number, project)
);

CREATE TABLE releases (
  tag              TEXT PRIMARY KEY,
  name             TEXT,
  draft            INTEGER NOT NULL DEFAULT 0,
  prerelease       INTEGER NOT NULL DEFAULT 0,
  author           TEXT,
  created_at       TEXT,
  published_at     TEXT,
  target_commitish TEXT,
  body             TEXT,
  raw_json         TEXT NOT NULL,
  ord              INTEGER
);

CREATE TABLE projects (
  number      INTEGER PRIMARY KEY,
  title       TEXT,
  state       TEXT,
  public      INTEGER NOT NULL DEFAULT 0,
  owner       TEXT,
  url         TEXT,
  description TEXT,
  created_at  TEXT,
  updated_at  TEXT,
  raw_json    TEXT NOT NULL,
  items_json  TEXT
);

CREATE TABLE discussions (
  number       INTEGER PRIMARY KEY,
  title        TEXT,
  category     TEXT,
  state        TEXT,
  state_reason TEXT,
  author       TEXT,
  created_at   TEXT,
  updated_at   TEXT,
  closed_at    TEXT,
  answer_id    INTEGER,
  body         TEXT,
  raw_json     TEXT NOT NULL
);

CREATE TABLE events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  type        TEXT NOT NULL,
  number      INTEGER,
  title       TEXT,
  author      TEXT,
  state       TEXT,
  labels_json TEXT,
  file        TEXT,
  repo        TEXT,
  body        TEXT,
  url         TEXT,
  extra_json  TEXT,
  detected_at TEXT,
  exported_at TEXT
);
`

// Store wraps the SQLite connection. Methods come in three families: Upsert*
// (write during sync), Prev*/*State (read prior state for change detection),
// and All*/Pending* (read for export).
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path, configures it for
// fast bulk writes, and runs migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// WAL + NORMAL gives us durable-enough, much faster bulk inserts. Single
	// connection keeps the modernc driver's write path simple.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle for the query/API layers built in later phases.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	if v == schemaVersion {
		return nil
	}
	if v == 0 {
		if _, err := s.db.Exec(schemaSQL); err != nil {
			return fmt.Errorf("applying schema: %w", err)
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
			return fmt.Errorf("setting user_version: %w", err)
		}
		return nil
	}
	return fmt.Errorf("database schema version %d is newer than supported %d; upgrade the tool", v, schemaVersion)
}

// --- meta ---

// GetMeta returns the value for key, or "" if absent.
func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetMeta upserts a meta key/value.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(
		"INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		key, value)
	return err
}

// OwnerRepo reads the synced owner/repo recorded in meta.
func (s *Store) OwnerRepo() (owner, repo string, err error) {
	if owner, err = s.GetMeta("owner"); err != nil {
		return
	}
	repo, err = s.GetMeta("repo")
	return
}

// SyncedAt returns the timestamp of the last completed sync, or "".
func (s *Store) SyncedAt() (string, error) { return s.GetMeta("synced_at") }

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// The inputs are always JSON-decoded maps/slices, so this is unreachable
		// in practice; fall back to an empty object rather than crash a sync.
		return "{}"
	}
	return string(b)
}
