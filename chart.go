package main

import (
	"fmt"
	"strconv"
	"time"

	"weight-tracker/internal/db"
)

// trendWindowDays is the trailing window (in real elapsed days, not sample
// count) used to smooth the raw weight series. 7 days matches the default
// used by other weight-tracking apps and lines up with a weekly cadence of
// water/sodium fluctuation.
const trendWindowDays = 7.0

// ChartPoint is one plotted point, sent to the client for Chart.js to
// render directly — no server-side pixel math, Chart.js handles scaling,
// gridlines, and tooltips itself. Date/Value are pre-formatted so the
// client's tooltip callback doesn't need to duplicate any formatting logic.
type ChartPoint struct {
	X     int64   `json:"x"` // unix ms
	Y     float64 `json:"y"`
	Color string  `json:"color"` // "morning"/"evening"/"loss"/"gain"
	Date  string  `json:"date"`
	Value string  `json:"value"`
}

// XY is a plain point used for the trend line and goal-weight reference
// line, which don't need their own tooltips.
type XY struct {
	X int64   `json:"x"`
	Y float64 `json:"y"`
}

type ChartData struct {
	HasData bool          `json:"hasData"`
	Empty   string        `json:"empty,omitempty"`
	IsBar   bool          `json:"isBar"`
	XMin    int64         `json:"xMin,omitempty"`
	XMax    int64         `json:"xMax,omitempty"`
	Points  []ChartPoint  `json:"points"`
	Trend   []XY          `json:"trend,omitempty"`
	Goals   []XY          `json:"goals,omitempty"`
	Markers []MarkerPoint `json:"markers,omitempty"`
}

func formatDateLabel(t time.Time) string {
	return t.Format("Jan 2")
}

func msOf(t time.Time) int64 {
	return t.UnixMilli()
}

// dayNum maps a timestamp onto a continuous timeline (fractional days since
// the Unix epoch) so point spacing/windowing reflects real elapsed time,
// including gaps where nothing was logged.
func dayNum(t time.Time) float64 {
	return float64(t.Unix()) / 86400.0
}

// rangeWindow is a resolved visible-range bound. Either side may be
// unbounded (hasFrom/hasUntil false) — both are for the "all time" preset,
// and either independently for a custom range where the user left that
// side blank.
type rangeWindow struct {
	from     time.Time
	hasFrom  bool
	until    time.Time
	hasUntil bool
}

func (w rangeWindow) contains(t time.Time) bool {
	if w.hasFrom && t.Before(w.from) {
		return false
	}
	if w.hasUntil && t.After(w.until) {
		return false
	}
	return true
}

// resolveRangeWindow turns the "range" query param into a rangeWindow: a
// preset day count ("7", "30", ...) becomes an inclusive trailing window
// ending now; "all" (or anything else unrecognized) is unbounded; "custom"
// reads the from/until query params instead.
func resolveRangeWindow(rangeParam, fromParam, untilParam string, today time.Time) rangeWindow {
	if rangeParam == "custom" {
		return customRangeWindow(fromParam, untilParam, today.Location())
	}
	days, err := strconv.Atoi(rangeParam)
	if err != nil || days <= 0 {
		return rangeWindow{}
	}
	y, m, d := today.Date()
	startOfToday := time.Date(y, m, d, 0, 0, 0, 0, today.Location())
	return rangeWindow{from: startOfToday.AddDate(0, 0, -days+1), hasFrom: true}
}

// customRangeWindow parses the "from"/"until" query params (date-only, e.g.
// "2026-01-01") for a custom range. Each is independently optional — a
// blank or unparseable side just leaves that end of the range unbounded.
// "until" is inclusive of the whole day.
func customRangeWindow(fromParam, untilParam string, loc *time.Location) rangeWindow {
	var w rangeWindow
	if t, err := time.ParseInLocation(dateOnlyLayout, fromParam, loc); err == nil {
		w.from, w.hasFrom = t, true
	}
	if t, err := time.ParseInLocation(dateOnlyLayout, untilParam, loc); err == nil {
		w.until = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, loc)
		w.hasUntil = true
	}
	return w
}

func emptyChartMessage(seriesParam string) string {
	switch seriesParam {
	case "morning-delta":
		return "Not enough data yet — log at least two morning weigh-ins to see day-over-day deltas."
	case "evening-delta":
		return "Not enough data yet — log at least two evening weigh-ins to see day-over-day deltas."
	default:
		return "No entries in this range yet."
	}
}

// sequentialDeltas computes, for each entry of the given period (chrono must
// be sorted ascending by RecordedAt), the delta against the immediately
// preceding entry of the SAME period — e.g. this morning's weight vs
// whatever the last morning entry was, even if that was several days ago.
// This is a plain day-over-day comparison, distinct from the overnight/daily
// deltas used in the history list (which compare across periods: morning
// vs. the prior evening, or evening vs. that same day's morning).
func sequentialDeltas(chrono []db.Entry, period string) map[int64]float64 {
	deltas := make(map[int64]float64)
	var last *db.Entry
	for i := range chrono {
		e := &chrono[i]
		if entryPeriod(*e) != period {
			continue
		}
		if last != nil {
			deltas[e.ID] = e.WeightKg - last.WeightKg
		}
		last = e
	}
	return deltas
}

