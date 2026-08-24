package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Goal struct {
	ID            int64
	WeightG       int64
	EffectiveFrom time.Time
	CreatedAt     time.Time
}

func CreateGoal(ctx context.Context, sqlDB *sql.DB, weightG int64, effectiveFrom time.Time, createdAt time.Time) (int64, error) {
	return withSpan(ctx, "CreateGoal", func(ctx context.Context) (int64, error) {
		res, err := sqlDB.ExecContext(ctx,
			`INSERT INTO goals (weight_g, effective_from, created_at) VALUES (?, ?, ?)`,
			weightG, effectiveFrom.UTC().Format(time.RFC3339), createdAt.UTC().Format(time.RFC3339),
		)
		if err != nil {
			return 0, fmt.Errorf("insert goal: %w", err)
		}
		return res.LastInsertId()
	})
}

func UpdateGoal(ctx context.Context, sqlDB *sql.DB, id int64, weightG int64, effectiveFrom time.Time) error {
	return withSpanErr(ctx, "UpdateGoal", func(ctx context.Context) error {
		_, err := sqlDB.ExecContext(ctx,
			`UPDATE goals SET weight_g = ?, effective_from = ? WHERE id = ?`,
			weightG, effectiveFrom.UTC().Format(time.RFC3339), id,
		)
		if err != nil {
			return fmt.Errorf("update goal %d: %w", id, err)
		}
		return nil
	})
}

func DeleteGoal(ctx context.Context, sqlDB *sql.DB, id int64) error {
	return deleteByID(ctx, sqlDB, "goals", id)
}

func GetGoal(ctx context.Context, sqlDB *sql.DB, id int64) (Goal, error) {
	return withSpan(ctx, "GetGoal", func(ctx context.Context) (Goal, error) {
		var g Goal
		var effectiveFrom, createdAt string
		err := sqlDB.QueryRowContext(ctx,
			`SELECT id, weight_g, effective_from, created_at FROM goals WHERE id = ?`, id,
		).Scan(&g.ID, &g.WeightG, &effectiveFrom, &createdAt)
		if err != nil {
			return Goal{}, fmt.Errorf("get goal %d: %w", id, err)
		}
		if g.EffectiveFrom, err = parseStoredTime(effectiveFrom); err != nil {
			return Goal{}, fmt.Errorf("parse effective_from for goal %d: %w", id, err)
		}
		if g.CreatedAt, err = parseStoredTime(createdAt); err != nil {
			return Goal{}, fmt.Errorf("parse created_at for goal %d: %w", id, err)
		}
		return g, nil
	})
}

// ListGoals returns every goal newest-first.
func ListGoals(ctx context.Context, sqlDB *sql.DB) ([]Goal, error) {
	return withSpan(ctx, "ListGoals", func(ctx context.Context) ([]Goal, error) {
		rows, err := sqlDB.QueryContext(ctx,
			`SELECT id, weight_g, effective_from, created_at FROM goals ORDER BY effective_from DESC`,
		)
		if err != nil {
			return nil, fmt.Errorf("list goals: %w", err)
		}
		defer rows.Close()

		var goals []Goal
		for rows.Next() {
			var g Goal
			var effectiveFrom, createdAt string
			if err := rows.Scan(&g.ID, &g.WeightG, &effectiveFrom, &createdAt); err != nil {
				return nil, fmt.Errorf("scan goal: %w", err)
			}
			if g.EffectiveFrom, err = parseStoredTime(effectiveFrom); err != nil {
				return nil, fmt.Errorf("parse effective_from: %w", err)
			}
			if g.CreatedAt, err = parseStoredTime(createdAt); err != nil {
				return nil, fmt.Errorf("parse created_at: %w", err)
			}
			goals = append(goals, g)
		}
		return goals, rows.Err()
	})
}
