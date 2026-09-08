// Package summary compares the trailing 7-day average morning weight to the
// preceding 7-day average.
package summary

import (
	"fmt"
	"math"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/goals"
	"weight-tracker/internal/weight"
)

// WeeklySummary compares the trailing 7-day average morning weight to the
// preceding 7-day average, using morning entries only (the fasted reading
// is less noisy than an evening one, and is what the rest of the app already
// treats as the reference point for overnight deltas).
type WeeklySummary struct {
	Empty         string // set (with everything else zero) if no data this week
	ThisWeekAvg   string
	HasComparison bool // false if last week has zero qualifying entries
	LastWeekAvg   string
	Delta         string
	DeltaIsLoss   bool
	Goal          GoalProgress
}

// GoalProgress projects this week's average onward to the active goal
// weight (see goals.Current), using the same week-over-week rate Delta is
// built from. It is the zero value (HasGoal false) if there is no active
// goal, or if there is no data this week to project from at all.
type GoalProgress struct {
	HasGoal      bool
	WeightStr    string // goal weight, kg
	Reached      bool
	RemainingStr string // e.g. "2.2 kg to go"
	ETALabel     string // e.g. "Sep 30, 2026"; empty if not computable
	Stalled      string // explanation shown in place of ETALabel
}

// reachedEpsilonKg is how close this week's average has to be to the goal
// weight to call it reached, rather than reporting a razor-thin "0.0 kg to
// go" as still outstanding.
const reachedEpsilonKg = 0.05

// minRateKgPerWeek is the smallest week-over-week rate treated as an actual
// trend. Below it, dividing the remaining distance by the rate would
// project an ETA years or decades out — an appearance of precision the
// data doesn't support — so it's reported as stalled instead.
const minRateKgPerWeek = 0.01

// Build computes the weekly comparison from entries as of now, plus (if
// allGoals has an active goal — see goals.Current) a projection toward it.
func Build(entries []db.Entry, allGoals []db.Goal, now time.Time) WeeklySummary {
	thisStart := now.AddDate(0, 0, -7)
	lastStart := now.AddDate(0, 0, -14)

	var thisWeek, lastWeek []int64
	for _, e := range entries {
		if weight.EntryPeriod(e) != "morning" {
			continue
		}
		switch {
		case !e.RecordedAt.Before(thisStart) && !e.RecordedAt.After(now):
			thisWeek = append(thisWeek, e.WeightG)
		case !e.RecordedAt.Before(lastStart) && e.RecordedAt.Before(thisStart):
			lastWeek = append(lastWeek, e.WeightG)
		}
	}

	if len(thisWeek) == 0 {
		return WeeklySummary{Empty: "Not enough morning weigh-ins this week yet for a trend comparison."}
	}
	thisAvg := meanKg(thisWeek)
	result := WeeklySummary{ThisWeekAvg: fmt.Sprintf("%.1f kg", thisAvg)}

	activeGoal, hasGoal := goals.Current(allGoals, now)
	var lastAvg float64
	haveLastAvg := len(lastWeek) > 0
	if haveLastAvg {
		lastAvg = meanKg(lastWeek)
		result.HasComparison = true
		result.LastWeekAvg = fmt.Sprintf("%.1f kg", lastAvg)
		result.Delta = fmt.Sprintf("%+.1f kg", thisAvg-lastAvg)
		result.DeltaIsLoss = thisAvg < lastAvg
	}
	if hasGoal {
		result.Goal = buildGoalProgress(thisAvg, activeGoal, haveLastAvg, lastAvg, now)
	}
	return result
}

// buildGoalProgress projects thisAvg onward to goal using the week-over-
// week rate (thisAvg - lastAvg, the same number Delta is built from).
func buildGoalProgress(thisAvg float64, goal db.Goal, haveLastAvg bool, lastAvg float64, now time.Time) GoalProgress {
	goalKg := db.GramsToKg(goal.WeightG)
	progress := GoalProgress{HasGoal: true, WeightStr: weight.FormatKg(goal.WeightG)}

	remaining := thisAvg - goalKg
	if math.Abs(remaining) <= reachedEpsilonKg {
		progress.Reached = true
		return progress
	}
	progress.RemainingStr = fmt.Sprintf("%.1f kg to go", math.Abs(remaining))

	if !haveLastAvg {
		progress.Stalled = "Not enough data yet for a goal estimate."
		return progress
	}
	// remaining > 0 means still above goal, so needs a falling rate; remaining
	// < 0 means still below goal, so needs a rising rate. Either way,
	// progressPerWeek is positive exactly when movement is toward the goal.
	rate := thisAvg - lastAvg
	progressPerWeek := -rate
	if remaining < 0 {
		progressPerWeek = rate
	}
	if progressPerWeek <= minRateKgPerWeek {
		progress.Stalled = "Not currently trending toward your goal."
		return progress
	}
	weeks := math.Abs(remaining) / progressPerWeek
	progress.ETALabel = now.AddDate(0, 0, int(math.Round(weeks*7))).Format("Jan 2, 2006")
	return progress
}

// meanKg averages gram values and returns kilograms. The sum is taken in
// integer grams so it is exact however many weigh-ins it covers; only the
// final division is floating point.
func meanKg(grams []int64) float64 {
	var sum int64
	for _, g := range grams {
		sum += g
	}
	return db.GramsToKg(sum) / float64(len(grams))
}
