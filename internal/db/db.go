// Package db owns the SQLite schema and per-entity CRUD for the weight
// tracker: entries (weigh-ins), goals, and markers (chart annotations).
// Each entity's struct and queries live in their own file (entries.go,
// goals.go, markers.go); this file holds only what's genuinely shared:
// the schema, connection setup, and the one operation that spans all
// three tables.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by the Delete* functions when no row with the
// given id exists, so a handler can answer 404 rather than reporting
// success for a row that was never there (e.g. a stale second tab deleting
// the same entry twice).
var ErrNotFound = errors.New("not found")

// deleteByID runs a single-row delete and reports ErrNotFound when the
// statement matched nothing. Shared by entries, goals, and markers, whose
// delete paths are otherwise identical apart from the table name.
func deleteByID(sqlDB *sql.DB, table string, id int64) error {
	// table is never user input — every caller passes a literal — so
	// interpolating it is safe; SQLite cannot parameterize table names.
	res, err := sqlDB.Exec(`DELETE FROM `+table+` WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete from %s %d: %w", table, id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected deleting from %s %d: %w", table, id, err)
	}
	if affected == 0 {
		return fmt.Errorf("delete from %s %d: %w", table, id, ErrNotFound)
	}
	return nil
}

// Weights are stored as whole grams rather than a REAL number of
// kilograms. A binary float cannot represent most one-decimal kilogram
// values exactly, so a weight entered as 82.4 came back as
// 82.400000000000006, and a unit conversion during import (180 lb ->
// 81.64662660000001 kg) made it worse — visible in the CSV export, which
// prints the stored value at full precision. Grams are exact, well inside
// int64, and finer than any scale the app is used with. Kilograms are
// purely a presentation concern; see GramsToKg.
const schema = `
CREATE TABLE IF NOT EXISTS entries (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	recorded_at     TEXT    NOT NULL, -- when the weigh-in happened, RFC3339
	weight_g        INTEGER NOT NULL, -- whole grams
	period_override TEXT,             -- "morning"/"evening"; NULL means auto-detect from recorded_at
	created_at      TEXT    NOT NULL  -- when the row was inserted, RFC3339
);
CREATE INDEX IF NOT EXISTS idx_entries_recorded_at ON entries (recorded_at);

CREATE TABLE IF NOT EXISTS goals (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	weight_g       INTEGER NOT NULL, -- whole grams
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

	if err := applyPragmas(sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}

	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Order matters: the grams migration copies period_override across, so
	// that column has to exist first on a database old enough to predate it.
	if err := addPeriodOverrideColumn(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate period_override column: %w", err)
	}
	if err := migrateWeightsToGrams(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate weights to grams: %w", err)
	}
	// Views come last: they read the post-migration column names.
	if err := applyViews(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply views: %w", err)
	}
	return sqlDB, nil
}

// views are the stable query surface for anything outside this app —
// currently Grafana dashboards, which query them through the SQLite
// datasource. They exist so a schema change is not automatically a broken
// dashboard: the kilogram-to-gram migration would have silently broken
// every panel that referenced entries.weight_kg, whereas a view can absorb
// that and keep presenting kilograms.
//
// They are dropped and recreated on every open rather than versioned. A
// view holds no data, so recreating costs nothing and guarantees the
// definition always matches the binary that is running — no migration step
// to forget.
//
// Kilograms, not grams, on purpose: the database's internal representation
// is an implementation detail, and a dashboard axis wants the unit a person
// reads.
//
// Two time columns, because the consumers disagree: time_ms is Unix
// milliseconds, the generally useful form, while time_s is Unix seconds
// because that is what frser-sqlite-datasource expects from an integer
// time column — handed milliseconds it overflows and plots the series in
// the 1890s. Dashboards select time_s AS time.
var views = []struct {
	name string
	sql  string
}{
	{
		// period and logical_date duplicate DetectPeriod and logicalDate.
		// TestViewPeriodMatchesGo and TestViewLogicalDateMatchesGo compare
		// the two implementations across every hour of the day so the pair
		// cannot drift silently.
		name: "v_entries",
		sql: `CREATE VIEW v_entries AS
			SELECT
				id,
				recorded_at,
				CAST(strftime('%s', recorded_at) AS INTEGER) * 1000 AS time_ms,
				CAST(strftime('%s', recorded_at) AS INTEGER) AS time_s,
				weight_g,
				weight_g / 1000.0 AS weight_kg,
				COALESCE(
					NULLIF(period_override, ''),
					CASE WHEN CAST(strftime('%H', recorded_at, 'localtime') AS INTEGER) BETWEEN 4 AND 11
						THEN 'morning' ELSE 'evening' END
				) AS period,
				date(
					recorded_at, 'localtime',
					CASE WHEN CAST(strftime('%H', recorded_at, 'localtime') AS INTEGER) < 4
						THEN '-1 day' ELSE '+0 days' END
				) AS logical_date
			FROM entries`,
	},
	{
		// Day-over-day change against the previous reading of the same
		// period, matching what the chart's delta series plots.
		name: "v_entry_deltas",
		sql: `CREATE VIEW v_entry_deltas AS
			SELECT
				id,
				time_ms,
				time_s,
				period,
				weight_kg,
				weight_kg - LAG(weight_kg) OVER (PARTITION BY period ORDER BY time_ms) AS delta_kg
			FROM v_entries`,
	},
	{
		// Each goal contributes a point at the moment it takes effect, and
		// the newest goal gets a second point at "now" so a step line has
		// somewhere to extend to rather than stopping at its start.
		name: "v_goals",
		sql: `CREATE VIEW v_goals AS
			SELECT
				CAST(strftime('%s', effective_from) AS INTEGER) * 1000 AS time_ms,
				CAST(strftime('%s', effective_from) AS INTEGER) AS time_s,
				weight_g / 1000.0 AS goal_kg
			FROM goals
			UNION ALL
			SELECT CAST(strftime('%s', 'now') AS INTEGER) * 1000,
			       CAST(strftime('%s', 'now') AS INTEGER),
			       goal_kg FROM (
				SELECT weight_g / 1000.0 AS goal_kg
				FROM goals
				ORDER BY effective_from DESC
				LIMIT 1
			)`,
	},
	{
		// Shaped for a Grafana annotation query, which wants a time column
		// and a text column.
		name: "v_markers",
		sql: `CREATE VIEW v_markers AS
			SELECT
				CAST(strftime('%s', date) AS INTEGER) * 1000 AS time_ms,
				CAST(strftime('%s', date) AS INTEGER) AS time_s,
				note AS text
			FROM markers`,
	},
}

func applyViews(sqlDB *sql.DB) error {
	for _, v := range views {
		if _, err := sqlDB.Exec(`DROP VIEW IF EXISTS ` + v.name); err != nil {
			return fmt.Errorf("drop view %s: %w", v.name, err)
		}
		if _, err := sqlDB.Exec(v.sql); err != nil {
			return fmt.Errorf("create view %s: %w", v.name, err)
		}
	}
	return nil
}

// applyPragmas configures the connection before any schema work, so the
// migrations below also run under the settings chosen here.
func applyPragmas(sqlDB *sql.DB) error {
	// A contended write waits rather than failing outright with SQLITE_BUSY.
	// SetMaxOpenConns(1) already serializes this process's own queries, but
	// nothing stops a second process — a shell running sqlite3, or an
	// overlapping restart — from holding the write lock briefly.
	if _, err := sqlDB.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		return fmt.Errorf("set busy_timeout: %w", err)
	}

	// WAL keeps a reader from blocking the writer and vice versa, which is
	// what lets a backup read a consistent snapshot while a weigh-in is
	// being logged. It also survives a crash mid-write by replaying the log
	// rather than risking a torn main database file — the failure mode that
	// matters on a Pi running off an SD card.
	//
	// journal_mode is a query: it reports the mode actually in effect, which
	// is not necessarily WAL (it is unsupported on some network filesystems,
	// and an in-memory database reports "memory"). The pragma is persisted
	// in the database file itself, so this is a one-time upgrade rather than
	// something re-applied on every open.
	var journalMode string
	if err := sqlDB.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode); err != nil {
		return fmt.Errorf("set journal_mode: %w", err)
	}
	// Not an error if the filesystem refused WAL: the rollback journal is
	// still correct, just less concurrent. JournalMode reports what stuck so
	// the caller can log it.
	return nil
}

