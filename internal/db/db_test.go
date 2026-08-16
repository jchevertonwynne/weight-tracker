package db

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openTest opens a throwaway database in a per-test temp directory.
func openTest(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return sqlDB
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		t.Fatalf("parse test time %q: %v", s, err)
	}
	return parsed
}

func TestDetectPeriod(t *testing.T) {
	tests := []struct {
		at   string
		want string
	}{
		{"2026-08-16 00:00", "evening"}, // midnight is still last night
		{"2026-08-16 03:59", "evening"},
		{"2026-08-16 04:00", "morning"}, // the cutoff
		{"2026-08-16 07:30", "morning"},
		{"2026-08-16 11:59", "morning"},
		{"2026-08-16 12:00", "evening"},
		{"2026-08-16 23:59", "evening"},
	}
	for _, tc := range tests {
		t.Run(tc.at, func(t *testing.T) {
			if got := DetectPeriod(at(t, tc.at)); got != tc.want {
				t.Errorf("DetectPeriod(%s) = %q, want %q", tc.at, got, tc.want)
			}
		})
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := CreateEntry(first, at(t, "2026-08-16 07:30"), 82400, "", time.Now()); err != nil {
		t.Fatalf("create entry: %v", err)
	}
	first.Close()

	// Re-opening an existing database must preserve its data, not reset it.
	second, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer second.Close()
	entries, err := ListEntries(second)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d entries after reopening, want 1", len(entries))
	}
}

func TestEntryRoundTrip(t *testing.T) {
	sqlDB := openTest(t)
	recordedAt := at(t, "2026-08-16 07:30")

	id, err := CreateEntry(sqlDB, recordedAt, 82400, "", time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := GetEntry(sqlDB, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WeightG != 82400 {
		t.Errorf("WeightG = %v, want 82400", got.WeightG)
	}
	// Timestamps are stored as UTC but must come back as the same instant,
	// rendered in local time for the period/same-day comparisons.
	if !got.RecordedAt.Equal(recordedAt) {
		t.Errorf("RecordedAt = %v, want %v", got.RecordedAt, recordedAt)
	}
	if got.PeriodOverride != "" {
		t.Errorf("PeriodOverride = %q, want empty (stored as NULL)", got.PeriodOverride)
	}

	if err := UpdateEntry(sqlDB, id, recordedAt.Add(time.Hour), 81900, "evening"); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = GetEntry(sqlDB, id)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.WeightG != 81900 || got.PeriodOverride != "evening" {
		t.Errorf("after update: %v g, override %q; want 81900 / evening", got.WeightG, got.PeriodOverride)
	}

	if err := DeleteEntry(sqlDB, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := GetEntry(sqlDB, id); err == nil {
		t.Error("get after delete returned no error, want one")
	}
}

func TestListEntriesIsNewestFirst(t *testing.T) {
	sqlDB := openTest(t)
	for _, ts := range []string{"2026-08-14 07:00", "2026-08-16 07:00", "2026-08-15 07:00"} {
		if _, err := CreateEntry(sqlDB, at(t, ts), 82, "", time.Now()); err != nil {
			t.Fatalf("create %s: %v", ts, err)
		}
	}
	entries, err := ListEntries(sqlDB)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].RecordedAt.After(entries[i-1].RecordedAt) {
			t.Errorf("entry %d is newer than entry %d; want newest-first", i, i-1)
		}
	}
}

func TestListEntriesOnEmptyDatabase(t *testing.T) {
	entries, err := ListEntries(openTest(t))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want none", len(entries))
	}
}

