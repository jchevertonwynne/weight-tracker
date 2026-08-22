// Package chart builds the JSON payload the client-side Chart.js instance
// renders directly — no server-side pixel math, just points, a rolling
// trend line, and clipped goal/marker overlays.
package chart

import (
	"fmt"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/goals"
	"weight-tracker/internal/markers"
	"weight-tracker/internal/timerange"
	"weight-tracker/internal/weight"
)

// trendWindowDays is the trailing window (in real elapsed days, not sample
// count) used to smooth the raw weight series. 7 days matches the default
// used by other weight-tracking apps and lines up with a weekly cadence of
// water/sodium fluctuation.
const trendWindowDays = 7.0

// Point is one plotted point, sent to the client for Chart.js to render
// directly. Date/Value are pre-formatted so the client's tooltip callback
// doesn't need to duplicate any formatting logic.
type Point struct {
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

type Data struct {
	HasData bool    `json:"hasData"`
	Empty   string  `json:"empty,omitempty"`
	IsBar   bool    `json:"isBar"`
	XMin    int64   `json:"xMin,omitempty"`
	XMax    int64   `json:"xMax,omitempty"`
	Points  []Point `json:"points"`
	// Trend is the single rolling-average line for a single-period series
	// (morning/evening only, or a delta series). The "all" series — both
	// periods at once — instead splits it into TrendMorning/TrendEvening,
	// each smoothed over its own period's points, since averaging morning
	// and evening readings together produces a line that isn't a rolling
	// average of either.
	Trend        []XY            `json:"trend,omitempty"`
	TrendMorning []XY            `json:"trendMorning,omitempty"`
	TrendEvening []XY            `json:"trendEvening,omitempty"`
	Goals        []XY            `json:"goals,omitempty"`
	Markers      []markers.Point `json:"markers,omitempty"`
}

func dayNum(t time.Time) float64 {
	return float64(t.Unix()) / 86400.0
}

func emptyMessage(seriesParam string) string {
	switch seriesParam {
	case "morning-delta":
		return "Not enough data yet — log at least two morning weigh-ins to see day-over-day deltas."
	case "evening-delta":
		return "Not enough data yet — log at least two evening weigh-ins to see day-over-day deltas."
	case "overnight":
		return "Not enough data yet — log an evening weigh-in followed by the next morning's to see overnight changes."
	case "daily":
		return "Not enough data yet — log a morning and evening weigh-in on the same day to see daily changes."
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
func sequentialDeltas(chrono []db.Entry, period string) map[int64]int64 {
	deltas := make(map[int64]int64)
	var last *db.Entry
	for i := range chrono {
		e := &chrono[i]
		if weight.EntryPeriod(*e) != period {
			continue
		}
		if last != nil {
			deltas[e.ID] = e.WeightG - last.WeightG
		}
		last = e
	}
	return deltas
}

type rawPoint struct {
	x     float64
	t     time.Time
	val   float64
	class string
}

// rollingTrend computes, for each point in series (must be sorted ascending
// by x), the mean of val over all points with x in [point.x - windowDays,
// point.x]. O(n) via a two-pointer sliding window.
func rollingTrend(series []rawPoint, windowDays float64) []rawPoint {
	trend := make([]rawPoint, len(series))
	sum := 0.0
	start := 0
	for i, p := range series {
		sum += p.val
		for series[start].x < p.x-windowDays {
			sum -= series[start].val
			start++
		}
		trend[i] = rawPoint{x: p.x, t: p.t, val: sum / float64(i-start+1)}
	}
	return trend
}

// filterByWindow drops points outside window (a no-op if window is fully
// unbounded). Used to trim a rolling trend — computed over full history so
// its window is never truncated at the visible range's edge — back down to
// the same visible window as the raw series.
func filterByWindow(pts []rawPoint, window timerange.Window) []rawPoint {
	if !window.HasFrom && !window.HasUntil {
		return pts
	}
	var out []rawPoint
	for _, p := range pts {
		if window.Contains(p.t) {
			out = append(out, p)
		}
	}
	return out
}

// filterByClass keeps only points of the given class, preserving order —
// used to compute a rolling trend per period rather than one trend blending
// morning and evening readings together.
func filterByClass(pts []rawPoint, class string) []rawPoint {
	var out []rawPoint
	for _, p := range pts {
		if p.class == class {
			out = append(out, p)
		}
	}
	return out
}

// trendXY runs the rolling average and window-trims it down to plain XY
// points ready for the client, or nil if fewer than 2 points survive — a
// single-point "trend" isn't a line.
func trendXY(source []rawPoint, window timerange.Window) []XY {
	visible := filterByWindow(rollingTrend(source, trendWindowDays), window)
	if len(visible) < 2 {
		return nil
	}
	xy := make([]XY, len(visible))
	for i, p := range visible {
		xy[i] = XY{X: timerange.MsOf(p.t), Y: p.val}
	}
	return xy
}

// axisExtent decides how far the x-axis runs. A bounded end of the
// requested window wins over the data, so a range shows its whole span
// whether or not every day of it was logged; an unbounded end falls back
// to the data, extended to now so an open-ended range like "last 30 days"
// still reaches today rather than stopping at the most recent reading.
//
// firstPoint/lastPoint are the extremes of the visible data, and are used
// verbatim for "all time", which is unbounded at both ends and so is
// exactly as wide as the data.
func axisExtent(window timerange.Window, firstPoint, lastPoint, now time.Time) (from, until time.Time) {
	from, until = firstPoint, lastPoint

	if window.HasFrom {
		from = window.From
	}
	if window.HasUntil {
		until = window.Until
	} else if window.HasFrom && now.After(until) {
		// A range with a start has a definite length, so it runs to today
		// even if the last few days went unlogged — that gap is part of what
		// the range is showing. End of today rather than this moment, so
		// "30 days" covers thirty whole days rather than twenty-nine and a
		// fraction.
		//
		// A range unbounded at both ends ("all time") has no requested
		// length, so it hugs the data instead of growing an empty tail.
		y, m, d := now.Date()
		until = time.Date(y, m, d, 23, 59, 59, int(time.Second-time.Nanosecond), now.Location())
	}

	// A window narrower than a single reading would collapse the axis.
	if !from.Before(until) {
		return firstPoint, lastPoint
	}
	return from, until
}

// Build assembles the chart JSON payload for one series/range combination.
func Build(allEntries []db.Entry, allGoals []db.Goal, allMarkers []db.Marker, rangeParam, seriesParam, fromParam, untilParam string, today time.Time) Data {
	chrono, overnightByID, dailyByID := weight.ChronologicalWithDeltas(allEntries)
	window := timerange.Resolve(rangeParam, fromParam, untilParam, today)
	isBar := seriesParam == "morning-delta" || seriesParam == "evening-delta" || seriesParam == "overnight" || seriesParam == "daily"

	var morningDeltaByID, eveningDeltaByID map[int64]int64
	switch seriesParam {
	case "morning-delta":
		morningDeltaByID = sequentialDeltas(chrono, "morning")
	case "evening-delta":
		eveningDeltaByID = sequentialDeltas(chrono, "evening")
	}

	var pts, trendSourcePts []rawPoint
	for _, e := range chrono {
		period := weight.EntryPeriod(e)
		visible := window.Contains(e.RecordedAt)
		switch seriesParam {
		case "morning", "evening":
			if period != seriesParam {
				continue
			}
			p := rawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: db.GramsToKg(e.WeightG), class: period}
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
			pts = append(pts, rawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: db.GramsToKg(delta), class: deltaClass(delta)})
		case "evening-delta":
			if !visible {
				continue
			}
			delta, ok := eveningDeltaByID[e.ID]
			if !ok {
				continue
			}
			pts = append(pts, rawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: db.GramsToKg(delta), class: deltaClass(delta)})
		case "overnight":
			if !visible {
				continue
			}
			delta, ok := overnightByID[e.ID]
			if !ok {
				continue
			}
			pts = append(pts, rawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: db.GramsToKg(delta), class: deltaClass(delta)})
		case "daily":
			if !visible {
				continue
			}
			delta, ok := dailyByID[e.ID]
			if !ok {
				continue
			}
			pts = append(pts, rawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: db.GramsToKg(delta), class: deltaClass(delta)})
		default: // "all"
			p := rawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: db.GramsToKg(e.WeightG), class: period}
			trendSourcePts = append(trendSourcePts, p)
			if visible {
				pts = append(pts, p)
			}
		}
	}

	if len(pts) == 0 {
		return Data{Empty: emptyMessage(seriesParam)}
	}

	// The axis spans the range that was asked for, not just the part of it
	// that happens to hold readings. A 30-day range with a week's data used
	// to draw a week-wide chart, which quietly misrepresented both the
	// density of the readings and how long the gap before them was.
	axisFrom, axisUntil := axisExtent(window, pts[0].t, pts[len(pts)-1].t, today)

	data := Data{
		HasData: true,
		IsBar:   isBar,
		XMin:    timerange.MsOf(axisFrom),
		XMax:    timerange.MsOf(axisUntil),
	}

	for _, p := range pts {
		valueLabel := fmt.Sprintf("%.1f kg", p.val)
		if isBar {
			valueLabel = fmt.Sprintf("%+.1f kg", p.val)
		}
		data.Points = append(data.Points, Point{
			X:     timerange.MsOf(p.t),
			Y:     p.val,
			Color: p.class,
			Date:  timerange.DateLabel(p.t),
			Value: valueLabel,
		})
	}

	// The trend line and goal reference lines only apply to the continuous
	// weight-value series (all/morning/evening), never the delta bar charts.
	if !isBar {
		if seriesParam == "all" {
			// Split into a trend per period rather than one line averaging
			// morning and evening readings together, which would smooth
			// over the very gap between them that the raw lines show.
			data.TrendMorning = trendXY(filterByClass(trendSourcePts, "morning"), window)
			data.TrendEvening = trendXY(filterByClass(trendSourcePts, "evening"), window)
		} else {
			data.Trend = trendXY(trendSourcePts, window)
		}

		if len(allGoals) > 0 {
			for _, seg := range goals.ClipSegments(goals.BuildSegments(allGoals), axisFrom, axisUntil) {
				// Both endpoints of each segment are included so consecutive
				// segments with different goal weights connect via a
				// vertical jump at the boundary, rendering a step shape with
				// a single plain line dataset.
				data.Goals = append(data.Goals,
					XY{X: timerange.MsOf(seg.From), Y: db.GramsToKg(seg.WeightG)},
					XY{X: timerange.MsOf(seg.Until), Y: db.GramsToKg(seg.WeightG)},
				)
			}
		}
	}

	// Markers add context ("started new diet") regardless of which series
	// is being viewed, so — unlike trend/goal — they apply to bar charts too.
	if len(allMarkers) > 0 {
		data.Markers = markers.Visible(allMarkers, axisFrom, axisUntil)
	}

	return data
}

func deltaClass(deltaG int64) string {
	if deltaG < 0 {
		return "loss"
	}
	return "gain"
}
