// Package importer parses weight-log CSV exports from third-party sources
// into rows this app can bulk-insert. Confirmed against a real Google Health
// (Google Takeout) export — header `timestamp,weight grams,data source`,
// UTC timestamps, integer grams. The split date+time "Fitbit CSV" path
// remains a best-effort, unverified fallback for a plain/ambiguous `weight`
// column.
package importer

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

type ParsedRow struct {
	RecordedAt time.Time
	// WeightG is whole grams, rounded from whatever unit the file used —
	// the representation the database stores (see db.KgToGrams).
	WeightG int64
}

type SkippedRow struct {
	LineNumber int
	Reason     string
}

type Result struct {
	Rows    []ParsedRow
	Skipped []SkippedRow
}

const (
	lbToKg = 0.45359237
	gToKg  = 0.001
)

// weightColumnCandidates lists recognized weight-column headers in priority
// order, paired with the unit those values are expressed in when the header
// is self-describing. An empty unit means the header is ambiguous (a bare
// "weight" column) and the caller-supplied unit parameter is used instead —
// self-describing data beats a manually-selected form guess.
var weightColumnCandidates = []struct {
	header string
	unit   string
}{
	{"weight_kg", "kg"},   // this app's own CSV export
	{"weight grams", "g"}, // Google Health (Google Takeout) export
	{"weight_grams", "g"}, // tolerate underscore variant
	{"weight_lb", "lb"},
	{"weight", ""}, // ambiguous — form-selected unit applies
}

func findWeightColumn(cols map[string]int) (idx int, impliedUnit string, ok bool) {
	for _, c := range weightColumnCandidates {
		if i, present := cols[c.header]; present {
			return i, c.unit, true
		}
	}
	return 0, "", false
}

func toKg(value float64, unit string) (float64, error) {
	switch unit {
	case "kg":
		return value, nil
	case "lb":
		return value * lbToKg, nil
	case "g":
		return value * gToKg, nil
	default:
		return 0, fmt.Errorf("unrecognized weight unit %q", unit)
	}
}

// combinedDatetimeLayouts are tried, in order, against a single combined
// date+time column (including this app's own recorded_at export).
var combinedDatetimeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"01/02/06 15:04:05",
	"01/02/2006 15:04:05",
}

// dateLayouts / timeLayouts are tried when date and time are split into two
// separate columns, a layout historically used by Fitbit's own CSV export.
var dateLayouts = []string{"01/02/06", "01/02/2006", "2006-01-02"}
var timeLayouts = []string{"15:04:05", "15:04", "3:04:05 PM", "3:04 PM"}

// Parse reads a weight CSV (Google Health, this app's own export, or a
// Fitbit-style split date+time file). unit must be "kg", "lb", or "g" — it's
// used only when the weight column's header doesn't already declare its own
// unit (e.g. "weight grams" always means grams regardless of unit). Bad rows
// are skipped and reported rather than aborting the whole import.
func Parse(r io.Reader, unit string) (Result, error) {
	if unit != "kg" && unit != "lb" && unit != "g" {
		return Result{}, fmt.Errorf("invalid unit %q", unit)
	}

	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // tolerate ragged rows

	header, err := cr.Read()
	if err != nil {
		return Result{}, fmt.Errorf("read header row: %w", err)
	}
	cols := indexHeader(header)

	weightIdx, impliedUnit, ok := findWeightColumn(cols)
	if !ok {
		return Result{}, fmt.Errorf("unrecognized CSV header, no weight column found: %v", header)
	}
	effectiveUnit := unit
	if impliedUnit != "" {
		effectiveUnit = impliedUnit
	}
	datetimeIdx, hasDatetime := firstPresent(cols, "recorded_at", "date time", "datetime", "timestamp")
	dateIdx, hasDate := firstPresent(cols, "date")
	timeIdx, hasTime := firstPresent(cols, "time")
	if !hasDatetime && !(hasDate && hasTime) {
		return Result{}, fmt.Errorf("unrecognized CSV header, no date/time column found: %v", header)
	}

	var result Result
	lineNumber := 1 // header was line 1
	for {
		lineNumber++
		record, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			result.Skipped = append(result.Skipped, SkippedRow{LineNumber: lineNumber, Reason: err.Error()})
			continue
		}

		var recordedAt time.Time
		var parseErr error
		if hasDatetime {
			recordedAt, parseErr = parseWithLayouts(field(record, datetimeIdx), combinedDatetimeLayouts)
		} else {
			recordedAt, parseErr = parseSplitDateTime(field(record, dateIdx), field(record, timeIdx))
		}
		if parseErr != nil {
			result.Skipped = append(result.Skipped, SkippedRow{LineNumber: lineNumber, Reason: "invalid date/time"})
			continue
		}
		// Timestamps carrying an explicit zone (e.g. Google Health's UTC "Z"
		// suffix) parse into that zone, not time.Local, regardless of the
		// loc argument passed to ParseInLocation — normalize so every entry
		// is represented the same way as manually-entered ones (local wall
		// clock). A no-op for values already in time.Local.
		recordedAt = recordedAt.In(time.Local)

		weightRaw, err := strconv.ParseFloat(strings.TrimSpace(field(record, weightIdx)), 64)
		if err != nil || weightRaw <= 0 {
			result.Skipped = append(result.Skipped, SkippedRow{LineNumber: lineNumber, Reason: "invalid weight"})
			continue
		}
		weight, err := toKg(weightRaw, effectiveUnit)
		if err != nil {
			result.Skipped = append(result.Skipped, SkippedRow{LineNumber: lineNumber, Reason: "invalid weight"})
			continue
		}

		result.Rows = append(result.Rows, ParsedRow{RecordedAt: recordedAt, WeightG: int64(math.Round(weight * 1000))})
	}

	return result, nil
}

func field(record []string, idx int) string {
	if idx < 0 || idx >= len(record) {
		return ""
	}
	return record[idx]
}

func indexHeader(header []string) map[string]int {
	cols := make(map[string]int, len(header))
	for i, h := range header {
		cols[strings.ToLower(strings.TrimSpace(h))] = i
	}
	return cols
}

func firstPresent(cols map[string]int, names ...string) (int, bool) {
	for _, name := range names {
		if idx, ok := cols[name]; ok {
			return idx, true
		}
	}
	return 0, false
}

func parseWithLayouts(value string, layouts []string) (time.Time, error) {
	value = strings.TrimSpace(value)
	var lastErr error
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return t, nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, lastErr
}

func parseSplitDateTime(dateStr, timeStr string) (time.Time, error) {
	dateStr, timeStr = strings.TrimSpace(dateStr), strings.TrimSpace(timeStr)
	var lastErr error
	for _, dl := range dateLayouts {
		for _, tl := range timeLayouts {
			if t, err := time.ParseInLocation(dl+" "+tl, dateStr+" "+timeStr, time.Local); err == nil {
				return t, nil
			} else {
				lastErr = err
			}
		}
	}
	return time.Time{}, lastErr
}
