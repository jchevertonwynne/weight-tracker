//! The SQLite schema and per-entity CRUD: entries (weigh-ins), goals, and
//! markers (chart annotations).
//!
//! Weights are stored as whole grams rather than a REAL number of kilograms.
//! A binary float cannot represent most one-decimal kilogram values exactly,
//! so a weight entered as 82.4 came back as 82.400000000000006, and a unit
//! conversion during import (180 lb -> 81.64662660000001 kg) made it worse —
//! visible in the CSV export, which prints the stored value at full
//! precision. Grams are exact, well inside i64, and finer than any scale the
//! app is used with. Kilograms are purely a presentation concern; see
//! [`grams_to_kg`].
//!
//! Timestamps are stored as UTC RFC 3339 strings (see [`clock::stored`]) and
//! read back in the local zone, where the period and same-day comparisons
//! happen.

use std::collections::HashSet;
use std::path::Path;

use anyhow::{Context as _, Result, anyhow};
use jiff::Zoned;
use opentelemetry::KeyValue;
use r2d2_sqlite::SqliteConnectionManager;
use r2d2_sqlite::rusqlite::{Connection, OptionalExtension as _, Row, params};

use crate::clock;

/// Returned (inside the anyhow chain) by the delete functions when no row
/// with the given id exists, so a handler can answer 404 rather than
/// reporting success for a row that was never there — a stale second tab
/// deleting the same entry twice, say.
#[derive(Debug)]
pub struct NotFound;

impl std::fmt::Display for NotFound {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("not found")
    }
}

impl std::error::Error for NotFound {}

pub fn is_not_found(err: &anyhow::Error) -> bool {
    err.chain().any(|e| e.is::<NotFound>())
}

#[derive(Clone, Debug)]
pub struct Entry {
    pub id: i64,
    pub recorded_at: Zoned,
    pub weight_g: i64,
    /// "" (auto-detect from recorded_at via [`detect_period`]), "morning", or
    /// "evening" — set when the user overrides the period detected for a
    /// weigh-in logged close to the morning/evening boundary.
    pub period_override: String,
}

#[derive(Clone, Debug)]
pub struct Goal {
    pub id: i64,
    pub weight_g: i64,
    pub effective_from: Zoned,
}

/// A date-based annotation shown as a vertical reference line on the chart,
/// giving context for why the weight moved ("started new diet").
#[derive(Clone, Debug)]
pub struct Marker {
    pub id: i64,
    pub date: Zoned,
    pub note: String,
}

/// A not-yet-persisted entry, for bulk imports.
pub struct NewEntry {
    pub recorded_at: Zoned,
    pub weight_g: i64,
}

const SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS entries (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	recorded_at     TEXT    NOT NULL, -- when the weigh-in happened, RFC3339
	weight_g        INTEGER NOT NULL, -- whole grams
	period_override TEXT,             -- \"morning\"/\"evening\"; NULL means auto-detect from recorded_at
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
";

/// How to rebuild each table that stored weight as REAL kilograms. SQLite
/// cannot change a column's type in place, so the table is recreated in its
/// new shape, copied across with the conversion applied, then swapped in.
struct WeightTableMigration {
    name: &'static str,
    create_new: &'static str,
    copy_rows: &'static str,
    index: &'static str,
}

const WEIGHT_TABLE_MIGRATIONS: [WeightTableMigration; 2] = [
    WeightTableMigration {
        name: "entries",
        create_new: "CREATE TABLE entries_grams_migration (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			recorded_at     TEXT    NOT NULL,
			weight_g        INTEGER NOT NULL,
			period_override TEXT,
			created_at      TEXT    NOT NULL
		)",
        copy_rows: "INSERT INTO entries_grams_migration (id, recorded_at, weight_g, period_override, created_at)
			SELECT id, recorded_at, CAST(ROUND(weight_kg * 1000) AS INTEGER), period_override, created_at FROM entries",
        index: "CREATE INDEX IF NOT EXISTS idx_entries_recorded_at ON entries (recorded_at)",
    },
    WeightTableMigration {
        name: "goals",
        create_new: "CREATE TABLE goals_grams_migration (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			weight_g       INTEGER NOT NULL,
			effective_from TEXT    NOT NULL,
			created_at     TEXT    NOT NULL
		)",
        copy_rows: "INSERT INTO goals_grams_migration (id, weight_g, effective_from, created_at)
			SELECT id, CAST(ROUND(weight_kg * 1000) AS INTEGER), effective_from, created_at FROM goals",
        index: "CREATE INDEX IF NOT EXISTS idx_goals_effective_from ON goals (effective_from)",
    },
];

/// The database. One connection, as the Go version had: SQLite tolerates one
/// writer at a time, and a single connection avoids SQLITE_BUSY under htmx's
/// concurrent requests. Queries run on a blocking thread, one at a time.
#[derive(Clone)]
pub struct Db {
    pool: r2d2::Pool<SqliteConnectionManager>,
}

/// Every request-path query's span: `db.<op>`, with db.system set, on the
/// tracer named after the Go package it replaces — so a slow query shows up
/// as its own span rather than folded into the handler that called it.
/// Startup work (open, migrations) is not traced; nothing is tracing it yet.
async fn span<T>(
    op: &'static str,
    fut: impl Future<Output = std::result::Result<T, DisplayChain>>,
) -> Result<T> {
    homelab::trace::op(
        "weight-tracker/internal/db",
        op,
        vec![KeyValue::new("db.system", "sqlite")],
        fut,
    )
    .await
    .map_err(|e| e.0)
}

