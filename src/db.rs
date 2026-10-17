//! The SQLite database: its schema and migrations, and every query.
//!
//! Timestamps are stored as UTC RFC 3339 strings (see [`clock::to_stored`]),
//! weights as whole grams, and a period override as "morning"/"evening" or
//! NULL. Existing databases are in exactly that shape, and must stay
//! readable as they are.

use std::collections::HashSet;
use std::fmt;
use std::path::Path;

use anyhow::{Context as _, Result, anyhow};
use jiff::Zoned;
use opentelemetry::KeyValue;
use r2d2_sqlite::SqliteConnectionManager;
use r2d2_sqlite::rusqlite::types::{
    FromSql, FromSqlError, FromSqlResult, ToSql, ToSqlOutput, ValueRef,
};
use r2d2_sqlite::rusqlite::{self, Connection, OptionalExtension as _, Row, params};
use rusqlite_migration::{M, Migrations};

use crate::clock;
use crate::model::{Entry, Goal, Grams, Marker, Period};

/// Version 1 of the schema. `IF NOT EXISTS` because [`adopt_unversioned`]
/// also runs it, over files that already have some of it.
const SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS entries (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	recorded_at     TEXT    NOT NULL, -- when the weigh-in happened, RFC3339
	weight_g        INTEGER NOT NULL, -- whole grams
	period_override TEXT,             -- 'morning'/'evening'; NULL means detect from recorded_at
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

/// What a weigh-in is created or updated with.
#[derive(Clone, Debug)]
pub struct EntryFields {
    pub recorded_at: Zoned,
    pub weight: Grams,
    pub period_override: Option<Period>,
}

/// The database. One connection: SQLite takes one writer at a time, and
/// queuing this process's queries behind a single connection is simpler than
/// retrying SQLITE_BUSY. Queries run on a blocking thread.
#[derive(Clone)]
pub struct Db {
    pool: r2d2::Pool<SqliteConnectionManager>,
}

impl Db {
    /// Opens the file at `path`, creating it if need be, and brings its
    /// schema up to date. A migration that fails leaves the app refusing to
    /// start rather than serving a half-converted database.
    pub fn open(path: impl AsRef<Path>) -> Result<Db> {
        let manager = SqliteConnectionManager::file(path.as_ref()).with_init(configure);
        let pool = r2d2::Pool::builder()
            .max_size(1)
            .build(manager)
            .context("open sqlite")?;
        let mut conn = pool.get().context("open sqlite")?;
        migrate(&mut conn)?;
        drop(conn);
        Ok(Db { pool })
    }

    /// The journal mode in effect, so startup can say whether WAL took.
    pub fn journal_mode(&self) -> Result<String> {
        let conn = self.pool.get()?;
        Ok(conn.query_row("PRAGMA journal_mode", [], |r| r.get(0))?)
    }

    /// Runs `f` with the connection on a blocking thread, in a `db.<op>` span
    /// of its own, so a slow query shows in a trace as itself rather than as
    /// time in the handler that asked for it.
    async fn run<T: Send + 'static>(
        &self,
        op: &'static str,
        f: impl FnOnce(&mut Connection) -> Result<T> + Send + 'static,
    ) -> Result<T> {
        let pool = self.pool.clone();
        let work = async move {
            tokio::task::spawn_blocking(move || f(&mut *pool.get()?))
                .await
                .map_err(|e| anyhow!("database task: {e}"))?
        };
        let attrs = vec![KeyValue::new("db.system", "sqlite")];
        homelab::trace::op("weight-tracker/db", op, attrs, async {
            work.await.map_err(Chain)
        })
        .await
        .map_err(|Chain(e)| e)
    }

    /// Every weigh-in, newest first.
    pub async fn entries(&self) -> Result<Vec<Entry>> {
        self.run("db.ListEntries", |c| {
            let mut stmt = c.prepare(&format!("{SELECT_ENTRY} ORDER BY recorded_at DESC"))?;
            Ok(stmt
                .query_map([], entry)?
                .collect::<rusqlite::Result<_>>()?)
        })
        .await
    }

    pub async fn add_entry(&self, fields: EntryFields, now: &Zoned) -> Result<i64> {
        let now = Stamp(now.clone());
        self.run("db.CreateEntry", move |c| {
            c.execute(
                "INSERT INTO entries (recorded_at, weight_g, period_override, created_at) VALUES (?, ?, ?, ?)",
                params![Stamp(fields.recorded_at), fields.weight, fields.period_override, now],
            )?;
            Ok(c.last_insert_rowid())
        })
        .await
    }

    /// Whether there was an entry `id` to update.
    pub async fn update_entry(&self, id: i64, fields: EntryFields) -> Result<bool> {
        self.run("db.UpdateEntry", move |c| {
            let changed = c.execute(
                "UPDATE entries SET recorded_at = ?, weight_g = ?, period_override = ? WHERE id = ?",
                params![Stamp(fields.recorded_at), fields.weight, fields.period_override, id],
            )?;
            Ok(changed > 0)
        })
        .await
    }

    /// Whether there was an entry `id` to delete.
    pub async fn delete_entry(&self, id: i64) -> Result<bool> {
        self.delete("db.DeleteEntry", "entries", id).await
    }

    /// Adds every reading not already recorded at the same second, in one
    /// transaction, and returns how many that was. A reading repeated within
    /// `readings` is added once.
    pub async fn import(&self, readings: Vec<(Zoned, Grams)>, now: &Zoned) -> Result<usize> {
        let now = clock::to_stored(now);
        self.run("db.ImportEntries", move |c| {
            let tx = c.transaction()?;
            let mut seen: HashSet<String> = tx
                .prepare("SELECT recorded_at FROM entries")?
                .query_map([], |r| r.get(0))?
                .collect::<rusqlite::Result<_>>()?;
            let mut added = 0;
            {
                let mut insert = tx.prepare(
                    "INSERT INTO entries (recorded_at, weight_g, created_at) VALUES (?, ?, ?)",
                )?;
                for (at, weight) in readings {
                    let at = clock::to_stored(&at);
                    if seen.insert(at.clone()) {
                        insert.execute(params![at, weight, now])?;
                        added += 1;
                    }
                }
            }
            tx.commit()?;
            Ok(added)
        })
        .await
    }

    /// Every goal, newest first.
    pub async fn goals(&self) -> Result<Vec<Goal>> {
        self.run("db.ListGoals", |c| {
            let mut stmt = c.prepare(&format!("{SELECT_GOAL} ORDER BY effective_from DESC"))?;
            Ok(stmt.query_map([], goal)?.collect::<rusqlite::Result<_>>()?)
        })
        .await
    }

    pub async fn add_goal(&self, weight: Grams, from: Zoned, now: &Zoned) -> Result<i64> {
        let now = Stamp(now.clone());
        self.run("db.CreateGoal", move |c| {
            c.execute(
                "INSERT INTO goals (weight_g, effective_from, created_at) VALUES (?, ?, ?)",
                params![weight, Stamp(from), now],
            )?;
            Ok(c.last_insert_rowid())
        })
        .await
    }

    pub async fn update_goal(&self, id: i64, weight: Grams, from: Zoned) -> Result<bool> {
        self.run("db.UpdateGoal", move |c| {
            let changed = c.execute(
                "UPDATE goals SET weight_g = ?, effective_from = ? WHERE id = ?",
                params![weight, Stamp(from), id],
            )?;
            Ok(changed > 0)
        })
        .await
    }

    pub async fn delete_goal(&self, id: i64) -> Result<bool> {
        self.delete("db.DeleteGoal", "goals", id).await
    }

    /// Every marker, newest first.
    pub async fn markers(&self) -> Result<Vec<Marker>> {
        self.run("db.ListMarkers", |c| {
            let mut stmt = c.prepare(&format!("{SELECT_MARKER} ORDER BY date DESC"))?;
            Ok(stmt
                .query_map([], marker)?
                .collect::<rusqlite::Result<_>>()?)
        })
        .await
    }

    pub async fn marker(&self, id: i64) -> Result<Option<Marker>> {
        self.run("db.GetMarker", move |c| {
            Ok(
                c.query_row(&format!("{SELECT_MARKER} WHERE id = ?"), [id], marker)
                    .optional()?,
            )
        })
        .await
    }

    pub async fn add_marker(&self, date: Zoned, note: String, now: &Zoned) -> Result<i64> {
        let now = Stamp(now.clone());
        self.run("db.CreateMarker", move |c| {
            c.execute(
                "INSERT INTO markers (date, note, created_at) VALUES (?, ?, ?)",
                params![Stamp(date), note, now],
            )?;
            Ok(c.last_insert_rowid())
        })
        .await
    }

    pub async fn update_marker(&self, id: i64, date: Zoned, note: String) -> Result<bool> {
        self.run("db.UpdateMarker", move |c| {
            let changed = c.execute(
                "UPDATE markers SET date = ?, note = ? WHERE id = ?",
                params![Stamp(date), note, id],
            )?;
            Ok(changed > 0)
        })
        .await
    }

    pub async fn delete_marker(&self, id: i64) -> Result<bool> {
        self.delete("db.DeleteMarker", "markers", id).await
    }

    /// Removes every entry, goal and marker, together or not at all.
    pub async fn delete_everything(&self) -> Result<()> {
        self.run("db.DeleteAllData", |c| {
            let tx = c.transaction()?;
            tx.execute_batch("DELETE FROM entries; DELETE FROM goals; DELETE FROM markers;")?;
            tx.commit()?;
            Ok(())
        })
        .await
    }

    /// Writes a consistent snapshot of the database to `path`, which must not
    /// exist yet. VACUUM INTO reads through a snapshot like any other query,
    /// so it neither stops nor blocks writers, and it includes what is still
    /// in the write-ahead log — which copying the file would miss. The result
    /// is compacted and self-contained, with no -wal or -shm beside it.
    pub async fn backup_to(&self, path: &Path) -> Result<()> {
        let path = path.to_string_lossy().into_owned();
        self.run("db.BackupTo", move |c| {
            c.execute("VACUUM INTO ?", [&path])
                .with_context(|| format!("vacuum into {path}"))?;
            Ok(())
        })
        .await
    }

    async fn delete(&self, op: &'static str, table: &'static str, id: i64) -> Result<bool> {
        self.run(op, move |c| {
            Ok(c.execute(&format!("DELETE FROM {table} WHERE id = ?"), [id])? > 0)
        })
        .await
    }
}

