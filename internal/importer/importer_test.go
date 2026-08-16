package importer

import (
	"strings"
	"testing"
	"time"
)

const epsilon = 1e-6

func nearlyEqual(a, b float64) bool {
	diff := a - b
	return diff < epsilon && diff > -epsilon
}

// parseOK runs Parse and fails the test if it returns an error.
func parseOK(t *testing.T, csv, unit string) Result {
	t.Helper()
	got, err := Parse(strings.NewReader(csv), unit)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return got
}

func TestParseRejectsInvalidUnit(t *testing.T) {
	for _, unit := range []string{"", "stone", "KG", "pounds"} {
		if _, err := Parse(strings.NewReader("weight\n80\n"), unit); err == nil {
			t.Errorf("unit %q was accepted, want an error", unit)
		}
	}
}

// The Google Health (Takeout) format is the one confirmed against real data:
// a "weight grams" header, UTC timestamps, integer grams.
func TestParseGoogleHealthExport(t *testing.T) {
	csv := "timestamp,weight grams,data source\n" +
		"2026-08-14T06:00:00Z,81500,Some Scale\n" +
		"2026-08-15T06:30:00Z,81200,Some Scale\n"

	// The form's unit is deliberately wrong; the header must win.
	got := parseOK(t, csv, "lb")
	if len(got.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(got.Rows))
	}
	if len(got.Skipped) != 0 {
		t.Errorf("skipped %v, want none", got.Skipped)
	}
	if !nearlyEqual(got.Rows[0].WeightKg, 81.5) {
		t.Errorf("weight = %v kg, want 81.5 — the 'weight grams' header should override the form's unit", got.Rows[0].WeightKg)
	}
	// A UTC timestamp must land on the same instant, expressed locally.
	wantInstant := time.Date(2026, 8, 14, 6, 0, 0, 0, time.UTC)
	if !got.Rows[0].RecordedAt.Equal(wantInstant) {
		t.Errorf("RecordedAt = %v, want the same instant as %v", got.Rows[0].RecordedAt, wantInstant)
	}
	if got.Rows[0].RecordedAt.Location() != time.Local {
		t.Errorf("RecordedAt location = %v, want time.Local", got.Rows[0].RecordedAt.Location())
	}
}

func TestParseAppsOwnExport(t *testing.T) {
	csv := "recorded_at,weight_kg\n2026-08-14T07:00:00+01:00,81.5\n"
	got := parseOK(t, csv, "lb") // header declares kg, so the form's unit is ignored
	if len(got.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(got.Rows))
	}
	if !nearlyEqual(got.Rows[0].WeightKg, 81.5) {
		t.Errorf("weight = %v, want 81.5", got.Rows[0].WeightKg)
	}
}

func TestParseAmbiguousWeightColumnUsesTheFormUnit(t *testing.T) {
	csv := "recorded_at,weight\n2026-08-14T07:00:00Z,180\n"

	asKg := parseOK(t, csv, "kg")
	if !nearlyEqual(asKg.Rows[0].WeightKg, 180) {
		t.Errorf("as kg = %v, want 180", asKg.Rows[0].WeightKg)
	}

	asLb := parseOK(t, csv, "lb")
	if !nearlyEqual(asLb.Rows[0].WeightKg, 180*lbToKg) {
		t.Errorf("as lb = %v, want %v", asLb.Rows[0].WeightKg, 180*lbToKg)
	}

	asG := parseOK(t, csv, "g")
	if !nearlyEqual(asG.Rows[0].WeightKg, 0.18) {
		t.Errorf("as grams = %v, want 0.18", asG.Rows[0].WeightKg)
	}
}