/// anyhow's own Display is only the outermost context; the span records the
/// whole chain, as Go's wrapped error did.
struct DisplayChain(anyhow::Error);

impl std::fmt::Display for DisplayChain {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{:#}", self.0)
    }
}

impl Db {
    /// Opens (creating if need be) the file at `path` and brings its schema
    /// up to date. An error here is a startup failure: if a migration fails
    /// the app refuses to start rather than serving against a half-converted
    /// database.
    pub fn open(path: impl AsRef<Path>) -> Result<Db> {
        let manager = SqliteConnectionManager::file(path.as_ref()).with_init(apply_pragmas);
        let pool = r2d2::Pool::builder()
            .max_size(1)
            .build(manager)
            .context("open sqlite")?;
        let mut conn = pool.get().context("open sqlite")?;
        conn.execute_batch(SCHEMA).context("apply schema")?;
        // Order matters: the grams migration copies period_override across,
        // so that column has to exist first on a database old enough to
        // predate it.
        add_period_override_column(&conn).context("migrate period_override column")?;
        migrate_weights_to_grams(&mut conn).context("migrate weights to grams")?;
        drop(conn);
        Ok(Db { pool })
    }

    /// Runs `f` on a blocking thread with the connection.
    async fn with<T: Send + 'static>(
        &self,
        f: impl FnOnce(&mut Connection) -> Result<T> + Send + 'static,
    ) -> Result<T> {
        let pool = self.pool.clone();
        tokio::task::spawn_blocking(move || {
            let mut conn = pool.get().context("database connection")?;
            f(&mut conn)
        })
        .await
        .map_err(|e| anyhow!("database task: {e}"))?
    }

    async fn traced<T: Send + 'static>(
        &self,
        op: &'static str,
        f: impl FnOnce(&mut Connection) -> Result<T> + Send + 'static,
    ) -> Result<T> {
        span(op, async { self.with(f).await.map_err(DisplayChain) }).await
    }

    /// The journaling mode in effect, so startup can say whether WAL took.
    pub fn journal_mode(&self) -> Result<String> {
        let conn = self.pool.get().context("database connection")?;
        conn.query_row("PRAGMA journal_mode", [], |r| r.get(0))
            .context("read journal_mode")
    }

    pub async fn create_entry(
        &self,
        recorded_at: &Zoned,
        weight_g: i64,
        period_override: &str,
        created_at: &Zoned,
    ) -> Result<i64> {
        let args = (
            clock::stored(recorded_at),
            weight_g,
            null_if_empty(period_override),
            clock::stored(created_at),
        );
        self.traced("db.CreateEntry", move |c| {
            c.execute(
                "INSERT INTO entries (recorded_at, weight_g, period_override, created_at) VALUES (?, ?, ?, ?)",
                params![args.0, args.1, args.2, args.3],
            )
            .context("insert entry")?;
            Ok(c.last_insert_rowid())
        })
        .await
    }

    pub async fn update_entry(
        &self,
        id: i64,
        recorded_at: &Zoned,
        weight_g: i64,
        period_override: &str,
    ) -> Result<()> {
        let args = (
            clock::stored(recorded_at),
            weight_g,
            null_if_empty(period_override),
        );
        self.traced("db.UpdateEntry", move |c| {
            c.execute(
                "UPDATE entries SET recorded_at = ?, weight_g = ?, period_override = ? WHERE id = ?",
                params![args.0, args.1, args.2, id],
            )
            .with_context(|| format!("update entry {id}"))?;
            Ok(())
        })
        .await
    }

    pub async fn delete_entry(&self, id: i64) -> Result<()> {
        self.delete_by_id("db.delete entries", "entries", id).await
    }

    pub async fn get_entry(&self, id: i64) -> Result<Entry> {
        self.traced("db.GetEntry", move |c| {
            c.query_row(
                "SELECT id, recorded_at, weight_g, COALESCE(period_override, '') FROM entries WHERE id = ?",
                [id],
                raw_entry,
            )
            .optional()
            .with_context(|| format!("get entry {id}"))?
            .ok_or_else(|| anyhow!("get entry {id}: sql: no rows in result set"))?
            .parse()
            .with_context(|| format!("parse entry {id}"))
        })
        .await
    }

    /// Every entry, newest first.
    pub async fn list_entries(&self) -> Result<Vec<Entry>> {
        self.traced("db.ListEntries", |c| {
            let mut stmt = c.prepare(
                "SELECT id, recorded_at, weight_g, COALESCE(period_override, '') FROM entries ORDER BY recorded_at DESC",
            )?;
            let rows = stmt
                .query_map([], raw_entry)
                .context("list entries")?
                .collect::<Result<Vec<_>, _>>()
                .context("scan entry")?;
            rows.into_iter().map(RawEntry::parse).collect()
        })
        .await
    }

    /// The recorded_at values present, exactly as stored, for de-duplicating
    /// a bulk import in memory.
    pub async fn existing_recorded_at_set(&self) -> Result<HashSet<String>> {
        self.traced("db.ExistingRecordedAtSet", |c| {
            let mut stmt = c.prepare("SELECT recorded_at FROM entries")?;
            let set = stmt
                .query_map([], |r| r.get(0))
                .context("list recorded_at")?
                .collect::<Result<HashSet<String>, _>>()
                .context("scan recorded_at")?;
            Ok(set)
        })
        .await
    }

    /// Inserts `rows` in one transaction, skipping any whose recorded_at
    /// (exactly as stored) is already in `existing` — which is updated as it
    /// goes, so duplicates within `rows` itself are skipped too. Returns the
    /// count actually inserted, and the updated set.
    pub async fn bulk_create_entries(
        &self,
        rows: Vec<NewEntry>,
        mut existing: HashSet<String>,
        created_at: &Zoned,
    ) -> Result<(usize, HashSet<String>)> {
        let created = clock::stored(created_at);
        let keyed: Vec<(String, i64)> = rows
            .iter()
            .map(|r| (clock::stored(&r.recorded_at), r.weight_g))
            .collect();
        self.traced("db.BulkCreateEntries", move |c| {
            let tx = c.transaction().context("begin import tx")?;
            let mut inserted = 0;
            {
                let mut stmt = tx
                    .prepare(
                        "INSERT INTO entries (recorded_at, weight_g, created_at) VALUES (?, ?, ?)",
                    )
                    .context("prepare insert")?;
                for (key, weight_g) in keyed {
                    if existing.contains(&key) {
                        continue;
                    }
                    stmt.execute(params![key, weight_g, created])
                        .with_context(|| format!("insert row at {key}"))?;
                    existing.insert(key);
                    inserted += 1;
                }
            }
            tx.commit()?;
            Ok((inserted, existing))
        })
        .await
    }

    pub async fn create_goal(
        &self,
        weight_g: i64,
        effective_from: &Zoned,
        created_at: &Zoned,
    ) -> Result<i64> {
        let args = (clock::stored(effective_from), clock::stored(created_at));
        self.traced("db.CreateGoal", move |c| {
            c.execute(
                "INSERT INTO goals (weight_g, effective_from, created_at) VALUES (?, ?, ?)",
                params![weight_g, args.0, args.1],
            )
            .context("insert goal")?;
            Ok(c.last_insert_rowid())
        })
        .await
    }

    pub async fn update_goal(&self, id: i64, weight_g: i64, effective_from: &Zoned) -> Result<()> {
        let from = clock::stored(effective_from);
        self.traced("db.UpdateGoal", move |c| {
            c.execute(
                "UPDATE goals SET weight_g = ?, effective_from = ? WHERE id = ?",
                params![weight_g, from, id],
            )
            .with_context(|| format!("update goal {id}"))?;
            Ok(())
        })
        .await
    }

    pub async fn delete_goal(&self, id: i64) -> Result<()> {
        self.delete_by_id("db.delete goals", "goals", id).await
    }

    pub async fn get_goal(&self, id: i64) -> Result<Goal> {
        self.traced("db.GetGoal", move |c| {
            c.query_row(
                "SELECT id, weight_g, effective_from FROM goals WHERE id = ?",
                [id],
                raw_goal,
            )
            .optional()
            .with_context(|| format!("get goal {id}"))?
            .ok_or_else(|| anyhow!("get goal {id}: sql: no rows in result set"))?
            .parse()
            .with_context(|| format!("parse goal {id}"))
        })
        .await
    }

    /// Every goal, newest first.
    pub async fn list_goals(&self) -> Result<Vec<Goal>> {
        self.traced("db.ListGoals", |c| {
            let mut stmt = c.prepare(
                "SELECT id, weight_g, effective_from FROM goals ORDER BY effective_from DESC",
            )?;
            let rows = stmt
                .query_map([], raw_goal)
                .context("list goals")?
                .collect::<Result<Vec<_>, _>>()
                .context("scan goal")?;
            rows.into_iter().map(RawGoal::parse).collect()
        })
        .await
    }

    pub async fn create_marker(&self, date: &Zoned, note: &str, created_at: &Zoned) -> Result<i64> {
        let args = (
            clock::stored(date),
            note.to_string(),
            clock::stored(created_at),
        );
        self.traced("db.CreateMarker", move |c| {
            c.execute(
                "INSERT INTO markers (date, note, created_at) VALUES (?, ?, ?)",
                params![args.0, args.1, args.2],
            )
            .context("insert marker")?;
            Ok(c.last_insert_rowid())
        })
        .await
    }

    pub async fn update_marker(&self, id: i64, date: &Zoned, note: &str) -> Result<()> {
        let args = (clock::stored(date), note.to_string());
        self.traced("db.UpdateMarker", move |c| {
            c.execute(
                "UPDATE markers SET date = ?, note = ? WHERE id = ?",
                params![args.0, args.1, id],
            )
            .with_context(|| format!("update marker {id}"))?;
            Ok(())
        })
        .await
    }

    pub async fn delete_marker(&self, id: i64) -> Result<()> {
        self.delete_by_id("db.delete markers", "markers", id).await
    }

    pub async fn get_marker(&self, id: i64) -> Result<Marker> {
        self.traced("db.GetMarker", move |c| {
            c.query_row(
                "SELECT id, date, note FROM markers WHERE id = ?",
                [id],
                raw_marker,
            )
            .optional()
            .with_context(|| format!("get marker {id}"))?
            .ok_or_else(|| anyhow!("get marker {id}: sql: no rows in result set"))?
            .parse()
            .with_context(|| format!("parse marker {id}"))
        })
        .await
    }

    /// Every marker, newest first.
    pub async fn list_markers(&self) -> Result<Vec<Marker>> {
        self.traced("db.ListMarkers", |c| {
            let mut stmt = c.prepare("SELECT id, date, note FROM markers ORDER BY date DESC")?;
            let rows = stmt
                .query_map([], raw_marker)
                .context("list markers")?
                .collect::<Result<Vec<_>, _>>()
                .context("scan marker")?;
            rows.into_iter().map(RawMarker::parse).collect()
        })
        .await
    }

    /// Removes every entry, goal, and marker in one transaction.
    pub async fn delete_all_data(&self) -> Result<()> {
        self.traced("db.DeleteAllData", |c| {
            let tx = c.transaction().context("begin delete-all tx")?;
            tx.execute("DELETE FROM entries", [])
                .context("delete entries")?;
            tx.execute("DELETE FROM goals", [])
                .context("delete goals")?;
            tx.execute("DELETE FROM markers", [])
                .context("delete markers")?;
            tx.commit()?;
            Ok(())
        })
        .await
    }

    /// Writes a consistent snapshot of the database to `path` with VACUUM
    /// INTO, which reads through the same snapshot as any other query — so
    /// the copy is internally consistent without stopping the app or
    /// blocking writes, and it carries what is still in the write-ahead log.
    /// The output is a compacted, self-contained database file: no companion
    /// -wal or -shm to keep alongside it.
    ///
    /// `path` must not already exist; VACUUM INTO refuses to overwrite.
    pub async fn backup_to(&self, path: &Path) -> Result<()> {
        let path = path.to_string_lossy().into_owned();
        self.traced("db.BackupTo", move |c| {
            c.execute("VACUUM INTO ?", [&path])
                .with_context(|| format!("vacuum into {path}"))?;
            Ok(())
        })
        .await
    }

    /// A single-row delete, with [`NotFound`] when it matched nothing.
    async fn delete_by_id(&self, op: &'static str, table: &'static str, id: i64) -> Result<()> {
        self.traced(op, move |c| {
            // table is never user input — every caller passes a literal — and
            // SQLite cannot parameterize a table name.
            let affected = c
                .execute(&format!("DELETE FROM {table} WHERE id = ?"), [id])
                .with_context(|| format!("delete from {table} {id}"))?;
            if affected == 0 {
                return Err(
                    anyhow::Error::new(NotFound).context(format!("delete from {table} {id}"))
                );
            }
            Ok(())
        })
        .await
    }
}

