package main

import (
	"fmt"
	"net/http"
	"time"

	"weight-tracker/internal/db"
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

func buildWeeklySummary(entries []db.Entry, now time.Time) WeeklySummary {
	thisStart := now.AddDate(0, 0, -7)
	lastStart := now.AddDate(0, 0, -14)

	var thisWeek, lastWeek []float64
	for _, e := range entries {
		if db.DetectPeriod(e.RecordedAt) != "morning" {
			continue
		}
		switch {
		case !e.RecordedAt.Before(thisStart) && !e.RecordedAt.After(now):
			thisWeek = append(thisWeek, e.WeightKg)
		case !e.RecordedAt.Before(lastStart) && e.RecordedAt.Before(thisStart):
			lastWeek = append(lastWeek, e.WeightKg)
		}
	}

	if len(thisWeek) == 0 {
		return WeeklySummary{Empty: "Not enough morning weigh-ins this week yet for a trend comparison."}
	}
	summary := WeeklySummary{ThisWeekAvg: fmt.Sprintf("%.1f kg", mean(thisWeek))}
	if len(lastWeek) == 0 {
		return summary
	}
	thisAvg, lastAvg := mean(thisWeek), mean(lastWeek)
	summary.HasComparison = true
	summary.LastWeekAvg = fmt.Sprintf("%.1f kg", lastAvg)
	summary.Delta = fmt.Sprintf("%+.1f kg", thisAvg-lastAvg)
	summary.DeltaIsLoss = thisAvg < lastAvg
	return summary
}

func mean(vals []float64) float64 {
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func (s *server) handleSummary(w http.ResponseWriter, _ *http.Request) {
	entries, err := db.ListEntries(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := buildWeeklySummary(entries, time.Now())
	if err := tmpl.ExecuteTemplate(w, "summary", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