type chartRawPoint struct {
	x     float64
	t     time.Time
	val   float64
	class string
}

// rollingTrend computes, for each point in series (must be sorted ascending
// by x), the mean of val over all points with x in [point.x - windowDays,
// point.x]. O(n) via a two-pointer sliding window.
func rollingTrend(series []chartRawPoint, windowDays float64) []chartRawPoint {
	trend := make([]chartRawPoint, len(series))
	sum := 0.0
	start := 0
	for i, p := range series {
		sum += p.val
		for series[start].x < p.x-windowDays {
			sum -= series[start].val
			start++
		}
		trend[i] = chartRawPoint{x: p.x, t: p.t, val: sum / float64(i-start+1)}
	}
	return trend
}

// filterByWindow drops points outside window (a no-op if window is fully
// unbounded). Used to trim a rolling trend — computed over full history so
// its window is never truncated at the visible range's edge — back down to
// the same visible window as the raw series.
func filterByWindow(pts []chartRawPoint, window rangeWindow) []chartRawPoint {
	if !window.hasFrom && !window.hasUntil {
		return pts
	}
	var out []chartRawPoint
	for _, p := range pts {
		if window.contains(p.t) {
			out = append(out, p)
		}
	}
	return out
}

func buildChartData(allEntries []db.Entry, goals []db.Goal, markers []db.Marker, rangeParam, seriesParam, fromParam, untilParam string, today time.Time) ChartData {
	chrono, _, _ := chronologicalWithDeltas(allEntries)
	window := resolveRangeWindow(rangeParam, fromParam, untilParam, today)
	isBar := seriesParam == "morning-delta" || seriesParam == "evening-delta"

	var morningDeltaByID, eveningDeltaByID map[int64]float64
	switch seriesParam {
	case "morning-delta":
		morningDeltaByID = sequentialDeltas(chrono, "morning")
	case "evening-delta":
		eveningDeltaByID = sequentialDeltas(chrono, "evening")
	}

	var pts, trendSourcePts []chartRawPoint
	for _, e := range chrono {
		period := entryPeriod(e)
		visible := window.contains(e.RecordedAt)
		switch seriesParam {
		case "morning", "evening":
			if period != seriesParam {
				continue
			}
			p := chartRawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: e.WeightKg, class: period}
			trendSourcePts = append(trendSourcePts, p)
			if visible {
				pts = append(pts, p)
			}
		case "morning-delta":
			if !visible {
				continue
			}
			delta, ok := morningDeltaByID[e.ID]
			if !ok {
				continue
			}
			pts = append(pts, chartRawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: delta, class: deltaClass(delta)})
		case "evening-delta":
			if !visible {
				continue
			}
			delta, ok := eveningDeltaByID[e.ID]
			if !ok {
				continue
			}
			pts = append(pts, chartRawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: delta, class: deltaClass(delta)})
		default: // "all"
			p := chartRawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: e.WeightKg, class: period}
			trendSourcePts = append(trendSourcePts, p)
			if visible {
				pts = append(pts, p)
			}
		}
	}

	if len(pts) == 0 {
		return ChartData{Empty: emptyChartMessage(seriesParam)}
	}

	data := ChartData{
		HasData: true,
		IsBar:   isBar,
		XMin:    msOf(pts[0].t),
		XMax:    msOf(pts[len(pts)-1].t),
	}

	for _, p := range pts {
		valueLabel := fmt.Sprintf("%.1f kg", p.val)
		if isBar {
			valueLabel = fmt.Sprintf("%+.1f kg", p.val)
		}
		data.Points = append(data.Points, ChartPoint{
			X:     msOf(p.t),
			Y:     p.val,
			Color: p.class,
			Date:  formatDateLabel(p.t),
			Value: valueLabel,
		})
	}

	// The trend line and goal reference lines only apply to the continuous
	// weight-value series (all/morning/evening), never the delta bar charts.
	if !isBar {
		trendVisible := filterByWindow(rollingTrend(trendSourcePts, trendWindowDays), window)
		if len(trendVisible) >= 2 {
			for _, p := range trendVisible {
				data.Trend = append(data.Trend, XY{X: msOf(p.t), Y: p.val})
			}
		}

		if len(goals) > 0 {
			for _, seg := range clipGoalSegments(buildGoalSegments(goals), pts[0].t, pts[len(pts)-1].t) {
				// Both endpoints of each segment are included so consecutive
				// segments with different goal weights connect via a
				// vertical jump at the boundary, rendering a step shape with
				// a single plain line dataset.
				data.Goals = append(data.Goals,
					XY{X: msOf(seg.From), Y: seg.WeightKg},
					XY{X: msOf(seg.Until), Y: seg.WeightKg},
				)
			}
		}
	}

	// Markers add context ("started new diet") regardless of which series
	// is being viewed, so — unlike trend/goal — they apply to bar charts too.
	if len(markers) > 0 {
		data.Markers = visibleMarkers(markers, pts[0].t, pts[len(pts)-1].t)
	}

	return data
}

func deltaClass(delta float64) string {
	if delta < 0 {
		return "loss"
	}
	return "gain"
}