/// Every connection, before anything else runs on it:
///
/// - busy_timeout: a write contended by another process — a sqlite3 shell,
///   an overlapping restart — waits rather than failing with SQLITE_BUSY;
/// - journal_mode=WAL: a reader never blocks the writer, so a backup reads a
///   consistent snapshot while a weigh-in is logged, and a crash mid-write
///   replays the log rather than tearing the file. It persists in the file,
///   and a filesystem that refuses it is not an error — startup logs what
///   took.
fn configure(c: &mut Connection) -> rusqlite::Result<()> {
    c.busy_timeout(std::time::Duration::from_secs(5))?;
    c.query_row("PRAGMA journal_mode = WAL", [], |r| r.get::<_, String>(0))?;
    Ok(())
}

/// The schema, one step per user_version: a file at version n has had the
/// first n applied. Never edit a step that has shipped; add a new one.
fn migrations() -> Migrations<'static> {
    Migrations::new(vec![M::up(SCHEMA)])
}

/// Brings any database up to the current schema. A file that is ahead of
/// this build — an image rolled back past a schema change — is refused
/// rather than served by code that does not know its shape.
fn migrate(c: &mut Connection) -> Result<()> {
    let version: i64 = c.query_row("PRAGMA user_version", [], |r| r.get(0))?;
    if version == 0 && has_table(c, "entries")? {
        adopt_unversioned(c).context("adopt unversioned database")?;
    }
    migrations().to_latest(c).context("migrate")
}