/// Configures each connection before any schema work, so the migrations also
/// run under these settings.
///
/// - busy_timeout: a contended write waits rather than failing at once with
///   SQLITE_BUSY. The single connection already serialises this process's
///   own queries, but nothing stops a second process — a shell running
///   sqlite3, an overlapping restart — holding the write lock briefly.
/// - journal_mode=WAL: a reader never blocks the writer, which is what lets a
///   backup read a consistent snapshot while a weigh-in is logged, and a
///   crash mid-write replays the log rather than risking a torn file. It is
///   persisted in the file, and not an error if the filesystem refuses it:
///   startup logs what took.
fn apply_pragmas(c: &mut Connection) -> Result<(), r2d2_sqlite::rusqlite::Error> {
    c.execute_batch("PRAGMA busy_timeout = 5000;")?;
    let _: String = c.query_row("PRAGMA journal_mode = WAL", [], |r| r.get(0))?;
    Ok(())
}

/// Whether `table` already has `column`. SQLite has no IF NOT EXISTS for
/// ADD COLUMN or a type change, so every migration here checks first.
fn has_column(c: &Connection, table: &str, column: &str) -> Result<bool> {
    // table is never user input, and PRAGMA cannot be parameterized.
    let mut stmt = c.prepare(&format!("PRAGMA table_info({table})"))?;
    let names = stmt
        .query_map([], |r| r.get::<_, String>(1))?
        .collect::<Result<Vec<_>, _>>()?;
    Ok(names.iter().any(|n| n == column))
}

