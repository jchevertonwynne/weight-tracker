package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Marker is a date-based annotation shown as a vertical reference line on
// the chart, giving context for why the weight moved (e.g. "started new
// diet", "on holiday").
type Marker struct {
	ID        int64
	Date      time.Time
	Note      string
	CreatedAt time.Time
}

func CreateMarker(ctx context.Context, sqlDB *sql.DB, date time.Time, note string, createdAt time.Time) (int64, error) {
	return withSpan(ctx, "CreateMarker", func(ctx context.Context) (int64, error) {
		res, err := sqlDB.ExecContext(ctx,
			`INSERT INTO markers (date, note, created_at) VALUES (?, ?, ?)`,
			date.UTC().Format(time.RFC3339), note, createdAt.UTC().Format(time.RFC3339),
		)
		if err != nil {
			return 0, fmt.Errorf("insert marker: %w", err)
		}
		return res.LastInsertId()
	})
}

func UpdateMarker(ctx context.Context, sqlDB *sql.DB, id int64, date time.Time, note string) error {
	return withSpanErr(ctx, "UpdateMarker", func(ctx context.Context) error {
		_, err := sqlDB.ExecContext(ctx,
			`UPDATE markers SET date = ?, note = ? WHERE id = ?`,
			date.UTC().Format(time.RFC3339), note, id,
		)
		if err != nil {
			return fmt.Errorf("update marker %d: %w", id, err)
		}
		return nil
	})
}

func DeleteMarker(ctx context.Context, sqlDB *sql.DB, id int64) error {
	return deleteByID(ctx, sqlDB, "markers", id)
}

func GetMarker(ctx context.Context, sqlDB *sql.DB, id int64) (Marker, error) {
	return withSpan(ctx, "GetMarker", func(ctx context.Context) (Marker, error) {
		var m Marker
		var date, createdAt string
		err := sqlDB.QueryRowContext(ctx,
			`SELECT id, date, note, created_at FROM markers WHERE id = ?`, id,
		).Scan(&m.ID, &date, &m.Note, &createdAt)
		if err != nil {
			return Marker{}, fmt.Errorf("get marker %d: %w", id, err)
		}
		if m.Date, err = parseStoredTime(date); err != nil {
			return Marker{}, fmt.Errorf("parse date for marker %d: %w", id, err)
		}
		if m.CreatedAt, err = parseStoredTime(createdAt); err != nil {
			return Marker{}, fmt.Errorf("parse created_at for marker %d: %w", id, err)
		}
		return m, nil
	})
}

// ListMarkers returns every marker newest-first.
func ListMarkers(ctx context.Context, sqlDB *sql.DB) ([]Marker, error) {
	return withSpan(ctx, "ListMarkers", func(ctx context.Context) ([]Marker, error) {
		rows, err := sqlDB.QueryContext(ctx,
			`SELECT id, date, note, created_at FROM markers ORDER BY date DESC`,
		)
		if err != nil {
			return nil, fmt.Errorf("list markers: %w", err)
		}
		defer rows.Close()

		var markers []Marker
		for rows.Next() {
			var m Marker
			var date, createdAt string
			if err := rows.Scan(&m.ID, &date, &m.Note, &createdAt); err != nil {
				return nil, fmt.Errorf("scan marker: %w", err)
			}
			if m.Date, err = parseStoredTime(date); err != nil {
				return nil, fmt.Errorf("parse date: %w", err)
			}
			if m.CreatedAt, err = parseStoredTime(createdAt); err != nil {
				return nil, fmt.Errorf("parse created_at: %w", err)
			}
			markers = append(markers, m)
		}
		return markers, rows.Err()
	})
}