/// Files from before the schema was versioned are all at user_version 0,
/// in one of three shapes: weights in kilograms with or without
/// period_override, or the version 1 schema itself. Each step here checks
/// before it acts, so whichever shape it finds ends up as version 1, and a
/// run interrupted partway is finished by the next.
fn adopt_unversioned(c: &mut Connection) -> Result<()> {
    c.execute_batch(SCHEMA).context("apply schema")?;
    // Before the grams rebuild, which copies the column across.
    if !has_column(c, "entries", "period_override")? {
        c.execute("ALTER TABLE entries ADD COLUMN period_override TEXT", [])
            .context("add entries.period_override")?;
    }
    for table in &KILOGRAM_TABLES {
        if !has_column(c, table.name, "weight_g")? {
            rebuild_in_grams(c, table)
                .with_context(|| format!("convert {} to grams", table.name))?;
        }
    }
    c.execute_batch("PRAGMA user_version = 1")
        .context("set schema version")
}

/// A table that once stored weight as REAL kilograms, and the shape it is
/// rebuilt into: SQLite cannot change a column's type in place.
struct KilogramTable {
    name: &'static str,
    create: &'static str,
    copy: &'static str,
    index: &'static str,
}

const KILOGRAM_TABLES: [KilogramTable; 2] = [
    KilogramTable {
        name: "entries",
        create: "CREATE TABLE entries_in_grams (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			recorded_at     TEXT    NOT NULL,
			weight_g        INTEGER NOT NULL,
			period_override TEXT,
			created_at      TEXT    NOT NULL
		)",
        copy: "INSERT INTO entries_in_grams (id, recorded_at, weight_g, period_override, created_at)
			SELECT id, recorded_at, CAST(ROUND(weight_kg * 1000) AS INTEGER), period_override, created_at FROM entries",
        index: "CREATE INDEX idx_entries_recorded_at ON entries (recorded_at)",
    },
    KilogramTable {
        name: "goals",
        create: "CREATE TABLE goals_in_grams (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			weight_g       INTEGER NOT NULL,
			effective_from TEXT    NOT NULL,
			created_at     TEXT    NOT NULL
		)",
        copy: "INSERT INTO goals_in_grams (id, weight_g, effective_from, created_at)
			SELECT id, CAST(ROUND(weight_kg * 1000) AS INTEGER), effective_from, created_at FROM goals",
        index: "CREATE INDEX idx_goals_effective_from ON goals (effective_from)",
    },
];