/// Adds entries.period_override to a database created before it existed — a
/// no-op on any database created fresh by the schema above.
fn add_period_override_column(c: &Connection) -> Result<()> {
    if has_column(c, "entries", "period_override")? {
        return Ok(());
    }
    c.execute("ALTER TABLE entries ADD COLUMN period_override TEXT", [])?;
    Ok(())
}

/// Converts any table still holding REAL kilograms to whole grams, keeping
/// ids so nothing else has to care that rows were rewritten. A no-op once
/// done — the weight_g column is the marker — and each table is rebuilt in a
/// transaction, so an interrupted run leaves the original untouched rather
/// than half converted.
fn migrate_weights_to_grams(c: &mut Connection) -> Result<()> {
    for m in &WEIGHT_TABLE_MIGRATIONS {
        if has_column(c, m.name, "weight_g").with_context(|| format!("inspect {}", m.name))? {
            continue;
        }
        let tx = c
            .transaction()
            .with_context(|| format!("begin {} migration", m.name))?;
        // Dropping the old table takes its indexes with it, so the index is
        // recreated after the rename rather than carried over.
        let drop = format!("DROP TABLE {}", m.name);
        let rename = format!("ALTER TABLE {0}_grams_migration RENAME TO {0}", m.name);
        for step in [m.create_new, m.copy_rows, &drop, &rename, m.index] {
            tx.execute(step, [])
                .with_context(|| format!("migrate {}", m.name))?;
        }
        tx.commit()
            .with_context(|| format!("commit {} migration", m.name))?;
    }
    Ok(())
}

fn null_if_empty(s: &str) -> Option<String> {
    (!s.is_empty()).then(|| s.to_string())
}

/// A row as read, before its timestamps are parsed. created_at is written
/// but never read back: nothing shows it. — parsing happens outside
/// rusqlite's row callback so its error keeps its own type.
struct RawEntry(i64, String, i64, String);
struct RawGoal(i64, i64, String);
struct RawMarker(i64, String, String);

fn raw_entry(r: &Row) -> Result<RawEntry, r2d2_sqlite::rusqlite::Error> {
    Ok(RawEntry(r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?))
}

