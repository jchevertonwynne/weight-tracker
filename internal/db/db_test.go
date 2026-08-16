package db

import (
	"database/sql"
	"errors"
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
	if _, err := CreateEntry(first, at(t, "2026-08-16 07:30"), 82.4, "", time.Now()); err != nil {
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

	id, err := CreateEntry(sqlDB, recordedAt, 82.4, "", time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := GetEntry(sqlDB, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WeightKg != 82.4 {
		t.Errorf("WeightKg = %v, want 82.4", got.WeightKg)
	}
	// Timestamps are stored as UTC but must come back as the same instant,
	// rendered in local time for the period/same-day comparisons.
	if !got.RecordedAt.Equal(recordedAt) {
		t.Errorf("RecordedAt = %v, want %v", got.RecordedAt, recordedAt)
	}
	if got.PeriodOverride != "" {
		t.Errorf("PeriodOverride = %q, want empty (stored as NULL)", got.PeriodOverride)
	}

	if err := UpdateEntry(sqlDB, id, recordedAt.Add(time.Hour), 81.9, "evening"); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = GetEntry(sqlDB, id)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.WeightKg != 81.9 || got.PeriodOverride != "evening" {
		t.Errorf("after update: %v kg, override %q; want 81.9 / evening", got.WeightKg, got.PeriodOverride)
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

	id, err := CreateGoal(sqlDB, 78, effectiveFrom, time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := GetGoal(sqlDB, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WeightKg != 78 || !got.EffectiveFrom.Equal(effectiveFrom) {
		t.Errorf("got %v kg from %v, want 78 from %v", got.WeightKg, got.EffectiveFrom, effectiveFrom)
	}

	if err := UpdateGoal(sqlDB, id, 76, effectiveFrom); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ = GetGoal(sqlDB, id); got.WeightKg != 76 {
		t.Errorf("after update: %v kg, want 76", got.WeightKg)
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
	id, err := CreateEntry(sqlDB, at(t, "2026-08-16 07:30"), 82.4, "", time.Now())
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
	if _, err := CreateEntry(sqlDB, recordedAt, 82.4, "", time.Now()); err != nil {
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
			{RecordedAt: at(t, "2026-08-14 07:00"), WeightKg: 84},
			{RecordedAt: at(t, "2026-08-15 07:00"), WeightKg: 83},
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
		if _, err := CreateEntry(sqlDB, existingAt, 84, "", time.Now()); err != nil {
			t.Fatalf("seed: %v", err)
		}
		existing, err := ExistingRecordedAtSet(sqlDB)
		if err != nil {
			t.Fatalf("set: %v", err)
		}
		rows := []NewEntry{
			{RecordedAt: existingAt, WeightKg: 84},
			{RecordedAt: at(t, "2026-08-15 07:00"), WeightKg: 83},
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
			{RecordedAt: sameTime, WeightKg: 84},
			{RecordedAt: sameTime, WeightKg: 84.5},
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
	if _, err := CreateEntry(sqlDB, at(t, "2026-08-16 07:30"), 82.4, "", time.Now()); err != nil {
		t.Fatalf("create entry: %v", err)
	}
	if _, err := CreateGoal(sqlDB, 78, at(t, "2026-08-01 00:00"), time.Now()); err != nil {
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
	if _, err := CreateEntry(sqlDB, at(t, "2026-08-17 07:30"), 82.0, "", time.Now()); err != nil {
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
	if entries[0].WeightKg != 82.4 {
		t.Errorf("WeightKg = %v, want 82.4", entries[0].WeightKg)
	}
	if entries[0].PeriodOverride != "" {
		t.Errorf("PeriodOverride = %q, want empty for a legacy row", entries[0].PeriodOverride)
	}

	// The new column must be writable on the migrated table.
	if err := UpdateEntry(migrated, entries[0].ID, entries[0].RecordedAt, 82.4, "evening"); err != nil {
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
	id, err := CreateEntry(sqlDB, recordedAt, 82.4, "", time.Now())
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