/// Rebuilds `table` with whole grams, keeping every id, in one transaction:
/// interrupted, the original is untouched. Dropping the old table drops its
/// index, so the index is made again on the new one.
fn rebuild_in_grams(c: &mut Connection, table: &KilogramTable) -> Result<()> {
    let tx = c.transaction()?;
    tx.execute(table.create, [])?;
    tx.execute(table.copy, [])?;
    tx.execute(&format!("DROP TABLE {}", table.name), [])?;
    tx.execute(
        &format!("ALTER TABLE {0}_in_grams RENAME TO {0}", table.name),
        [],
    )?;
    tx.execute(table.index, [])?;
    tx.commit()?;
    Ok(())
}

fn has_table(c: &Connection, table: &str) -> Result<bool> {
    Ok(c.query_row(
        "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?",
        [table],
        |_| Ok(()),
    )
    .optional()?
    .is_some())
}

fn has_column(c: &Connection, table: &str, column: &str) -> Result<bool> {
    let mut stmt = c.prepare(&format!("PRAGMA table_info({table})"))?;
    let names = stmt
        .query_map([], |r| r.get::<_, String>("name"))?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    Ok(names.iter().any(|n| n == column))
}

const SELECT_ENTRY: &str = "SELECT id, recorded_at, weight_g, period_override FROM entries";
const SELECT_GOAL: &str = "SELECT id, weight_g, effective_from FROM goals";
const SELECT_MARKER: &str = "SELECT id, date, note FROM markers";

fn entry(r: &Row) -> rusqlite::Result<Entry> {
    Ok(Entry {
        id: r.get("id")?,
        recorded_at: r.get::<_, Stamp>("recorded_at")?.0,
        weight: r.get("weight_g")?,
        period_override: r.get("period_override")?,
    })
}

fn goal(r: &Row) -> rusqlite::Result<Goal> {
    Ok(Goal {
        id: r.get("id")?,
        weight: r.get("weight_g")?,
        effective_from: r.get::<_, Stamp>("effective_from")?.0,
    })
}

fn marker(r: &Row) -> rusqlite::Result<Marker> {
    Ok(Marker {
        id: r.get("id")?,
        date: r.get::<_, Stamp>("date")?.0,
        note: r.get("note")?,
    })
}

