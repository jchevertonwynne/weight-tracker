// Package db owns the SQLite schema and per-entity CRUD for the weight
// tracker: entries (weigh-ins), goals, and markers (chart annotations).
// Each entity's struct and queries live in their own file (entries.go,
// goals.go, markers.go); this file holds only what's genuinely shared:
// the schema, connection setup, and the one operation that spans all
// three tables.
package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS entries (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	recorded_at TEXT    NOT NULL, -- when the weigh-in happened, RFC3339
	weight_kg   REAL    NOT NULL,
	created_at  TEXT    NOT NULL  -- when the row was inserted, RFC3339
);
CREATE INDEX IF NOT EXISTS idx_entries_recorded_at ON entries (recorded_at);

CREATE TABLE IF NOT EXISTS goals (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	weight_kg      REAL    NOT NULL,
	effective_from TEXT    NOT NULL, -- when this goal becomes active, RFC3339
	created_at     TEXT    NOT NULL  -- when the row was inserted, RFC3339
);
CREATE INDEX IF NOT EXISTS idx_goals_effective_from ON goals (effective_from);

CREATE TABLE IF NOT EXISTS markers (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	date       TEXT    NOT NULL, -- the marker's date, RFC3339 (midnight local)
	note       TEXT    NOT NULL,
	created_at TEXT    NOT NULL  -- when the row was inserted, RFC3339
);
CREATE INDEX IF NOT EXISTS idx_markers_date ON markers (date);
`

func Open(path string) (*sql.DB, error) {
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite only tolerates one writer at a time; a single connection avoids
	// SQLITE_BUSY errors under htmx's concurrent request pattern.
	sqlDB.SetMaxOpenConns(1)

	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return sqlDB, nil
}

// DetectPeriod applies the morning/evening noon-split rule to a clock time.
func DetectPeriod(t time.Time) string {
	if t.Hour() < 12 {
		return "morning"
	}
	return "evening"
}

// parseStoredTime parses an RFC3339 timestamp read from the database.
// Timestamps are stored as UTC (see Create*/Update* in entries.go/goals.go/
// markers.go), but period detection and same-day comparisons operate on
// local wall-clock time, so the result is converted back to time.Local.
func parseStoredTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.Local(), nil
}

// DeleteAllData removes every entry, goal, and marker in a single
// transaction.
func DeleteAllData(sqlDB *sql.DB) error {
	tx, err := sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("begin delete-all tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM entries`); err != nil {
		return fmt.Errorf("delete entries: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM goals`); err != nil {
		return fmt.Errorf("delete goals: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM markers`); err != nil {
		return fmt.Errorf("delete markers: %w", err)
	}
	return tx.Commit()
}
