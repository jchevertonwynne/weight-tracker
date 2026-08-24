package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Entry struct {
	ID         int64
	RecordedAt time.Time
	WeightG    int64
	// PeriodOverride is "" (auto-detect from RecordedAt via DetectPeriod),
	// "morning", or "evening" — set when the user manually overrides the
	// period detected for a weigh-in logged close to the morning/evening
	// boundary.
	PeriodOverride string
	CreatedAt      time.Time
}

// nullIfEmpty converts "" to a SQL NULL — period_override is a nullable
// column, and an empty string means "no override", not the literal string
// "".
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func CreateEntry(ctx context.Context, sqlDB *sql.DB, recordedAt time.Time, weightG int64, periodOverride string, createdAt time.Time) (int64, error) {
	return withSpan(ctx, "CreateEntry", func(ctx context.Context) (int64, error) {
		res, err := sqlDB.ExecContext(ctx,
			`INSERT INTO entries (recorded_at, weight_g, period_override, created_at) VALUES (?, ?, ?, ?)`,
			recordedAt.UTC().Format(time.RFC3339), weightG, nullIfEmpty(periodOverride), createdAt.UTC().Format(time.RFC3339),
		)
		if err != nil {
			return 0, fmt.Errorf("insert entry: %w", err)
		}
		return res.LastInsertId()
	})
}

func UpdateEntry(ctx context.Context, sqlDB *sql.DB, id int64, recordedAt time.Time, weightG int64, periodOverride string) error {
	return withSpanErr(ctx, "UpdateEntry", func(ctx context.Context) error {
		_, err := sqlDB.ExecContext(ctx,
			`UPDATE entries SET recorded_at = ?, weight_g = ?, period_override = ? WHERE id = ?`,
			recordedAt.UTC().Format(time.RFC3339), weightG, nullIfEmpty(periodOverride), id,
		)
		if err != nil {
			return fmt.Errorf("update entry %d: %w", id, err)
		}
		return nil
	})
}

func DeleteEntry(ctx context.Context, sqlDB *sql.DB, id int64) error {
	return deleteByID(ctx, sqlDB, "entries", id)
}

func GetEntry(ctx context.Context, sqlDB *sql.DB, id int64) (Entry, error) {
	return withSpan(ctx, "GetEntry", func(ctx context.Context) (Entry, error) {
		var e Entry
		var recordedAt, createdAt string
		err := sqlDB.QueryRowContext(ctx,
			`SELECT id, recorded_at, weight_g, COALESCE(period_override, ''), created_at FROM entries WHERE id = ?`, id,
		).Scan(&e.ID, &recordedAt, &e.WeightG, &e.PeriodOverride, &createdAt)
		if err != nil {
			return Entry{}, fmt.Errorf("get entry %d: %w", id, err)
		}
		if e.RecordedAt, err = parseStoredTime(recordedAt); err != nil {
			return Entry{}, fmt.Errorf("parse recorded_at for entry %d: %w", id, err)
		}
		if e.CreatedAt, err = parseStoredTime(createdAt); err != nil {
			return Entry{}, fmt.Errorf("parse created_at for entry %d: %w", id, err)
		}
		return e, nil
	})
}

// ListEntries returns every entry newest-first.
func ListEntries(ctx context.Context, sqlDB *sql.DB) ([]Entry, error) {
	return withSpan(ctx, "ListEntries", func(ctx context.Context) ([]Entry, error) {
		rows, err := sqlDB.QueryContext(ctx,
			`SELECT id, recorded_at, weight_g, COALESCE(period_override, ''), created_at FROM entries ORDER BY recorded_at DESC`,
		)
		if err != nil {
			return nil, fmt.Errorf("list entries: %w", err)
		}
		defer rows.Close()

		var entries []Entry
		for rows.Next() {
			var e Entry
			var recordedAt, createdAt string
			if err := rows.Scan(&e.ID, &recordedAt, &e.WeightG, &e.PeriodOverride, &createdAt); err != nil {
				return nil, fmt.Errorf("scan entry: %w", err)
			}
			if e.RecordedAt, err = parseStoredTime(recordedAt); err != nil {
				return nil, fmt.Errorf("parse recorded_at: %w", err)
			}
			if e.CreatedAt, err = parseStoredTime(createdAt); err != nil {
				return nil, fmt.Errorf("parse created_at: %w", err)
			}
			entries = append(entries, e)
		}
		return entries, rows.Err()
	})
}

// NewEntry is a not-yet-persisted entry, used for bulk imports.
type NewEntry struct {
	RecordedAt time.Time
	WeightG    int64
}

// ExistingRecordedAtSet returns the set of recorded_at values (as stored,
// i.e. RFC3339-formatted) currently present in the entries table, for fast
// in-memory de-duplication during bulk imports.
func ExistingRecordedAtSet(ctx context.Context, sqlDB *sql.DB) (map[string]struct{}, error) {
	return withSpan(ctx, "ExistingRecordedAtSet", func(ctx context.Context) (map[string]struct{}, error) {
		rows, err := sqlDB.QueryContext(ctx, `SELECT recorded_at FROM entries`)
		if err != nil {
			return nil, fmt.Errorf("list recorded_at: %w", err)
		}
		defer rows.Close()

		set := make(map[string]struct{})
		for rows.Next() {
			var recordedAt string
			if err := rows.Scan(&recordedAt); err != nil {
				return nil, fmt.Errorf("scan recorded_at: %w", err)
			}
			set[recordedAt] = struct{}{}
		}
		return set, rows.Err()
	})
}

// BulkCreateEntries inserts all given rows in a single transaction, skipping
// any whose recorded_at (RFC3339, exact match) is already present in
// existing — which is also updated in place, so duplicates within rows
// itself are skipped too. Returns the count actually inserted.
func BulkCreateEntries(ctx context.Context, sqlDB *sql.DB, rows []NewEntry, existing map[string]struct{}, createdAt time.Time) (int, error) {
	return withSpan(ctx, "BulkCreateEntries", func(ctx context.Context) (int, error) {
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("begin import tx: %w", err)
		}
		defer tx.Rollback()

		stmt, err := tx.PrepareContext(ctx, `INSERT INTO entries (recorded_at, weight_g, created_at) VALUES (?, ?, ?)`)
		if err != nil {
			return 0, fmt.Errorf("prepare insert: %w", err)
		}
		defer stmt.Close()

		createdAtStr := createdAt.UTC().Format(time.RFC3339)
		inserted := 0
		for _, row := range rows {
			key := row.RecordedAt.UTC().Format(time.RFC3339)
			if _, dup := existing[key]; dup {
				continue
			}
			if _, err := stmt.ExecContext(ctx, key, row.WeightG, createdAtStr); err != nil {
				return inserted, fmt.Errorf("insert row at %s: %w", key, err)
			}
			existing[key] = struct{}{}
			inserted++
		}
		return inserted, tx.Commit()
	})
}