/// A timestamp column, in the stored format.
struct Stamp(Zoned);

impl ToSql for Stamp {
    fn to_sql(&self) -> rusqlite::Result<ToSqlOutput<'_>> {
        Ok(clock::to_stored(&self.0).into())
    }
}

impl FromSql for Stamp {
    fn column_result(value: ValueRef<'_>) -> FromSqlResult<Self> {
        clock::from_stored(value.as_str()?)
            .map(Stamp)
            .map_err(|e| FromSqlError::Other(Box::new(e)))
    }
}

impl ToSql for Grams {
    fn to_sql(&self) -> rusqlite::Result<ToSqlOutput<'_>> {
        Ok(self.0.into())
    }
}

impl FromSql for Grams {
    fn column_result(value: ValueRef<'_>) -> FromSqlResult<Self> {
        value.as_i64().map(Grams)
    }
}

impl ToSql for Period {
    fn to_sql(&self) -> rusqlite::Result<ToSqlOutput<'_>> {
        Ok(self.as_str().into())
    }
}

impl FromSql for Period {
    fn column_result(value: ValueRef<'_>) -> FromSqlResult<Self> {
        value
            .as_str()?
            .parse()
            .map_err(|_| FromSqlError::InvalidType)
    }
}

/// An error shown with its whole context chain, for the span to record.
struct Chain(anyhow::Error);

impl fmt::Display for Chain {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{:#}", self.0)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::at;

    struct Fixture {
        _dir: tempfile::TempDir,
        path: std::path::PathBuf,
    }

    fn fixture() -> Fixture {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("test.db");
        Fixture { _dir: dir, path }
    }

    fn open() -> (Fixture, Db) {
        let f = fixture();
        let db = Db::open(&f.path).unwrap();
        (f, db)
    }

    fn fields(t: &str, g: i64, period: Option<Period>) -> EntryFields {
        EntryFields {
            recorded_at: at(t),
            weight: Grams(g),
            period_override: period,
        }
    }

    fn now() -> Zoned {
        Zoned::now()
    }

    impl Db {
        async fn entry(&self, id: i64) -> Result<Option<Entry>> {
            Ok(self.entries().await?.into_iter().find(|e| e.id == id))
        }

        async fn goal(&self, id: i64) -> Result<Option<Goal>> {
            Ok(self.goals().await?.into_iter().find(|g| g.id == id))
        }
    }

    #[tokio::test]
    async fn reopening_keeps_the_data() {
        let f = fixture();
        let db = Db::open(&f.path).unwrap();
        db.add_entry(fields("2026-08-16 07:30", 82400, None), &now())
            .await
            .unwrap();
        drop(db);
        assert_eq!(Db::open(&f.path).unwrap().entries().await.unwrap().len(), 1);
    }

    #[tokio::test]
    async fn entries_round_trip() {
        let (_f, db) = open();
        let t = at("2026-08-16 07:30");
        let id = db
            .add_entry(fields("2026-08-16 07:30", 82400, None), &now())
            .await
            .unwrap();
        let got = db.entry(id).await.unwrap().unwrap();
        assert_eq!((got.weight, got.period_override), (Grams(82400), None));
        assert_eq!(got.recorded_at, t);

        assert!(
            db.update_entry(id, fields("2026-08-16 08:30", 81900, Some(Period::Evening)))
                .await
                .unwrap()
        );
        let got = db.entry(id).await.unwrap().unwrap();
        assert_eq!(
            (got.weight, got.period_override),
            (Grams(81900), Some(Period::Evening))
        );

        assert!(db.delete_entry(id).await.unwrap());
        assert!(db.entry(id).await.unwrap().is_none());
        // Gone is gone: a second delete, or an update, finds nothing.
        assert!(!db.delete_entry(id).await.unwrap());
        assert!(
            !db.update_entry(id, fields("2026-08-16 08:30", 1, None))
                .await
                .unwrap()
        );
    }

    #[tokio::test]
    async fn entries_are_newest_first() {
        let (_f, db) = open();
        for t in ["2026-08-14 07:00", "2026-08-16 07:00", "2026-08-15 07:00"] {
            db.add_entry(fields(t, 82000, None), &now()).await.unwrap();
        }
        let times: Vec<_> = db
            .entries()
            .await
            .unwrap()
            .into_iter()
            .map(|e| e.recorded_at)
            .collect();
        assert!(times.is_sorted_by(|a, b| a >= b), "{times:?}");
        assert!(open().1.entries().await.unwrap().is_empty());
    }

