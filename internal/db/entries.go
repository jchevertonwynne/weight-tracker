package db

import (
	"database/sql"
	"fmt"
	"time"
)

type Entry struct {
	ID         int64
	RecordedAt time.Time
	WeightKg   float64
	CreatedAt  time.Time
}

func CreateEntry(sqlDB *sql.DB, recordedAt time.Time, weightKg float64, createdAt time.Time) (int64, error) {
	res, err := sqlDB.Exec(
		`INSERT INTO entries (recorded_at, weight_kg, created_at) VALUES (?, ?, ?)`,
		recordedAt.UTC().Format(time.RFC3339), weightKg, createdAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("insert entry: %w", err)
	}
	return res.LastInsertId()
}

func UpdateEntry(sqlDB *sql.DB, id int64, recordedAt time.Time, weightKg float64) error {
	_, err := sqlDB.Exec(
		`UPDATE entries SET recorded_at = ?, weight_kg = ? WHERE id = ?`,
		recordedAt.UTC().Format(time.RFC3339), weightKg, id,
	)
	if err != nil {
		return fmt.Errorf("update entry %d: %w", id, err)
	}
	return nil
}

func DeleteEntry(sqlDB *sql.DB, id int64) error {
	if _, err := sqlDB.Exec(`DELETE FROM entries WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete entry %d: %w", id, err)
	}
	return nil
}

func GetEntry(sqlDB *sql.DB, id int64) (Entry, error) {
	var e Entry
	var recordedAt, createdAt string
	err := sqlDB.QueryRow(
		`SELECT id, recorded_at, weight_kg, created_at FROM entries WHERE id = ?`, id,
	).Scan(&e.ID, &recordedAt, &e.WeightKg, &createdAt)
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
}

// ListEntries returns every entry newest-first.
func ListEntries(sqlDB *sql.DB) ([]Entry, error) {
	rows, err := sqlDB.Query(
		`SELECT id, recorded_at, weight_kg, created_at FROM entries ORDER BY recorded_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var recordedAt, createdAt string
		if err := rows.Scan(&e.ID, &recordedAt, &e.WeightKg, &createdAt); err != nil {
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
}

// NewEntry is a not-yet-persisted entry, used for bulk imports.
type NewEntry struct {
	RecordedAt time.Time
	WeightKg   float64
}

// ExistingRecordedAtSet returns the set of recorded_at values (as stored,
// i.e. RFC3339-formatted) currently present in the entries table, for fast
// in-memory de-duplication during bulk imports.
func ExistingRecordedAtSet(sqlDB *sql.DB) (map[string]struct{}, error) {
	rows, err := sqlDB.Query(`SELECT recorded_at FROM entries`)
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
}

// BulkCreateEntries inserts all given rows in a single transaction, skipping
// any whose recorded_at (RFC3339, exact match) is already present in
// existing — which is also updated in place, so duplicates within rows
// itself are skipped too. Returns the count actually inserted.
func BulkCreateEntries(sqlDB *sql.DB, rows []NewEntry, existing map[string]struct{}, createdAt time.Time) (int, error) {
	tx, err := sqlDB.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin import tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO entries (recorded_at, weight_kg, created_at) VALUES (?, ?, ?)`)
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
		if _, err := stmt.Exec(key, row.WeightKg, createdAtStr); err != nil {
			return inserted, fmt.Errorf("insert row at %s: %w", key, err)
		}
		existing[key] = struct{}{}
		inserted++
	}
	return inserted, tx.Commit()
}