func TestParseWeightColumnPriority(t *testing.T) {
	// weight_kg is checked before the ambiguous bare "weight" column.
	csv := "recorded_at,weight,weight_kg\n2026-08-14T07:00:00Z,999,81.5\n"
	got := parseOK(t, csv, "lb")
	if !nearlyEqual(got.Rows[0].WeightKg, 81.5) {
		t.Errorf("weight = %v, want 81.5 from the self-describing column", got.Rows[0].WeightKg)
	}
}

func TestParseSplitDateAndTimeColumns(t *testing.T) {
	// The Fitbit-style layout: separate Date and Time columns.
	csv := "Date,Time,Weight\n08/14/26,07:00:00,180\n"
	got := parseOK(t, csv, "lb")
	if len(got.Rows) != 1 {
		t.Fatalf("got %d rows (skipped %v), want 1", len(got.Rows), got.Skipped)
	}
	if !nearlyEqual(got.Rows[0].WeightKg, 180*lbToKg) {
		t.Errorf("weight = %v, want %v", got.Rows[0].WeightKg, 180*lbToKg)
	}
	if got.Rows[0].RecordedAt.Year() != 2026 || got.Rows[0].RecordedAt.Month() != time.August {
		t.Errorf("RecordedAt = %v, want August 2026", got.Rows[0].RecordedAt)
	}
	if got.Rows[0].RecordedAt.Day() != 14 {
		t.Errorf("day = %d, want 14 (MM/DD/YY ordering)", got.Rows[0].RecordedAt.Day())
	}
}

func TestParseSplitDateTimeLayoutVariants(t *testing.T) {
	tests := []struct {
		name string
		date string
		time string
	}{
		{"two-digit year", "08/14/26", "07:00:00"},
		{"four-digit year", "08/14/2026", "07:00:00"},
		{"ISO date", "2026-08-14", "07:00:00"},
		{"no seconds", "2026-08-14", "07:00"},
		{"12-hour clock", "2026-08-14", "7:00:00 AM"},
		{"12-hour clock without seconds", "2026-08-14", "7:00 AM"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			csv := "Date,Time,Weight\n" + tc.date + "," + tc.time + ",80\n"
			got := parseOK(t, csv, "kg")
			if len(got.Rows) != 1 {
				t.Fatalf("got %d rows (skipped %v), want 1", len(got.Rows), got.Skipped)
			}
			ts := got.Rows[0].RecordedAt
			if ts.Year() != 2026 || ts.Month() != time.August || ts.Day() != 14 || ts.Hour() != 7 {
				t.Errorf("RecordedAt = %v, want 2026-08-14 07:00", ts)
			}
		})
	}
}

func TestParseCombinedDatetimeLayoutVariants(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"RFC3339 with zone", "2026-08-14T07:00:00+01:00"},
		{"ISO without zone", "2026-08-14T07:00:00"},
		{"space separated", "2026-08-14 07:00:00"},
		{"US two-digit year", "08/14/26 07:00:00"},
		{"US four-digit year", "08/14/2026 07:00:00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			csv := "datetime,weight\n" + tc.value + ",80\n"
			got := parseOK(t, csv, "kg")
			if len(got.Rows) != 1 {
				t.Fatalf("got %d rows (skipped %v), want 1", len(got.Rows), got.Skipped)
			}
			if got.Rows[0].RecordedAt.Hour() != 7 {
				t.Errorf("hour = %d, want 7", got.Rows[0].RecordedAt.Hour())
			}
		})
	}
}

func TestParseHeaderIsCaseAndSpaceInsensitive(t *testing.T) {
	csv := "  TimeStamp , Weight Grams ,Source\n2026-08-14T06:00:00Z,81500,x\n"
	got := parseOK(t, csv, "kg")
	if len(got.Rows) != 1 {
		t.Fatalf("got %d rows (skipped %v), want 1", len(got.Rows), got.Skipped)
	}
	if !nearlyEqual(got.Rows[0].WeightKg, 81.5) {
		t.Errorf("weight = %v, want 81.5", got.Rows[0].WeightKg)
	}
}