    // Near midnight, where a mishandled conversion would shift the date and
    // the period.
    #[tokio::test]
    async fn timestamps_keep_their_local_wall_clock() {
        let (_f, db) = open();
        let id = db
            .add_entry(fields("2026-08-16 00:30", 82400, None), &now())
            .await
            .unwrap();
        let got = db.entry(id).await.unwrap().unwrap();
        assert_eq!((got.recorded_at.hour(), got.recorded_at.minute()), (0, 30));
        assert_eq!(got.period(), Period::Evening);
    }

    #[tokio::test]
    async fn goals_and_markers_round_trip() {
        let (_f, db) = open();
        let id = db
            .add_goal(Grams(78000), at("2026-08-01 00:00"), &now())
            .await
            .unwrap();
        assert_eq!(
            db.goal(id).await.unwrap().unwrap().effective_from,
            at("2026-08-01 00:00")
        );
        assert!(
            db.update_goal(id, Grams(76000), at("2026-08-01 00:00"))
                .await
                .unwrap()
        );
        assert_eq!(db.goal(id).await.unwrap().unwrap().weight, Grams(76000));
        assert!(db.delete_goal(id).await.unwrap());
        assert!(!db.delete_goal(id).await.unwrap());

        let id = db
            .add_marker(at("2026-08-10 00:00"), "started".into(), &now())
            .await
            .unwrap();
        assert_eq!(db.marker(id).await.unwrap().unwrap().note, "started");
        assert!(
            db.update_marker(id, at("2026-08-10 00:00"), "ended".into())
                .await
                .unwrap()
        );
        assert_eq!(db.markers().await.unwrap()[0].note, "ended");
        assert!(db.delete_marker(id).await.unwrap());
        assert!(!db.delete_marker(999).await.unwrap());
    }

    #[tokio::test]
    async fn import_skips_what_is_already_there() {
        let (_f, db) = open();
        db.add_entry(fields("2026-08-14 07:00", 84000, None), &now())
            .await
            .unwrap();
        let readings = vec![
            (at("2026-08-14 07:00"), Grams(84000)), // already recorded
            (at("2026-08-15 07:00"), Grams(83000)),
            (at("2026-08-15 07:00"), Grams(83500)), // twice in the file
        ];
        assert_eq!(db.import(readings, &now()).await.unwrap(), 1);
        assert_eq!(db.entries().await.unwrap().len(), 2);
        assert_eq!(db.import(vec![], &now()).await.unwrap(), 0);
    }

    #[tokio::test]
    async fn delete_everything_leaves_usable_tables() {
        let (_f, db) = open();
        db.add_entry(fields("2026-08-16 07:30", 82400, None), &now())
            .await
            .unwrap();
        db.add_goal(Grams(78000), at("2026-08-01 00:00"), &now())
            .await
            .unwrap();
        db.add_marker(at("2026-08-10 00:00"), "note".into(), &now())
            .await
            .unwrap();
        db.delete_everything().await.unwrap();
        assert!(db.entries().await.unwrap().is_empty());
        assert!(db.goals().await.unwrap().is_empty());
        assert!(db.markers().await.unwrap().is_empty());
        db.add_entry(fields("2026-08-17 07:30", 82000, None), &now())
            .await
            .unwrap();
    }