// JournalMode reports the journaling mode in effect, so startup can log
// whether WAL actually took.
func JournalMode(sqlDB *sql.DB) (string, error) {
	var mode string
	if err := sqlDB.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		return "", fmt.Errorf("read journal_mode: %w", err)
	}
	return mode, nil
}

// BackupTo writes a consistent snapshot of the database to path using
// VACUUM INTO, which reads through the same MVCC snapshot as any other
// query — so the copy is internally consistent without stopping the app or
// blocking writes. The output is a compacted, fully self-contained database
// file: no companion -wal or -shm file to keep alongside it.
//
// path must not already exist; VACUUM INTO refuses to overwrite.
func BackupTo(sqlDB *sql.DB, path string) error {
	if _, err := sqlDB.Exec(`VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("vacuum into %s: %w", path, err)
	}
	return nil
}

// KgToGrams converts a kilogram value to whole grams, rounding to the
// nearest gram. This is the only place a user-supplied or imported weight
// loses precision, and it loses none that a bathroom scale provides.
func KgToGrams(kg float64) int64 {
	return int64(math.Round(kg * 1000))
}

// GramsToKg converts stored grams back to kilograms, for display and for
// the float arithmetic (weekly means, the rolling trend) the chart needs.
func GramsToKg(grams int64) float64 {
	return float64(grams) / 1000
}

// weightTableMigrations describes how to rebuild each table that stored
// weight as REAL kilograms. SQLite cannot change a column's type in place,
// so the table is recreated in its new shape, copied across with the
// conversion applied, then swapped in.
var weightTableMigrations = []struct {
	name      string
	createNew string
	copyRows  string
	index     string
}{
	{
		name: "entries",
		createNew: `CREATE TABLE entries_grams_migration (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			recorded_at     TEXT    NOT NULL,
			weight_g        INTEGER NOT NULL,
			period_override TEXT,
			created_at      TEXT    NOT NULL
		)`,
		copyRows: `INSERT INTO entries_grams_migration (id, recorded_at, weight_g, period_override, created_at)
			SELECT id, recorded_at, CAST(ROUND(weight_kg * 1000) AS INTEGER), period_override, created_at FROM entries`,
		index: `CREATE INDEX IF NOT EXISTS idx_entries_recorded_at ON entries (recorded_at)`,
	},
	{
		name: "goals",
		createNew: `CREATE TABLE goals_grams_migration (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			weight_g       INTEGER NOT NULL,
			effective_from TEXT    NOT NULL,
			created_at     TEXT    NOT NULL
		)`,
		copyRows: `INSERT INTO goals_grams_migration (id, weight_g, effective_from, created_at)
			SELECT id, CAST(ROUND(weight_kg * 1000) AS INTEGER), effective_from, created_at FROM goals`,
		index: `CREATE INDEX IF NOT EXISTS idx_goals_effective_from ON goals (effective_from)`,
	},
}

// migrateWeightsToGrams converts any table still holding REAL kilograms to
// whole grams, preserving ids so nothing else in the app has to care that
// rows were rewritten. It is a no-op once done — presence of the weight_g
// column is the marker — and each table is rebuilt inside a transaction, so
// an interrupted run leaves the original table untouched rather than half
// converted.
func migrateWeightsToGrams(sqlDB *sql.DB) error {
	for _, m := range weightTableMigrations {
		migrated, err := hasColumn(sqlDB, m.name, "weight_g")
		if err != nil {
			return fmt.Errorf("inspect %s: %w", m.name, err)
		}
		if migrated {
			continue
		}

		tx, err := sqlDB.Begin()
		if err != nil {
			return fmt.Errorf("begin %s migration: %w", m.name, err)
		}
		// Dropping the old table takes its indexes with it, so the index is
		// recreated after the rename rather than carried over.
		steps := []string{
			m.createNew,
			m.copyRows,
			`DROP TABLE ` + m.name,
			`ALTER TABLE ` + m.name + `_grams_migration RENAME TO ` + m.name,
			m.index,
		}
		for _, step := range steps {
			if _, err := tx.Exec(step); err != nil {
				tx.Rollback()
				return fmt.Errorf("migrate %s: %w", m.name, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s migration: %w", m.name, err)
		}
	}
	return nil
}

// hasColumn reports whether table already has the named column, via
// PRAGMA table_info — SQLite offers no "IF NOT EXISTS" form for either
// ALTER TABLE ADD COLUMN or a type change, so every migration here has to
// check first.
func hasColumn(sqlDB *sql.DB, table, column string) (bool, error) {
	// table is never user input — every caller passes a literal — and
	// PRAGMA cannot be parameterized.
	rows, err := sqlDB.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	// Must be closed before the caller's next statement: SetMaxOpenConns(1)
	// means an open cursor would deadlock waiting on this same connection.
	defer rows.Close()

	for rows.Next() {
		var cid, notNull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// addPeriodOverrideColumn adds entries.period_override for databases created
// before this column existed — a no-op if it's already there, which it
// always is for a database created fresh by the schema above. SQLite's
// ALTER TABLE ADD COLUMN has no "IF NOT EXISTS" form, so presence is
// checked first via PRAGMA table_info.
func addPeriodOverrideColumn(sqlDB *sql.DB) error {
	found, err := hasColumn(sqlDB, "entries", "period_override")
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = sqlDB.Exec(`ALTER TABLE entries ADD COLUMN period_override TEXT`)
	return err
}

// DetectPeriod applies the morning/evening split rule to a clock time: a
// weigh-in from midnight to 4am is treated as a continuation of the
// previous evening (late-night, not a fresh morning), so "morning" only
// starts at 4am.
func DetectPeriod(t time.Time) string {
	if h := t.Hour(); h >= 4 && h < 12 {
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
