// Package summary compares the trailing 7-day average morning weight to the
// preceding 7-day average.
package summary

import (
	"fmt"
	"time"

	"weight-tracker/internal/db"
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
}

// Build computes the weekly comparison from entries as of now.
func Build(entries []db.Entry, now time.Time) WeeklySummary {
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
	result := WeeklySummary{ThisWeekAvg: fmt.Sprintf("%.1f kg", meanKg(thisWeek))}
	if len(lastWeek) == 0 {
		return result
	}
	thisAvg, lastAvg := meanKg(thisWeek), meanKg(lastWeek)
	result.HasComparison = true
	result.LastWeekAvg = fmt.Sprintf("%.1f kg", lastAvg)
	result.Delta = fmt.Sprintf("%+.1f kg", thisAvg-lastAvg)
	result.DeltaIsLoss = thisAvg < lastAvg
	return result
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