    /// What existing rows look like on disk, written by the previous build.
    #[tokio::test]
    async fn reads_rows_in_the_stored_format() {
        let f = fixture();
        drop(Db::open(&f.path).unwrap());
        Connection::open(&f.path)
            .unwrap()
            .execute_batch(
                "INSERT INTO entries (id, recorded_at, weight_g, period_override, created_at) VALUES
                    (7, '2026-08-14T06:00:00Z', 81647, 'evening', '2026-08-14T06:00:00Z');
                 INSERT INTO goals (id, weight_g, effective_from, created_at) VALUES
                    (3, 78000, '2026-07-31T23:00:00Z', '2026-07-31T23:00:00Z');",
            )
            .unwrap();
        let db = Db::open(&f.path).unwrap();
        let e = db.entry(7).await.unwrap().unwrap();
        assert_eq!(
            e.recorded_at.timestamp(),
            "2026-08-14T06:00:00Z".parse().unwrap()
        );
        assert_eq!(
            (e.weight, e.period_override),
            (Grams(81647), Some(Period::Evening))
        );
        assert_eq!(db.goal(3).await.unwrap().unwrap().weight, Grams(78000));

        // And what this build writes is in the same shape.
        db.add_entry(
            fields("2026-08-16 07:30", 82400, None),
            &at("2026-08-16 07:31"),
        )
        .await
        .unwrap();
        let (recorded, created, period): (String, String, Option<String>) = Connection::open(
            &f.path,
        )
        .unwrap()
        .query_row(
            "SELECT recorded_at, created_at, period_override FROM entries ORDER BY id DESC LIMIT 1",
            [],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
        )
        .unwrap();
        assert_eq!(recorded, clock::to_stored(&at("2026-08-16 07:30")));
        assert!(
            recorded.ends_with(":00Z") && created.ends_with('Z'),
            "{recorded} {created}"
        );
        assert_eq!(period, None, "no override is NULL, not an empty string");
    }

    fn seed(path: &Path, sql: &str) {
        Connection::open(path).unwrap().execute_batch(sql).unwrap();
    }

    const OLDEST_SCHEMA: &str = "
        CREATE TABLE entries (
            id          INTEGER PRIMARY KEY AUTOINCREMENT,
            recorded_at TEXT    NOT NULL,
            weight_kg   REAL    NOT NULL,
            created_at  TEXT    NOT NULL
        );
        INSERT INTO entries (recorded_at, weight_kg, created_at)
        VALUES ('2026-08-16T06:30:00Z', 82.4, '2026-08-16T06:30:00Z');";