fn raw_goal(r: &Row) -> Result<RawGoal, r2d2_sqlite::rusqlite::Error> {
    Ok(RawGoal(r.get(0)?, r.get(1)?, r.get(2)?))
}

fn raw_marker(r: &Row) -> Result<RawMarker, r2d2_sqlite::rusqlite::Error> {
    Ok(RawMarker(r.get(0)?, r.get(1)?, r.get(2)?))
}

impl RawEntry {
    fn parse(self) -> Result<Entry> {
        Ok(Entry {
            id: self.0,
            recorded_at: clock::parse_stored(&self.1).context("parse recorded_at")?,
            weight_g: self.2,
            period_override: self.3,
        })
    }
}

impl RawGoal {
    fn parse(self) -> Result<Goal> {
        Ok(Goal {
            id: self.0,
            weight_g: self.1,
            effective_from: clock::parse_stored(&self.2).context("parse effective_from")?,
        })
    }
}

impl RawMarker {
    fn parse(self) -> Result<Marker> {
        Ok(Marker {
            id: self.0,
            date: clock::parse_stored(&self.1).context("parse date")?,
            note: self.2,
        })
    }
}

/// The morning/evening split on a clock time: a weigh-in from midnight to
/// 4am continues the previous evening (late night, not a fresh morning), so
/// "morning" only starts at 4am.
pub fn detect_period(t: &Zoned) -> &'static str {
    if (4..12).contains(&t.hour()) {
        "morning"
    } else {
        "evening"
    }
}

/// Kilograms to whole grams, to the nearest gram. The only place a
/// user-supplied or imported weight loses precision, and it loses none that a
/// bathroom scale provides.
pub fn kg_to_grams(kg: f64) -> i64 {
    // Go's math.Round: halves away from zero, as f64::round.
    (kg * 1000.0).round() as i64
}

