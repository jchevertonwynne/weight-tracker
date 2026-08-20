// Package testsupport holds fixture builders shared by every other
// package's tests — a single place for them rather than duplicating the
// same db.Entry/db.Goal/db.Marker construction logic in each package's own
// _test.go file.
package testsupport

import (
	"testing"
	"time"

	"weight-tracker/internal/db"
)

// At parses a "2006-01-02 15:04" wall-clock string in time.Local, matching
// how the app itself interprets the date/time form inputs. Tests therefore
// pass in any timezone the CI runner happens to use.
func At(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		t.Fatalf("parse test time %q: %v", s, err)
	}
	return parsed
}

// Entry builds a db.Entry from a kilogram figure, since that is what test
// cases read naturally; storage is grams, so it converts on the way in
// exactly as the handlers do.
func Entry(id int64, recordedAt time.Time, weightKg float64, override string) db.Entry {
	return db.Entry{ID: id, RecordedAt: recordedAt, WeightG: db.KgToGrams(weightKg), PeriodOverride: override}
}

// Goal builds a db.Goal from a kilogram figure and an "effective from" date.
func Goal(id int64, weightKg float64, effectiveFrom string, t *testing.T) db.Goal {
	t.Helper()
	return db.Goal{ID: id, WeightG: db.KgToGrams(weightKg), EffectiveFrom: At(t, effectiveFrom+" 00:00")}
}

// Marker builds a db.Marker from a date string and note.
func Marker(id int64, date string, note string, t *testing.T) db.Marker {
	t.Helper()
	return db.Marker{ID: id, Date: At(t, date+" 00:00"), Note: note}
}

// Epsilon is the tolerance NearlyEqual uses for floating-point comparisons.
const Epsilon = 1e-9

// NearlyEqual reports whether a and b are within Epsilon of each other —
// tests compare computed kilogram averages, which accumulate ordinary
// floating-point error, not exact values.
func NearlyEqual(a, b float64) bool {
	diff := a - b
	return diff < Epsilon && diff > -Epsilon
}