func TestParseRejectsUnrecognizedHeaders(t *testing.T) {
	tests := []struct {
		name string
		csv  string
	}{
		{"no weight column", "timestamp,steps\n2026-08-14T06:00:00Z,1000\n"},
		{"no date column", "weight_kg,source\n81.5,scale\n"},
		{"only a date, no time, with no combined column", "date,weight\n2026-08-14,81.5\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(tc.csv), "kg"); err == nil {
				t.Error("Parse succeeded, want an error naming the bad header")
			}
		})
	}
}

func TestParseEmptyInput(t *testing.T) {
	if _, err := Parse(strings.NewReader(""), "kg"); err == nil {
		t.Error("Parse of empty input succeeded, want an error")
	}
}

func TestParseHeaderOnlyYieldsNoRows(t *testing.T) {
	got := parseOK(t, "recorded_at,weight_kg\n", "kg")
	if len(got.Rows) != 0 || len(got.Skipped) != 0 {
		t.Errorf("got %d rows and %d skips, want none of either", len(got.Rows), len(got.Skipped))
	}
}

func TestParseSkipsBadRowsWithoutAbortingTheImport(t *testing.T) {
	csv := "recorded_at,weight_kg\n" +
		"2026-08-14T07:00:00Z,81.5\n" + // good
		"not-a-date,80\n" + // bad timestamp
		"2026-08-15T07:00:00Z,abc\n" + // bad weight
		"2026-08-16T07:00:00Z,-5\n" + // non-positive weight
		"2026-08-17T07:00:00Z,0\n" + // zero weight
		"2026-08-18T07:00:00Z,80.1\n" // good

	got := parseOK(t, csv, "kg")
	if len(got.Rows) != 2 {
		t.Errorf("got %d good rows, want 2", len(got.Rows))
	}
	if len(got.Skipped) != 4 {
		t.Fatalf("got %d skipped rows, want 4: %v", len(got.Skipped), got.Skipped)
	}

	// Line numbers are 1-based and count the header, so the reader can find
	// the offending row in their file.
	wantLines := []int{3, 4, 5, 6}
	for i, want := range wantLines {
		if got.Skipped[i].LineNumber != want {
			t.Errorf("skip %d reported line %d, want %d", i, got.Skipped[i].LineNumber, want)
		}
		if got.Skipped[i].Reason == "" {
			t.Errorf("skip %d has no reason", i)
		}
	}
}

func TestParseToleratesRaggedRows(t *testing.T) {
	// FieldsPerRecord = -1, so a short row is skipped rather than aborting.
	csv := "recorded_at,weight_kg,source\n" +
		"2026-08-14T07:00:00Z,81.5,scale\n" +
		"2026-08-15T07:00:00Z\n" + // missing the weight field
		"2026-08-16T07:00:00Z,81.2,scale\n"

	got := parseOK(t, csv, "kg")
	if len(got.Rows) != 2 {
		t.Errorf("got %d rows, want 2", len(got.Rows))
	}
	if len(got.Skipped) != 1 {
		t.Errorf("got %d skips, want 1: %v", len(got.Skipped), got.Skipped)
	}
}

func TestToKg(t *testing.T) {
	tests := []struct {
		value float64
		unit  string
		want  float64
	}{
		{81.5, "kg", 81.5},
		{1000, "g", 1},
		{180, "lb", 81.6466266},
	}
	for _, tc := range tests {
		t.Run(tc.unit, func(t *testing.T) {
			got, err := toKg(tc.value, tc.unit)
			if err != nil {
				t.Fatalf("toKg: %v", err)
			}
			if !nearlyEqual(got, tc.want) {
				t.Errorf("toKg(%v, %q) = %v, want %v", tc.value, tc.unit, got, tc.want)
			}
		})
	}

	if _, err := toKg(80, "stone"); err == nil {
		t.Error("toKg accepted an unknown unit, want an error")
	}
}