/// Stored grams back to kilograms, for display and for the float arithmetic
/// (weekly means, the rolling trend) the chart needs.
pub fn grams_to_kg(grams: i64) -> f64 {
    grams as f64 / 1000.0
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::clock::tests::at;

    fn temp() -> (tempfile::TempDir, std::path::PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("test.db");
        (dir, path)
    }

    fn open_test() -> (tempfile::TempDir, Db) {
        let (dir, path) = temp();
        (dir, Db::open(path).unwrap())
    }

    fn now() -> Zoned {
        Zoned::now()
    }

    #[test]
    fn detect_period_cases() {
        for (t, want) in [
            ("2026-08-16 00:00", "evening"), // midnight is still last night
            ("2026-08-16 03:59", "evening"),
            ("2026-08-16 04:00", "morning"), // the cutoff
            ("2026-08-16 07:30", "morning"),
            ("2026-08-16 11:59", "morning"),
            ("2026-08-16 12:00", "evening"),
            ("2026-08-16 23:59", "evening"),
        ] {
            assert_eq!(detect_period(&at(t)), want, "{t}");
        }
    }

    #[tokio::test]
    async fn open_is_idempotent() {
        let (_dir, path) = temp();
        let first = Db::open(&path).unwrap();
        first
            .create_entry(&at("2026-08-16 07:30"), 82400, "", &now())
            .await
            .unwrap();
        drop(first);
        // Re-opening an existing database must preserve its data.
        let second = Db::open(&path).unwrap();
        assert_eq!(second.list_entries().await.unwrap().len(), 1);
    }

    #[tokio::test]
    async fn entry_round_trip() {
        let (_dir, db) = open_test();
        let recorded_at = at("2026-08-16 07:30");
        let id = db
            .create_entry(&recorded_at, 82400, "", &now())
            .await
            .unwrap();

        let got = db.get_entry(id).await.unwrap();
        assert_eq!(got.weight_g, 82400);
        // Stored as UTC, but must come back as the same instant.
        assert_eq!(got.recorded_at.timestamp(), recorded_at.timestamp());
        assert_eq!(got.period_override, "", "stored as NULL");

        let later = clock::add(&recorded_at, jiff::SignedDuration::from_hours(1));
        db.update_entry(id, &later, 81900, "evening").await.unwrap();
        let got = db.get_entry(id).await.unwrap();
        assert_eq!(
            (got.weight_g, got.period_override.as_str()),
            (81900, "evening")
        );

        db.delete_entry(id).await.unwrap();
        assert!(db.get_entry(id).await.is_err());
    }

    #[tokio::test]
    async fn list_entries_is_newest_first() {
        let (_dir, db) = open_test();
        for t in ["2026-08-14 07:00", "2026-08-16 07:00", "2026-08-15 07:00"] {
            db.create_entry(&at(t), 82, "", &now()).await.unwrap();
        }
        let entries = db.list_entries().await.unwrap();
        assert_eq!(entries.len(), 3);
        for w in entries.windows(2) {
            assert!(w[0].recorded_at >= w[1].recorded_at, "want newest-first");
        }
    }

    #[tokio::test]
    async fn list_entries_on_empty_database() {
        let (_dir, db) = open_test();
        assert!(db.list_entries().await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn goal_round_trip() {
        let (_dir, db) = open_test();
        let from = at("2026-08-01 00:00");
        let id = db.create_goal(78000, &from, &now()).await.unwrap();
        let got = db.get_goal(id).await.unwrap();
        assert_eq!(got.weight_g, 78000);
        assert_eq!(got.effective_from.timestamp(), from.timestamp());
        db.update_goal(id, 76000, &from).await.unwrap();
        assert_eq!(db.get_goal(id).await.unwrap().weight_g, 76000);
        db.delete_goal(id).await.unwrap();
    }

    #[tokio::test]
    async fn marker_round_trip() {
        let (_dir, db) = open_test();
        let date = at("2026-08-10 00:00");
        let id = db
            .create_marker(&date, "started cutting", &now())
            .await
            .unwrap();
        let got = db.get_marker(id).await.unwrap();
        assert_eq!(got.note, "started cutting");
        assert_eq!(got.date.timestamp(), date.timestamp());
        db.update_marker(id, &date, "ended cutting").await.unwrap();
        assert_eq!(db.get_marker(id).await.unwrap().note, "ended cutting");
        db.delete_marker(id).await.unwrap();
    }

    #[tokio::test]
    async fn delete_missing_row_reports_not_found() {
        let (_dir, db) = open_test();
        assert!(is_not_found(&db.delete_entry(999).await.unwrap_err()));
        assert!(is_not_found(&db.delete_goal(999).await.unwrap_err()));
        assert!(is_not_found(&db.delete_marker(999).await.unwrap_err()));
        assert_eq!(
            db.delete_entry(999).await.unwrap_err().to_string(),
            "delete from entries 999"
        );
    }

    #[tokio::test]
    async fn delete_twice_reports_not_found_the_second_time() {
        let (_dir, db) = open_test();
        let id = db
            .create_entry(&at("2026-08-16 07:30"), 82400, "", &now())
            .await
            .unwrap();
        db.delete_entry(id).await.unwrap();
        assert!(is_not_found(&db.delete_entry(id).await.unwrap_err()));
    }

    #[tokio::test]
    async fn existing_recorded_at_set() {
        let (_dir, db) = open_test();
        let recorded_at = at("2026-08-16 07:30");
        db.create_entry(&recorded_at, 82400, "", &now())
            .await
            .unwrap();
        // Keyed by the stored (UTC RFC3339) representation.
        let set = db.existing_recorded_at_set().await.unwrap();
        assert!(set.contains(&clock::stored(&recorded_at)), "{set:?}");
    }

    fn new(t: &str, g: i64) -> NewEntry {
        NewEntry {
            recorded_at: at(t),
            weight_g: g,
        }
    }

    #[tokio::test]
    async fn bulk_create_entries() {
        // Inserts every new row.
        let (_dir, db) = open_test();
        let rows = vec![
            new("2026-08-14 07:00", 84000),
            new("2026-08-15 07:00", 83000),
        ];
        let (n, _) = db
            .bulk_create_entries(rows, HashSet::new(), &now())
            .await
            .unwrap();
        assert_eq!(n, 2);

        // Skips rows already in the database.
        let (_dir, db) = open_test();
        db.create_entry(&at("2026-08-14 07:00"), 84000, "", &now())
            .await
            .unwrap();
        let existing = db.existing_recorded_at_set().await.unwrap();
        let rows = vec![
            new("2026-08-14 07:00", 84000),
            new("2026-08-15 07:00", 83000),
        ];
        let (n, _) = db
            .bulk_create_entries(rows, existing, &now())
            .await
            .unwrap();
        assert_eq!(n, 1, "the duplicate should be skipped");
        assert_eq!(db.list_entries().await.unwrap().len(), 2);

        // De-duplicates within the batch itself.
        let (_dir, db) = open_test();
        let rows = vec![
            new("2026-08-14 07:00", 84000),
            new("2026-08-14 07:00", 84500),
        ];
        let (n, _) = db
            .bulk_create_entries(rows, HashSet::new(), &now())
            .await
            .unwrap();
        assert_eq!(n, 1, "the batch contains the same timestamp twice");

        // An empty batch is a no-op.
        let (_dir, db) = open_test();
        let (n, _) = db
            .bulk_create_entries(vec![], HashSet::new(), &now())
            .await
            .unwrap();
        assert_eq!(n, 0);
    }

    #[tokio::test]
    async fn delete_all_data() {
        let (_dir, db) = open_test();
        db.create_entry(&at("2026-08-16 07:30"), 82400, "", &now())
            .await
            .unwrap();
        db.create_goal(78000, &at("2026-08-01 00:00"), &now())
            .await
            .unwrap();
        db.create_marker(&at("2026-08-10 00:00"), "note", &now())
            .await
            .unwrap();
        db.delete_all_data().await.unwrap();
        assert!(db.list_entries().await.unwrap().is_empty());
        assert!(db.list_goals().await.unwrap().is_empty());
        assert!(db.list_markers().await.unwrap().is_empty());
        // The tables must still be usable afterwards.
        db.create_entry(&at("2026-08-17 07:30"), 82000, "", &now())
            .await
            .unwrap();
    }

    fn seed(path: &Path, sql: &str) {
        Connection::open(path).unwrap().execute_batch(sql).unwrap();
    }

    // The upgrade path for a database created before period_override
    // existed — the case CREATE TABLE IF NOT EXISTS silently skips.
    #[tokio::test]
    async fn migration_adds_period_override_column() {
        let (_dir, path) = temp();
        seed(
            &path,
            "CREATE TABLE entries (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				recorded_at TEXT    NOT NULL,
				weight_kg   REAL    NOT NULL,
				created_at  TEXT    NOT NULL
			);
			INSERT INTO entries (recorded_at, weight_kg, created_at)
			VALUES ('2026-08-16T06:30:00Z', 82.4, '2026-08-16T06:30:00Z');",
        );
        let db = Db::open(&path).unwrap();
        let entries = db.list_entries().await.unwrap();
        assert_eq!(entries.len(), 1, "the pre-existing row preserved");
        assert_eq!(
            entries[0].weight_g, 82400,
            "legacy 82.4 kg converted to grams"
        );
        assert_eq!(entries[0].period_override, "");
        // The new column must be writable on the migrated table.
        db.update_entry(entries[0].id, &entries[0].recorded_at, 82400, "evening")
            .await
            .unwrap();
        assert_eq!(
            db.get_entry(entries[0].id).await.unwrap().period_override,
            "evening"
        );
    }

    // Guards the PRAGMA-then-ALTER approach, which would fail with
    // "duplicate column name" if the presence check regressed.
    #[test]
    fn migration_is_safe_to_run_twice() {
        let (_dir, path) = temp();
        for _ in 0..3 {
            Db::open(&path).unwrap();
        }
    }

    #[tokio::test]
    async fn stored_timestamps_survive_a_timezone_round_trip() {
        let (_dir, db) = open_test();
        // Near midnight, where a mishandled conversion would shift the date
        // and change the detected period.
        let recorded_at = at("2026-08-16 00:30");
        let id = db
            .create_entry(&recorded_at, 82400, "", &now())
            .await
            .unwrap();
        let got = db.get_entry(id).await.unwrap();
        assert_eq!(got.recorded_at.timestamp(), recorded_at.timestamp());
        assert_eq!((got.recorded_at.hour(), got.recorded_at.minute()), (0, 30));
        assert_eq!(detect_period(&got.recorded_at), "evening");
    }

    #[test]
    fn kg_to_grams_and_back() {
        for (name, kg, want) in [
            ("a whole kilogram", 82.0, 82000),
            ("one decimal place", 82.4, 82400),
            ("three decimal places", 81.647, 81647),
            ("rounds half up", 81.6465, 81647),
            ("rounds down below the half gram", 81.64649, 81646),
            (
                "a float with representation error",
                180.0 * 0.45359237,
                81647,
            ),
            ("a tiny weight", 0.001, 1),
        ] {
            assert_eq!(kg_to_grams(kg), want, "{name}");
        }
        // Grams are canonical, so grams -> kg -> grams must hold.
        for grams in [1, 999, 82000, 82400, 81647, 250000] {
            assert_eq!(kg_to_grams(grams_to_kg(grams)), grams);
        }
        for (g, kg) in [(82000, 82.0), (82400, 82.4), (81647, 81.647), (0, 0.0)] {
            assert_eq!(grams_to_kg(g), kg);
        }
    }

    /// A database in the pre-grams shape, entries and goals both holding REAL
    /// kilograms.
    fn seed_legacy_kilograms() -> (tempfile::TempDir, std::path::PathBuf) {
        let (dir, path) = temp();
        seed(
            &path,
            "CREATE TABLE entries (
				id              INTEGER PRIMARY KEY AUTOINCREMENT,
				recorded_at     TEXT    NOT NULL,
				weight_kg       REAL    NOT NULL,
				period_override TEXT,
				created_at      TEXT    NOT NULL
			);
			CREATE INDEX idx_entries_recorded_at ON entries (recorded_at);
			INSERT INTO entries (id, recorded_at, weight_kg, period_override, created_at) VALUES
				(1, '2026-08-14T06:00:00Z', 82.4,              NULL,      '2026-08-14T06:00:00Z'),
				(2, '2026-08-14T20:00:00Z', 83.1,              'evening', '2026-08-14T20:00:00Z'),
				(3, '2026-08-15T06:00:00Z', 81.64662660000001, NULL,      '2026-08-15T06:00:00Z');

			CREATE TABLE goals (
				id             INTEGER PRIMARY KEY AUTOINCREMENT,
				weight_kg      REAL    NOT NULL,
				effective_from TEXT    NOT NULL,
				created_at     TEXT    NOT NULL
			);
			CREATE INDEX idx_goals_effective_from ON goals (effective_from);
			INSERT INTO goals (id, weight_kg, effective_from, created_at) VALUES
				(1, 78.0, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z'),
				(2, 76.5, '2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z');",
        );
        (dir, path)
    }

    #[tokio::test]
    async fn migration_converts_kilograms_to_grams() {
        let (_dir, path) = seed_legacy_kilograms();
        let db = Db::open(&path).unwrap();
        let entries = db.list_entries().await.unwrap();
        assert_eq!(entries.len(), 3, "all 3 preserved");
        let by_id = |id| entries.iter().find(|e| e.id == id).unwrap();
        assert_eq!(by_id(1).weight_g, 82400);
        assert_eq!(by_id(2).weight_g, 83100);
        // The point of it: what a REAL column printed as 81.64662660000001
        // becomes an exact 81647 g.
        assert_eq!(by_id(3).weight_g, 81647);
        assert_eq!(by_id(2).period_override, "evening");
        assert_eq!(by_id(1).period_override, "");
        assert_eq!(
            by_id(1).recorded_at.timestamp().to_string(),
            "2026-08-14T06:00:00Z"
        );

        let goals = db.list_goals().await.unwrap();
        assert_eq!(goals.len(), 2);
        let goal = |id| goals.iter().find(|g| g.id == id).unwrap().weight_g;
        assert_eq!((goal(1), goal(2)), (78000, 76500));
    }

    #[tokio::test]
    async fn migration_preserves_autoincrement_after_rebuild() {
        let (_dir, path) = seed_legacy_kilograms();
        let db = Db::open(&path).unwrap();
        // Ids were copied explicitly, so the next insert must not collide.
        let id = db
            .create_entry(&at("2026-08-16 07:00"), 81000, "", &now())
            .await
            .unwrap();
        assert!(id > 3, "new entry got id {id}");
    }

    #[test]
    fn migration_recreates_indexes() {
        let (_dir, path) = seed_legacy_kilograms();
        drop(Db::open(&path).unwrap());
        let c = Connection::open(&path).unwrap();
        for want in ["idx_entries_recorded_at", "idx_goals_effective_from"] {
            let found: Option<String> = c
                .query_row(
                    "SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?",
                    [want],
                    |r| r.get(0),
                )
                .optional()
                .unwrap();
            assert!(found.is_some(), "index {want} missing after migration");
        }
    }

    #[tokio::test]
    async fn migration_is_idempotent_across_reopens() {
        let (_dir, path) = seed_legacy_kilograms();
        for i in 0..3 {
            let db = Db::open(&path).unwrap();
            let entries = db.list_entries().await.unwrap();
            assert_eq!(entries.len(), 3, "open {i}: lost or duplicated rows");
            assert_ne!(entries[0].weight_g, 0, "open {i}: re-converted grams");
        }
    }

    // A database predating both period_override and grams passes through each
    // migration in turn — the ordering open depends on.
    #[tokio::test]
    async fn migration_from_the_oldest_schema() {
        let (_dir, path) = temp();
        seed(
            &path,
            "CREATE TABLE entries (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				recorded_at TEXT    NOT NULL,
				weight_kg   REAL    NOT NULL,
				created_at  TEXT    NOT NULL
			);
			INSERT INTO entries (recorded_at, weight_kg, created_at)
			VALUES ('2026-08-16T06:30:00Z', 82.4, '2026-08-16T06:30:00Z');",
        );
        let db = Db::open(&path).unwrap();
        let entries = db.list_entries().await.unwrap();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].weight_g, 82400);
        assert_eq!(entries[0].period_override, "");
        db.update_entry(entries[0].id, &entries[0].recorded_at, 81500, "evening")
            .await
            .unwrap();
    }

    #[test]
    fn open_enables_wal_and_it_persists() {
        let (_dir, path) = temp();
        assert_eq!(Db::open(&path).unwrap().journal_mode().unwrap(), "wal");
        // Stored in the file, so it holds across a reopen.
        assert_eq!(Db::open(&path).unwrap().journal_mode().unwrap(), "wal");
    }

    #[test]
    fn busy_timeout_is_set() {
        let (_dir, db) = open_test();
        let c = db.pool.get().unwrap();
        let timeout: i64 = c
            .query_row("PRAGMA busy_timeout", [], |r| r.get(0))
            .unwrap();
        assert_eq!(timeout, 5000);
    }

    #[tokio::test]
    async fn backup_produces_a_restorable_copy() {
        let (_dir, db) = open_test();
        db.create_entry(&at("2026-08-16 07:30"), 82400, "evening", &now())
            .await
            .unwrap();
        db.create_goal(78000, &at("2026-08-01 00:00"), &now())
            .await
            .unwrap();
        db.create_marker(&at("2026-08-10 00:00"), "started cutting", &now())
            .await
            .unwrap();

        let out = tempfile::tempdir().unwrap();
        let backup = out.path().join("backup.db");
        db.backup_to(&backup).await.unwrap();
        // Self-contained: no companion -wal needed.
        assert!(!out.path().join("backup.db-wal").exists());

        let restored = Db::open(&backup).unwrap();
        let entries = restored.list_entries().await.unwrap();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].weight_g, 82400);
        // Everything the CSV export cannot carry survives too.
        assert_eq!(entries[0].period_override, "evening");
        let goals = restored.list_goals().await.unwrap();
        assert_eq!((goals.len(), goals[0].weight_g), (1, 78000));
        let markers = restored.list_markers().await.unwrap();
        assert_eq!(
            (markers.len(), markers[0].note.as_str()),
            (1, "started cutting")
        );
    }

    // Under WAL a recent commit lives in the log rather than the main file,
    // so a naive file copy could miss it. VACUUM INTO must not.
    #[tokio::test]
    async fn backup_captures_writes_made_after_open() {
        let (_dir, db) = open_test();
        for i in 0..5 {
            let t = clock::add_date(&at("2026-08-16 07:30"), 0, 0, i);
            db.create_entry(&t, 82000, "", &now()).await.unwrap();
        }
        let out = tempfile::tempdir().unwrap();
        let backup = out.path().join("backup.db");
        db.backup_to(&backup).await.unwrap();
        assert_eq!(
            Db::open(&backup)
                .unwrap()
                .list_entries()
                .await
                .unwrap()
                .len(),
            5
        );
    }

    #[tokio::test]
    async fn backup_refuses_to_overwrite() {
        let (_dir, db) = open_test();
        let out = tempfile::tempdir().unwrap();
        let path = out.path().join("existing.db");
        std::fs::write(&path, "not a database").unwrap();
        assert!(db.backup_to(&path).await.is_err());
        assert_eq!(std::fs::read_to_string(&path).unwrap(), "not a database");
    }

    #[tokio::test]
    async fn backup_leaves_the_source_usable() {
        let (_dir, db) = open_test();
        db.create_entry(&at("2026-08-16 07:30"), 82400, "", &now())
            .await
            .unwrap();
        let out = tempfile::tempdir().unwrap();
        db.backup_to(&out.path().join("backup.db")).await.unwrap();
        db.create_entry(&at("2026-08-17 07:30"), 82000, "", &now())
            .await
            .unwrap();
        assert_eq!(db.list_entries().await.unwrap().len(), 2);
    }
}