func TestGoalRoundTrip(t *testing.T) {
	sqlDB := openTest(t)
	effectiveFrom := at(t, "2026-08-01 00:00")

	id, err := CreateGoal(sqlDB, 78000, effectiveFrom, time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := GetGoal(sqlDB, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WeightG != 78000 || !got.EffectiveFrom.Equal(effectiveFrom) {
		t.Errorf("got %v g from %v, want 78000 from %v", got.WeightG, got.EffectiveFrom, effectiveFrom)
	}

	if err := UpdateGoal(sqlDB, id, 76000, effectiveFrom); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ = GetGoal(sqlDB, id); got.WeightG != 76000 {
		t.Errorf("after update: %v g, want 76000", got.WeightG)
	}
	if err := DeleteGoal(sqlDB, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	sqlDB := openTest(t)
	date := at(t, "2026-08-10 00:00")

	id, err := CreateMarker(sqlDB, date, "started cutting", time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := GetMarker(sqlDB, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Note != "started cutting" || !got.Date.Equal(date) {
		t.Errorf("got %q on %v, want %q on %v", got.Note, got.Date, "started cutting", date)
	}

	if err := UpdateMarker(sqlDB, id, date, "ended cutting"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ = GetMarker(sqlDB, id); got.Note != "ended cutting" {
		t.Errorf("after update: %q", got.Note)
	}
	if err := DeleteMarker(sqlDB, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestDeleteMissingRowReportsNotFound(t *testing.T) {
	sqlDB := openTest(t)
	tests := []struct {
		name   string
		delete func(*sql.DB, int64) error
	}{
		{"entry", DeleteEntry},
		{"goal", DeleteGoal},
		{"marker", DeleteMarker},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.delete(sqlDB, 999)
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestDeleteTwiceReportsNotFoundTheSecondTime(t *testing.T) {
	sqlDB := openTest(t)
	id, err := CreateEntry(sqlDB, at(t, "2026-08-16 07:30"), 82400, "", time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := DeleteEntry(sqlDB, id); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := DeleteEntry(sqlDB, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete err = %v, want ErrNotFound", err)
	}
}

func TestExistingRecordedAtSet(t *testing.T) {
	sqlDB := openTest(t)
	recordedAt := at(t, "2026-08-16 07:30")
	if _, err := CreateEntry(sqlDB, recordedAt, 82400, "", time.Now()); err != nil {
		t.Fatalf("create: %v", err)
	}
	set, err := ExistingRecordedAtSet(sqlDB)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	// The set is keyed by the stored (UTC RFC3339) representation.
	if _, ok := set[recordedAt.UTC().Format(time.RFC3339)]; !ok {
		t.Errorf("set %v is missing the entry's timestamp", set)
	}
}

func TestBulkCreateEntries(t *testing.T) {
	t.Run("inserts every new row", func(t *testing.T) {
		sqlDB := openTest(t)
		rows := []NewEntry{
			{RecordedAt: at(t, "2026-08-14 07:00"), WeightG: 84000},
			{RecordedAt: at(t, "2026-08-15 07:00"), WeightG: 83000},
		}
		inserted, err := BulkCreateEntries(sqlDB, rows, map[string]struct{}{}, time.Now())
		if err != nil {
			t.Fatalf("bulk create: %v", err)
		}
		if inserted != 2 {
			t.Errorf("inserted %d, want 2", inserted)
		}
	})

	t.Run("skips rows already in the database", func(t *testing.T) {
		sqlDB := openTest(t)
		existingAt := at(t, "2026-08-14 07:00")
		if _, err := CreateEntry(sqlDB, existingAt, 84000, "", time.Now()); err != nil {
			t.Fatalf("seed: %v", err)
		}
		existing, err := ExistingRecordedAtSet(sqlDB)
		if err != nil {
			t.Fatalf("set: %v", err)
		}
		rows := []NewEntry{
			{RecordedAt: existingAt, WeightG: 84000},
			{RecordedAt: at(t, "2026-08-15 07:00"), WeightG: 83000},
		}
		inserted, err := BulkCreateEntries(sqlDB, rows, existing, time.Now())
		if err != nil {
			t.Fatalf("bulk create: %v", err)
		}
		if inserted != 1 {
			t.Errorf("inserted %d, want 1 (the duplicate should be skipped)", inserted)
		}
		entries, _ := ListEntries(sqlDB)
		if len(entries) != 2 {
			t.Errorf("database holds %d entries, want 2", len(entries))
		}
	})

	t.Run("de-duplicates within the batch itself", func(t *testing.T) {
		sqlDB := openTest(t)
		sameTime := at(t, "2026-08-14 07:00")
		rows := []NewEntry{
			{RecordedAt: sameTime, WeightG: 84000},
			{RecordedAt: sameTime, WeightG: 84500},
		}
		inserted, err := BulkCreateEntries(sqlDB, rows, map[string]struct{}{}, time.Now())
		if err != nil {
			t.Fatalf("bulk create: %v", err)
		}
		if inserted != 1 {
			t.Errorf("inserted %d, want 1 — the batch contains the same timestamp twice", inserted)
		}
	})

	t.Run("an empty batch is a no-op", func(t *testing.T) {
		sqlDB := openTest(t)
		inserted, err := BulkCreateEntries(sqlDB, nil, map[string]struct{}{}, time.Now())
		if err != nil {
			t.Fatalf("bulk create: %v", err)
		}
		if inserted != 0 {
			t.Errorf("inserted %d, want 0", inserted)
		}
	})
}

func TestDeleteAllData(t *testing.T) {
	sqlDB := openTest(t)
	if _, err := CreateEntry(sqlDB, at(t, "2026-08-16 07:30"), 82400, "", time.Now()); err != nil {
		t.Fatalf("create entry: %v", err)
	}
	if _, err := CreateGoal(sqlDB, 78000, at(t, "2026-08-01 00:00"), time.Now()); err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := CreateMarker(sqlDB, at(t, "2026-08-10 00:00"), "note", time.Now()); err != nil {
		t.Fatalf("create marker: %v", err)
	}

	if err := DeleteAllData(sqlDB); err != nil {
		t.Fatalf("delete all: %v", err)
	}
	entries, _ := ListEntries(sqlDB)
	goals, _ := ListGoals(sqlDB)
	markers, _ := ListMarkers(sqlDB)
	if len(entries) != 0 || len(goals) != 0 || len(markers) != 0 {
		t.Errorf("after delete-all: %d/%d/%d entries/goals/markers, want none", len(entries), len(goals), len(markers))
	}

	// The tables must still be usable afterwards.
	if _, err := CreateEntry(sqlDB, at(t, "2026-08-17 07:30"), 82000, "", time.Now()); err != nil {
		t.Errorf("create after delete-all: %v", err)
	}
}

// TestMigrationAddsPeriodOverrideColumn exercises the upgrade path for a
// database created before period_override existed — the case the schema's
// CREATE TABLE IF NOT EXISTS silently skips.
func TestMigrationAddsPeriodOverrideColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE entries (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			recorded_at TEXT    NOT NULL,
			weight_kg   REAL    NOT NULL,
			created_at  TEXT    NOT NULL
		);
		INSERT INTO entries (recorded_at, weight_kg, created_at)
		VALUES ('2026-08-16T06:30:00Z', 82.4, '2026-08-16T06:30:00Z');
	`)
	if err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	legacy.Close()

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("open (migrate): %v", err)
	}
	defer migrated.Close()

	entries, err := ListEntries(migrated)
	if err != nil {
		t.Fatalf("list after migration: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want the pre-existing row preserved", len(entries))
	}
	if entries[0].WeightG != 82400 {
		t.Errorf("WeightG = %v, want 82400 — the legacy 82.4 kg converted to grams", entries[0].WeightG)
	}
	if entries[0].PeriodOverride != "" {
		t.Errorf("PeriodOverride = %q, want empty for a legacy row", entries[0].PeriodOverride)
	}

	// The new column must be writable on the migrated table.
	if err := UpdateEntry(migrated, entries[0].ID, entries[0].RecordedAt, 82400, "evening"); err != nil {
		t.Fatalf("update with an override after migration: %v", err)
	}
	updated, err := GetEntry(migrated, entries[0].ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if updated.PeriodOverride != "evening" {
		t.Errorf("PeriodOverride = %q, want evening", updated.PeriodOverride)
	}
}

// TestMigrationIsSafeToRunTwice guards the PRAGMA-then-ALTER approach, which
// would fail with "duplicate column name" if the presence check regressed.
func TestMigrationIsSafeToRunTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	for i := range 3 {
		sqlDB, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i+1, err)
		}
		sqlDB.Close()
	}
}

func TestStoredTimestampsSurviveATimezoneRoundTrip(t *testing.T) {
	sqlDB := openTest(t)
	// A time deliberately near midnight, where a mishandled zone conversion
	// would shift the calendar date and change the detected period.
	recordedAt := at(t, "2026-08-16 00:30")
	id, err := CreateEntry(sqlDB, recordedAt, 82400, "", time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := GetEntry(sqlDB, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.RecordedAt.Equal(recordedAt) {
		t.Errorf("RecordedAt = %v, want the same instant as %v", got.RecordedAt, recordedAt)
	}
	if got.RecordedAt.Hour() != 0 || got.RecordedAt.Minute() != 30 {
		t.Errorf("local wall clock = %02d:%02d, want 00:30", got.RecordedAt.Hour(), got.RecordedAt.Minute())
	}
	if DetectPeriod(got.RecordedAt) != "evening" {
		t.Error("a 00:30 reading did not come back as evening")
	}
}

func TestKgToGramsAndBack(t *testing.T) {
	tests := []struct {
		name string
		kg   float64
		want int64
	}{
		{"a whole kilogram", 82, 82000},
		{"one decimal place", 82.4, 82400},
		{"three decimal places", 81.647, 81647},
		{"rounds half up", 81.6465, 81647},
		{"rounds down below the half gram", 81.64649, 81646},
		{"a float with representation error", 180 * 0.45359237, 81647},
		{"a tiny weight", 0.001, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := KgToGrams(tc.kg); got != tc.want {
				t.Errorf("KgToGrams(%v) = %d, want %d", tc.kg, got, tc.want)
			}
		})
	}

	// Grams are the canonical value, so the round trip that must hold is
	// grams -> kg -> grams, not the other way (a kg float can carry more
	// precision than a whole gram can represent).
	for _, grams := range []int64{1, 999, 82000, 82400, 81647, 250000} {
		if got := KgToGrams(GramsToKg(grams)); got != grams {
			t.Errorf("%d g round-tripped to %d g", grams, got)
		}
	}
}

func TestGramsToKg(t *testing.T) {
	tests := []struct {
		grams int64
		want  float64
	}{
		{82000, 82},
		{82400, 82.4},
		{81647, 81.647},
		{0, 0},
	}
	for _, tc := range tests {
		if got := GramsToKg(tc.grams); got != tc.want {
			t.Errorf("GramsToKg(%d) = %v, want %v", tc.grams, got, tc.want)
		}
	}
}

// seedLegacyKilogramDatabase creates a database in the pre-grams shape —
// entries and goals both holding REAL kilograms — and returns its path.
func seedLegacyKilogramDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	defer legacy.Close()

	_, err = legacy.Exec(`
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
			(2, 76.5, '2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z');
	`)
	if err != nil {
		t.Fatalf("seed legacy kilogram schema: %v", err)
	}
	return path
}

func TestMigrationConvertsKilogramsToGrams(t *testing.T) {
	path := seedLegacyKilogramDatabase(t)

	sqlDB, err := Open(path)
	if err != nil {
		t.Fatalf("open (migrate): %v", err)
	}
	defer sqlDB.Close()

	entries, err := ListEntries(sqlDB)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want all 3 preserved", len(entries))
	}

	// ListEntries is newest-first, so index by id instead of position.
	byID := make(map[int64]Entry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	wantGrams := map[int64]int64{
		1: 82400,
		2: 83100,
		// The whole point of the migration: a value that a REAL column had
		// been printing as 81.64662660000001 becomes an exact 81647 g.
		3: 81647,
	}
	for id, want := range wantGrams {
		if got := byID[id].WeightG; got != want {
			t.Errorf("entry %d: %d g, want %d", id, got, want)
		}
	}

	// Everything else about each row must survive untouched.
	if byID[2].PeriodOverride != "evening" {
		t.Errorf("entry 2 period_override = %q, want evening", byID[2].PeriodOverride)
	}
	if byID[1].PeriodOverride != "" {
		t.Errorf("entry 1 period_override = %q, want empty", byID[1].PeriodOverride)
	}
	wantRecordedAt := time.Date(2026, 8, 14, 6, 0, 0, 0, time.UTC)
	if !byID[1].RecordedAt.Equal(wantRecordedAt) {
		t.Errorf("entry 1 recorded_at = %v, want %v", byID[1].RecordedAt, wantRecordedAt)
	}

	goals, err := ListGoals(sqlDB)
	if err != nil {
		t.Fatalf("list goals: %v", err)
	}
	if len(goals) != 2 {
		t.Fatalf("got %d goals, want 2", len(goals))
	}
	goalsByID := make(map[int64]Goal, len(goals))
	for _, g := range goals {
		goalsByID[g.ID] = g
	}
	if got := goalsByID[1].WeightG; got != 78000 {
		t.Errorf("goal 1 = %d g, want 78000", got)
	}
	if got := goalsByID[2].WeightG; got != 76500 {
		t.Errorf("goal 2 = %d g, want 76500", got)
	}
}

func TestMigrationPreservesAutoincrementAfterRebuild(t *testing.T) {
	path := seedLegacyKilogramDatabase(t)
	sqlDB, err := Open(path)
	if err != nil {
		t.Fatalf("open (migrate): %v", err)
	}
	defer sqlDB.Close()

	// Ids were copied explicitly, so the next insert must not collide with
	// an existing row.
	id, err := CreateEntry(sqlDB, at(t, "2026-08-16 07:00"), 81000, "", time.Now())
	if err != nil {
		t.Fatalf("create after migration: %v", err)
	}
	if id <= 3 {
		t.Errorf("new entry got id %d, want one past the migrated rows", id)
	}
}

func TestMigrationRecreatesIndexes(t *testing.T) {
	path := seedLegacyKilogramDatabase(t)
	sqlDB, err := Open(path)
	if err != nil {
		t.Fatalf("open (migrate): %v", err)
	}
	defer sqlDB.Close()

	// Dropping the old table takes its indexes with it; the migration has to
	// put them back or every range query degrades to a scan.
	for _, want := range []string{"idx_entries_recorded_at", "idx_goals_effective_from"} {
		var name string
		err := sqlDB.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, want).Scan(&name)
		if err != nil {
			t.Errorf("index %s missing after migration: %v", want, err)
		}
	}
}

func TestMigrationIsIdempotentAcrossReopens(t *testing.T) {
	path := seedLegacyKilogramDatabase(t)

	for i := range 3 {
		sqlDB, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i+1, err)
		}
		entries, err := ListEntries(sqlDB)
		if err != nil {
			t.Fatalf("list entries on open %d: %v", i+1, err)
		}
		if len(entries) != 3 {
			t.Fatalf("open %d: got %d entries, want 3 — a repeated migration lost or duplicated rows", i+1, len(entries))
		}
		if entries[0].WeightG == 0 {
			t.Fatalf("open %d: weight is zero, a second migration re-converted already-converted grams", i+1)
		}
		sqlDB.Close()
	}
}

// TestMigrationFromTheOldestSchema covers a database predating both
// period_override and grams, so it has to pass through each migration in
// turn — the ordering Open depends on.
func TestMigrationFromTheOldestSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ancient.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE entries (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			recorded_at TEXT    NOT NULL,
			weight_kg   REAL    NOT NULL,
			created_at  TEXT    NOT NULL
		);
		INSERT INTO entries (recorded_at, weight_kg, created_at)
		VALUES ('2026-08-16T06:30:00Z', 82.4, '2026-08-16T06:30:00Z');
	`)
	if err != nil {
		t.Fatalf("seed ancient schema: %v", err)
	}
	legacy.Close()

	sqlDB, err := Open(path)
	if err != nil {
		t.Fatalf("open (migrate): %v", err)
	}
	defer sqlDB.Close()

	entries, err := ListEntries(sqlDB)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].WeightG != 82400 {
		t.Errorf("WeightG = %d, want 82400", entries[0].WeightG)
	}
	if entries[0].PeriodOverride != "" {
		t.Errorf("PeriodOverride = %q, want empty", entries[0].PeriodOverride)
	}
	// Both new columns must be writable on the doubly-migrated table.
	if err := UpdateEntry(sqlDB, entries[0].ID, entries[0].RecordedAt, 81500, "evening"); err != nil {
		t.Fatalf("update after migration: %v", err)
	}
}

func TestOpenEnablesWAL(t *testing.T) {
	sqlDB := openTest(t)
	mode, err := JournalMode(sqlDB)
	if err != nil {
		t.Fatalf("journal mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal mode = %q, want wal", mode)
	}
}

func TestWALPersistsAcrossReopen(t *testing.T) {
	// journal_mode is stored in the database file, so a database created by
	// an older build picks WAL up on its next open and keeps it thereafter.
	path := filepath.Join(t.TempDir(), "test.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer second.Close()
	mode, err := JournalMode(second)
	if err != nil {
		t.Fatalf("journal mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal mode after reopen = %q, want wal", mode)
	}
}

func TestBusyTimeoutIsSet(t *testing.T) {
	sqlDB := openTest(t)
	var timeout int
	if err := sqlDB.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}

func TestBackupToProducesARestorableCopy(t *testing.T) {
	sqlDB := openTest(t)
	if _, err := CreateEntry(sqlDB, at(t, "2026-08-16 07:30"), 82400, "evening", time.Now()); err != nil {
		t.Fatalf("create entry: %v", err)
	}
	if _, err := CreateGoal(sqlDB, 78000, at(t, "2026-08-01 00:00"), time.Now()); err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := CreateMarker(sqlDB, at(t, "2026-08-10 00:00"), "started cutting", time.Now()); err != nil {
		t.Fatalf("create marker: %v", err)
	}

	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err := BackupTo(sqlDB, backupPath); err != nil {
		t.Fatalf("backup: %v", err)
	}

	// The snapshot must be a self-contained database: openable on its own,
	// with no companion -wal file needed.
	if _, err := os.Stat(backupPath + "-wal"); !os.IsNotExist(err) {
		t.Errorf("backup left a -wal file alongside it; it should be self-contained")
	}

	restored, err := Open(backupPath)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer restored.Close()

	entries, err := ListEntries(restored)
	if err != nil {
		t.Fatalf("list entries from backup: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("backup holds %d entries, want 1", len(entries))
	}
	if entries[0].WeightG != 82400 {
		t.Errorf("backup entry = %d g, want 82400", entries[0].WeightG)
	}
	// Everything the CSV export cannot carry must survive too.
	if entries[0].PeriodOverride != "evening" {
		t.Errorf("backup lost the period override: %q", entries[0].PeriodOverride)
	}
	goals, err := ListGoals(restored)
	if err != nil {
		t.Fatalf("list goals from backup: %v", err)
	}
	if len(goals) != 1 || goals[0].WeightG != 78000 {
		t.Errorf("backup goals = %+v, want one 78000 g goal", goals)
	}
	markers, err := ListMarkers(restored)
	if err != nil {
		t.Fatalf("list markers from backup: %v", err)
	}
	if len(markers) != 1 || markers[0].Note != "started cutting" {
		t.Errorf("backup markers = %+v, want one 'started cutting' marker", markers)
	}
}

func TestBackupCapturesWritesMadeAfterOpen(t *testing.T) {
	// Under WAL a recent commit lives in the log rather than the main file,
	// so a naive file copy could miss it. VACUUM INTO must not.
	sqlDB := openTest(t)
	for i := range 5 {
		if _, err := CreateEntry(sqlDB, at(t, "2026-08-16 07:30").AddDate(0, 0, i), 82000, "", time.Now()); err != nil {
			t.Fatalf("create entry %d: %v", i, err)
		}
	}
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err := BackupTo(sqlDB, backupPath); err != nil {
		t.Fatalf("backup: %v", err)
	}
	restored, err := Open(backupPath)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer restored.Close()
	entries, err := ListEntries(restored)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 5 {
		t.Errorf("backup holds %d entries, want all 5 including uncheckpointed writes", len(entries))
	}
}

func TestBackupRefusesToOverwrite(t *testing.T) {
	sqlDB := openTest(t)
	path := filepath.Join(t.TempDir(), "existing.db")
	if err := os.WriteFile(path, []byte("not a database"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := BackupTo(sqlDB, path); err == nil {
		t.Error("BackupTo overwrote an existing file, want an error")
	}
	// The original file must be untouched.
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(content) != "not a database" {
		t.Errorf("existing file was modified: %q", content)
	}
}

func TestBackupLeavesTheSourceUsable(t *testing.T) {
	sqlDB := openTest(t)
	if _, err := CreateEntry(sqlDB, at(t, "2026-08-16 07:30"), 82400, "", time.Now()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := BackupTo(sqlDB, filepath.Join(t.TempDir(), "backup.db")); err != nil {
		t.Fatalf("backup: %v", err)
	}
	// VACUUM must not have left the connection in a broken state.
	if _, err := CreateEntry(sqlDB, at(t, "2026-08-17 07:30"), 82000, "", time.Now()); err != nil {
		t.Errorf("write after backup: %v", err)
	}
	entries, err := ListEntries(sqlDB)
	if err != nil {
		t.Fatalf("list after backup: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("source holds %d entries after backup, want 2", len(entries))
	}
}

// TestViewPeriodMatchesGo is the guard against the one real hazard of
// having Grafana compute periods in SQL: two implementations of the same
// rule, drifting apart unnoticed. Every hour of the day is checked against
// DetectPeriod, which is what the app itself uses.
func TestViewPeriodMatchesGo(t *testing.T) {
	sqlDB := openTest(t)
	for hour := range 24 {
		recordedAt := time.Date(2026, 8, 16, hour, 30, 0, 0, time.Local)
		if _, err := CreateEntry(sqlDB, recordedAt, 80000, "", time.Now()); err != nil {
			t.Fatalf("create entry at %02d:30: %v", hour, err)
		}
	}

	rows, err := sqlDB.Query(`SELECT recorded_at, period FROM v_entries ORDER BY time_ms`)
	if err != nil {
		t.Fatalf("query view: %v", err)
	}
	defer rows.Close()

	checked := 0
	for rows.Next() {
		var recordedAt, viewPeriod string
		if err := rows.Scan(&recordedAt, &viewPeriod); err != nil {
			t.Fatalf("scan: %v", err)
		}
		parsed, err := parseStoredTime(recordedAt)
		if err != nil {
			t.Fatalf("parse %q: %v", recordedAt, err)
		}
		if want := DetectPeriod(parsed); viewPeriod != want {
			t.Errorf("%s (local %02d:%02d): view says %q, DetectPeriod says %q",
				recordedAt, parsed.Hour(), parsed.Minute(), viewPeriod, want)
		}
		checked++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if checked != 24 {
		t.Errorf("checked %d hours, want 24", checked)
	}
}

// TestViewLogicalDateMatchesGo does the same for the 4am-anchored logical
// day that the overnight/daily pairing depends on.
func TestViewLogicalDateMatchesGo(t *testing.T) {
	sqlDB := openTest(t)
	for hour := range 24 {
		recordedAt := time.Date(2026, 8, 16, hour, 30, 0, 0, time.Local)
		if _, err := CreateEntry(sqlDB, recordedAt, 80000, "", time.Now()); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	rows, err := sqlDB.Query(`SELECT recorded_at, logical_date FROM v_entries ORDER BY time_ms`)
	if err != nil {
		t.Fatalf("query view: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var recordedAt, viewDate string
		if err := rows.Scan(&recordedAt, &viewDate); err != nil {
			t.Fatalf("scan: %v", err)
		}
		parsed, err := parseStoredTime(recordedAt)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		// The same rule logicalDate applies in the main package: before 4am
		// belongs to the previous calendar date.
		want := parsed
		if want.Hour() < 4 {
			want = want.AddDate(0, 0, -1)
		}
		if got, wantStr := viewDate, want.Format("2006-01-02"); got != wantStr {
			t.Errorf("%s (local %02d:%02d): view logical_date %s, want %s",
				recordedAt, parsed.Hour(), parsed.Minute(), got, wantStr)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}

func TestViewEntriesExposesKilograms(t *testing.T) {
	sqlDB := openTest(t)
	if _, err := CreateEntry(sqlDB, at(t, "2026-08-16 07:30"), 82437, "", time.Now()); err != nil {
		t.Fatalf("create: %v", err)
	}
	var weightKg float64
	var timeMs int64
	err := sqlDB.QueryRow(`SELECT weight_kg, time_ms FROM v_entries`).Scan(&weightKg, &timeMs)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if weightKg != 82.437 {
		t.Errorf("weight_kg = %v, want 82.437 — the view presents kilograms whatever the column stores", weightKg)
	}
	if want := at(t, "2026-08-16 07:30").UnixMilli(); timeMs != want {
		t.Errorf("time_ms = %d, want %d", timeMs, want)
	}
}

func TestViewEntriesHonoursPeriodOverride(t *testing.T) {
	sqlDB := openTest(t)
	// 09:00 auto-detects morning; the override must win in the view too.
	if _, err := CreateEntry(sqlDB, at(t, "2026-08-16 09:00"), 82000, "evening", time.Now()); err != nil {
		t.Fatalf("create: %v", err)
	}
	var period string
	if err := sqlDB.QueryRow(`SELECT period FROM v_entries`).Scan(&period); err != nil {
		t.Fatalf("query: %v", err)
	}
	if period != "evening" {
		t.Errorf("period = %q, want evening", period)
	}
}

func TestViewEntryDeltas(t *testing.T) {
	sqlDB := openTest(t)
	for _, e := range []struct {
		at string
		g  int64
	}{
		{"2026-08-14 07:00", 84000},
		{"2026-08-14 21:00", 85000},
		{"2026-08-16 07:00", 83000}, // skips a day
		{"2026-08-16 21:00", 84500},
	} {
		if _, err := CreateEntry(sqlDB, at(t, e.at), e.g, "", time.Now()); err != nil {
			t.Fatalf("create %s: %v", e.at, err)
		}
	}

	rows, err := sqlDB.Query(`SELECT period, delta_kg FROM v_entry_deltas WHERE delta_kg IS NOT NULL ORDER BY time_ms`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	got := map[string]float64{}
	for rows.Next() {
		var period string
		var delta float64
		if err := rows.Scan(&period, &delta); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[period] = delta
	}
	// Morning-to-morning across the skipped day, and evening-to-evening.
	if d := got["morning"]; d < -1.0001 || d > -0.9999 {
		t.Errorf("morning delta = %v, want -1", d)
	}
	if d := got["evening"]; d < -0.5001 || d > -0.4999 {
		t.Errorf("evening delta = %v, want -0.5", d)
	}
	// The first reading of each period has no predecessor.
	var nulls int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM v_entry_deltas WHERE delta_kg IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if nulls != 2 {
		t.Errorf("%d rows without a delta, want 2 (first morning and first evening)", nulls)
	}
}

func TestViewGoalsExtendsToNow(t *testing.T) {
	sqlDB := openTest(t)
	if _, err := CreateGoal(sqlDB, 80000, at(t, "2026-01-01 00:00"), time.Now()); err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := CreateGoal(sqlDB, 78000, at(t, "2026-06-01 00:00"), time.Now()); err != nil {
		t.Fatalf("create goal: %v", err)
	}

	rows, err := sqlDB.Query(`SELECT time_ms, goal_kg FROM v_goals ORDER BY time_ms`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	type pt struct {
		ms int64
		kg float64
	}
	var pts []pt
	for rows.Next() {
		var p pt
		if err := rows.Scan(&p.ms, &p.kg); err != nil {
			t.Fatalf("scan: %v", err)
		}
		pts = append(pts, p)
	}
	// Two goal starts plus one trailing point carrying the newest goal.
	if len(pts) != 3 {
		t.Fatalf("got %d points, want 3: %+v", len(pts), pts)
	}
	if pts[len(pts)-1].kg != 78 {
		t.Errorf("trailing point = %v kg, want the newest goal (78)", pts[len(pts)-1].kg)
	}
	if pts[len(pts)-1].ms <= pts[1].ms {
		t.Error("trailing point is not after the newest goal's start; a step line would have nothing to extend along")
	}
}

func TestViewGoalsIsEmptyWithNoGoals(t *testing.T) {
	sqlDB := openTest(t)
	var n int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM v_goals`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d rows, want none", n)
	}
}

func TestViewMarkers(t *testing.T) {
	sqlDB := openTest(t)
	if _, err := CreateMarker(sqlDB, at(t, "2026-08-10 00:00"), "started cutting", time.Now()); err != nil {
		t.Fatalf("create: %v", err)
	}
	var timeMs int64
	var text string
	if err := sqlDB.QueryRow(`SELECT time_ms, text FROM v_markers`).Scan(&timeMs, &text); err != nil {
		t.Fatalf("query: %v", err)
	}
	if text != "started cutting" {
		t.Errorf("text = %q", text)
	}
	if want := at(t, "2026-08-10 00:00").UnixMilli(); timeMs != want {
		t.Errorf("time_ms = %d, want %d", timeMs, want)
	}
}

func TestViewsAreRecreatedOnEveryOpen(t *testing.T) {
	// The definitions live in Go and are refreshed on open, so a database
	// carrying an older definition picks the current one up rather than
	// needing a migration.
	path := filepath.Join(t.TempDir(), "test.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := first.Exec(`DROP VIEW v_entries`); err != nil {
		t.Fatalf("drop view: %v", err)
	}
	if _, err := first.Exec(`CREATE VIEW v_entries AS SELECT 1 AS stale`); err != nil {
		t.Fatalf("install stale view: %v", err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	// The real definition has a weight_kg column; the stale one does not.
	if _, err := second.Query(`SELECT weight_kg FROM v_entries`); err != nil {
		t.Errorf("view was not refreshed on open: %v", err)
	}
}

// TestViewsExposeSecondsAndMilliseconds pins the pair of time columns. The
// seconds one exists because frser-sqlite-datasource overflows on
// milliseconds and plots the series more than a century out; losing it
// would break every dashboard panel while every Go test still passed.
func TestViewsExposeSecondsAndMilliseconds(t *testing.T) {
	sqlDB := openTest(t)
	recordedAt := at(t, "2026-08-16 07:30")
	if _, err := CreateEntry(sqlDB, recordedAt, 82400, "", time.Now()); err != nil {
		t.Fatalf("create entry: %v", err)
	}
	if _, err := CreateGoal(sqlDB, 78000, at(t, "2026-08-01 00:00"), time.Now()); err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := CreateMarker(sqlDB, at(t, "2026-08-10 00:00"), "note", time.Now()); err != nil {
		t.Fatalf("create marker: %v", err)
	}

	for _, view := range []string{"v_entries", "v_entry_deltas", "v_goals", "v_markers"} {
		t.Run(view, func(t *testing.T) {
			rows, err := sqlDB.Query(`SELECT time_ms, time_s FROM ` + view)
			if err != nil {
				t.Fatalf("query %s: %v", view, err)
			}
			defer rows.Close()
			n := 0
			for rows.Next() {
				var ms, s int64
				if err := rows.Scan(&ms, &s); err != nil {
					t.Fatalf("scan: %v", err)
				}
				if ms != s*1000 {
					t.Errorf("time_ms=%d but time_s=%d; they must describe the same instant", ms, s)
				}
				if s < 1_000_000_000 || s > 10_000_000_000 {
					t.Errorf("time_s=%d is not a plausible Unix seconds value — a milliseconds value here is what broke the panels", s)
				}
				n++
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			if n == 0 {
				t.Errorf("%s returned no rows", view)
			}
		})
	}

	// And the entry's seconds value really is the entry's timestamp.
	var s int64
	if err := sqlDB.QueryRow(`SELECT time_s FROM v_entries`).Scan(&s); err != nil {
		t.Fatalf("query: %v", err)
	}
	if s != recordedAt.Unix() {
		t.Errorf("time_s = %d, want %d", s, recordedAt.Unix())
	}
}

// TestViewTrendIsASevenDayMean checks the smoothing the trend line draws.
// It uses RANGE rather than ROWS, so the window is seven days of elapsed
// time — a gap in logging must shorten the window, not reach further back
// for the same number of readings.
func TestViewTrendIsASevenDayMean(t *testing.T) {
	sqlDB := openTest(t)
	// Four consecutive mornings, then one 30 days later whose window
	// contains only itself.
	seed := []struct {
		at string
		g  int64
	}{
		{"2026-08-01 07:00", 80000},
		{"2026-08-02 07:00", 82000},
		{"2026-08-03 07:00", 84000},
		{"2026-08-04 07:00", 86000},
		{"2026-09-10 07:00", 70000},
	}
	for _, s := range seed {
		if _, err := CreateEntry(sqlDB, at(t, s.at), s.g, "", time.Now()); err != nil {
			t.Fatalf("create %s: %v", s.at, err)
		}
	}

	rows, err := sqlDB.Query(`SELECT recorded_at, weight_kg, trend_kg FROM v_entries ORDER BY time_s`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []float64
	for rows.Next() {
		var recordedAt string
		var weight, trend float64
		if err := rows.Scan(&recordedAt, &weight, &trend); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, trend)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d rows, want 5", len(got))
	}

	// Running mean while every reading is inside the trailing week.
	want := []float64{80, 81, 82, 83}
	for i, w := range want {
		if diff := got[i] - w; diff > 0.0001 || diff < -0.0001 {
			t.Errorf("trend[%d] = %v, want %v", i, got[i], w)
		}
	}
	// The isolated reading a month later averages only itself.
	if diff := got[4] - 70; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("trend after a month-long gap = %v, want 70 — the window reached past the gap", got[4])
	}
}

func TestViewTrendIsPerPeriod(t *testing.T) {
	sqlDB := openTest(t)
	// A heavy evening reading must not drag the morning trend down.
	if _, err := CreateEntry(sqlDB, at(t, "2026-08-01 07:00"), 80000, "", time.Now()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := CreateEntry(sqlDB, at(t, "2026-08-01 21:00"), 90000, "", time.Now()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := CreateEntry(sqlDB, at(t, "2026-08-02 07:00"), 82000, "", time.Now()); err != nil {
		t.Fatalf("create: %v", err)
	}

	var trend, trendAll float64
	err := sqlDB.QueryRow(
		`SELECT trend_kg, trend_all_kg FROM v_entries WHERE period = 'morning' ORDER BY time_s DESC LIMIT 1`,
	).Scan(&trend, &trendAll)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if diff := trend - 81; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("morning trend = %v, want 81 (the two mornings only)", trend)
	}
	if diff := trendAll - 84; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("all-entries trend = %v, want 84 (every reading)", trendAll)
	}
}