    const KILOGRAM_SCHEMA: &str = "
        CREATE TABLE entries (
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
            (2, 76.5, '2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z');";

    // A database from before period_override and grams passes through both
    // steps, in order.
    #[tokio::test]
    async fn migrates_the_oldest_schema() {
        let f = fixture();
        seed(&f.path, OLDEST_SCHEMA);
        let db = Db::open(&f.path).unwrap();
        let e = &db.entries().await.unwrap()[0];
        assert_eq!((e.weight, e.period_override), (Grams(82400), None));
        assert!(
            db.update_entry(
                e.id,
                fields("2026-08-16 07:30", 81500, Some(Period::Evening))
            )
            .await
            .unwrap()
        );
    }

    #[tokio::test]
    async fn migrates_kilograms_to_grams() {
        let f = fixture();
        seed(&f.path, KILOGRAM_SCHEMA);
        let db = Db::open(&f.path).unwrap();
        let mut entries = db.entries().await.unwrap();
        entries.sort_by_key(|e| e.id);
        let got: Vec<_> = entries
            .iter()
            .map(|e| (e.id, e.weight.0, e.period_override))
            .collect();
        // What a REAL column printed as 81.64662660000001 becomes an exact
        // 81647 g.
        assert_eq!(
            got,
            [
                (1, 82400, None),
                (2, 83100, Some(Period::Evening)),
                (3, 81647, None)
            ]
        );
        assert_eq!(
            entries[0].recorded_at.timestamp(),
            "2026-08-14T06:00:00Z".parse().unwrap()
        );
        let mut goals: Vec<_> = db
            .goals()
            .await
            .unwrap()
            .into_iter()
            .map(|g| (g.id, g.weight.0))
            .collect();
        goals.sort();
        assert_eq!(goals, [(1, 78000), (2, 76500)]);

        // Ids were kept, so new rows do not collide with them.
        let id = db
            .add_entry(fields("2026-08-16 07:00", 81000, None), &now())
            .await
            .unwrap();
        assert!(id > 3);

        // The indexes the rebuild dropped are back.
        let c = Connection::open(&f.path).unwrap();
        for index in ["idx_entries_recorded_at", "idx_goals_effective_from"] {
            let found: Option<String> = c
                .query_row(
                    "SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?",
                    [index],
                    |r| r.get(0),
                )
                .optional()
                .unwrap();
            assert!(found.is_some(), "{index}");
        }
    }

    fn user_version(path: &Path) -> i64 {
        Connection::open(path)
            .unwrap()
            .query_row("PRAGMA user_version", [], |r| r.get(0))
            .unwrap()
    }

    #[test]
    fn migrations_are_valid() {
        migrations().validate().unwrap();
    }

    #[test]
    fn a_new_file_is_at_the_latest_version() {
        let (f, _db) = open();
        assert_eq!(user_version(&f.path), 1);
    }

    // Every file from before versioning is adopted at version 1, whatever
    // shape it was in.
    #[test]
    fn unversioned_files_are_adopted() {
        for schema in [OLDEST_SCHEMA, KILOGRAM_SCHEMA] {
            let f = fixture();
            seed(&f.path, schema);
            drop(Db::open(&f.path).unwrap());
            assert_eq!(user_version(&f.path), 1);
        }
        // Already in grams: only the version changes.
        let (f, db) = open();
        drop(db);
        seed(&f.path, "PRAGMA user_version = 0");
        drop(Db::open(&f.path).unwrap());
        assert_eq!(user_version(&f.path), 1);
    }

    #[test]
    fn a_file_ahead_of_this_build_is_refused() {
        let (f, db) = open();
        drop(db);
        seed(&f.path, "PRAGMA user_version = 2");
        assert!(Db::open(&f.path).is_err());
    }

    #[tokio::test]
    async fn migrating_again_changes_nothing() {
        let f = fixture();
        seed(&f.path, KILOGRAM_SCHEMA);
        for _ in 0..3 {
            let db = Db::open(&f.path).unwrap();
            let entries = db.entries().await.unwrap();
            assert_eq!(entries.len(), 3);
            assert!(
                entries.iter().all(|e| e.weight.0 > 1000),
                "grams converted twice"
            );
        }
    }

    #[test]
    fn connections_are_configured() {
        let (f, db) = open();
        assert_eq!(db.journal_mode().unwrap(), "wal");
        drop(db);
        // Stored in the file, so it holds across a reopen.
        let db = Db::open(&f.path).unwrap();
        assert_eq!(db.journal_mode().unwrap(), "wal");
        let timeout: i64 = db
            .pool
            .get()
            .unwrap()
            .query_row("PRAGMA busy_timeout", [], |r| r.get(0))
            .unwrap();
        assert_eq!(timeout, 5000);
    }

    #[tokio::test]
    async fn backups_are_complete_and_self_contained() {
        let (_f, db) = open();
        for day in 0..5 {
            let t = format!("2026-08-1{day} 07:30");
            db.add_entry(fields(&t, 82000, Some(Period::Evening)), &now())
                .await
                .unwrap();
        }
        db.add_goal(Grams(78000), at("2026-08-01 00:00"), &now())
            .await
            .unwrap();
        db.add_marker(at("2026-08-10 00:00"), "started".into(), &now())
            .await
            .unwrap();

        let out = tempfile::tempdir().unwrap();
        let path = out.path().join("backup.db");
        db.backup_to(&path).await.unwrap();
        assert!(!out.path().join("backup.db-wal").exists());

        // Writes still in the log are in the copy, and so is everything the
        // CSV cannot carry.
        let restored = Db::open(&path).unwrap();
        let entries = restored.entries().await.unwrap();
        assert_eq!(entries.len(), 5);
        assert!(
            entries
                .iter()
                .all(|e| e.period_override == Some(Period::Evening))
        );
        assert_eq!(restored.goals().await.unwrap().len(), 1);
        assert_eq!(restored.markers().await.unwrap()[0].note, "started");

        // The source carries on.
        db.add_entry(fields("2026-08-20 07:30", 82000, None), &now())
            .await
            .unwrap();
        assert_eq!(db.entries().await.unwrap().len(), 6);

        // An existing file is refused, and left alone.
        std::fs::write(out.path().join("taken.db"), "not a database").unwrap();
        assert!(db.backup_to(&out.path().join("taken.db")).await.is_err());
        assert_eq!(
            std::fs::read_to_string(out.path().join("taken.db")).unwrap(),
            "not a database"
        );
    }
}
