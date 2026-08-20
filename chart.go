package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
	HasData bool         `json:"hasData"`
	Empty   string       `json:"empty,omitempty"`
	IsBar   bool         `json:"isBar"`
	XMin    int64        `json:"xMin,omitempty"`
	XMax    int64        `json:"xMax,omitempty"`
	Points  []ChartPoint `json:"points"`
	// Trend is the single rolling-average line for a single-period series
	// (morning/evening only, or a delta series). The "all" series — both
	// periods at once — instead splits it into TrendMorning/TrendEvening,
	// each smoothed over its own period's points, since averaging morning
	// and evening readings together produces a line that isn't a rolling
	// average of either.
	Trend        []XY          `json:"trend,omitempty"`
	TrendMorning []XY          `json:"trendMorning,omitempty"`
	TrendEvening []XY          `json:"trendEvening,omitempty"`
	Goals        []XY          `json:"goals,omitempty"`
	Markers      []MarkerPoint `json:"markers,omitempty"`
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

// TimeRangePickerConfig configures one instance of the shared
// "time-range-picker" template partial (see templates/time_range_picker.html)
// — the chart and the history filter each embed their own, with different
// defaults, since a chart benefits from a bounded default view but a list
// is more useful starting unfiltered.
type TimeRangePickerConfig struct {
	DefaultRange string // preset value ("30", "all", ...) selected by default
	DefaultLabel string // the button's initial text, matching DefaultRange
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

// resolveRangeWindow turns the "range" query param into a rangeWindow:
//   - a preset day count ("7", "30", ...) is an inclusive trailing window
//     ending now
//   - "this-year" runs from midnight on 1 January
//   - "custom" reads the from/until params instead
//   - "all", or anything unrecognized, is unbounded
func resolveRangeWindow(rangeParam, fromParam, untilParam string, today time.Time) rangeWindow {
	switch rangeParam {
	case "custom":
		return customRangeWindow(fromParam, untilParam, today)
	case "this-year":
		start := time.Date(today.Year(), time.January, 1, 0, 0, 0, 0, today.Location())
		return rangeWindow{from: start, hasFrom: true}
	}
	days, err := strconv.Atoi(rangeParam)
	if err != nil || days <= 0 {
		return rangeWindow{}
	}
	y, m, d := today.Date()
	startOfToday := time.Date(y, m, d, 0, 0, 0, 0, today.Location())
	return rangeWindow{from: startOfToday.AddDate(0, 0, -days+1), hasFrom: true}
}

// relativeTimePattern matches the subset of Grafana's relative time syntax
// worth supporting here: "now", or "now-" a count and a unit. The units are
// Grafana's own and are case-sensitive in the same way — "m" is minutes and
// "M" is months, which is worth knowing before typing "now-6m" and getting
// six minutes of chart. Only "-" is accepted: "now" only ever looks
// backward, unlike the cross-reference expressions below where either
// direction is meaningful.
var relativeTimePattern = regexp.MustCompile(`^now(?:-(\d+)([smhdwMy]))?$`)

// applyOffset shifts t by count units of unit, in the direction sign ("+"
// or "-"). Days and larger use AddDate rather than a fixed multiple of
// 24h, so the arithmetic is calendar-correct: an offset of one month lands
// on the same day of the target month whatever its length, and a day step
// across a DST boundary stays at the same wall-clock time. Reports ok=false
// for an unrecognized unit.
func applyOffset(t time.Time, sign string, count int, unit string) (time.Time, bool) {
	n := count
	if sign == "-" {
		n = -n
	}
	switch unit {
	case "s":
		return t.Add(time.Duration(n) * time.Second), true
	case "m":
		return t.Add(time.Duration(n) * time.Minute), true
	case "h":
		return t.Add(time.Duration(n) * time.Hour), true
	case "d":
		return t.AddDate(0, 0, n), true
	case "w":
		return t.AddDate(0, 0, 7*n), true
	case "M":
		return t.AddDate(0, n, 0), true
	case "y":
		return t.AddDate(n, 0, 0), true
	}
	return t, false
}

// parseRelativeTime resolves an expression like "now-5d" against now.
func parseRelativeTime(value string, now time.Time) (time.Time, bool) {
	m := relativeTimePattern.FindStringSubmatch(value)
	if m == nil {
		return time.Time{}, false
	}
	if m[1] == "" {
		return now, true // bare "now"
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return time.Time{}, false
	}
	return applyOffset(now, "-", n, m[2])
}

// crossRefPattern matches a bound expressed relative to the OTHER bound:
// "to" or "from" (naming the other query param), a sign, a count, and a
// unit — e.g. "to-5d" in the from field ("5 days before whatever the until
// field resolves to"), or "from+5d" in the until field ("5 days after
// whatever the from field resolves to"). Unlike "now", both signs are
// useful here since either field can reasonably sit before or after the
// other.
var crossRefPattern = regexp.MustCompile(`^(to|from)([+-])(\d+)([smhdwMy])$`)

// parseCrossRef resolves value as a reference to keyword ("to" or "from"),
// offset from anchor — the OTHER field's own already-resolved value. ok is
// false if value doesn't reference keyword, or if anchor never resolved
// (hasAnchor false): there's nothing to offset from in that case.
func parseCrossRef(value, keyword string, anchor time.Time, hasAnchor bool) (time.Time, bool) {
	m := crossRefPattern.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil || m[1] != keyword || !hasAnchor {
		return time.Time{}, false
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return time.Time{}, false
	}
	return applyOffset(anchor, m[2], n, m[4])
}

// parseRangeBound reads one side of a custom range, which may be either a
// calendar date ("2026-01-01") or a relative expression ("now-5d"). It
// does not resolve cross-references ("to-5d", "from+5d") — those need the
// other side's value too, so customRangeWindow handles them separately.
func parseRangeBound(value string, now time.Time) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if t, isRelative := parseRelativeTime(value, now); isRelative {
		return t, true
	}
	if t, err := time.ParseInLocation(dateOnlyLayout, value, now.Location()); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// startOfDay and endOfDay snap a bound to the day that contains it.
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func endOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 23, 59, 59, 999999999, t.Location())
}

// customRangeWindow parses the "from"/"until" query params for a custom
// range. Each side is independently optional — blank or unparseable just
// leaves that end unbounded — and each accepts a date, a relative
// expression ("now-5d"), or a reference to the OTHER side ("to-5d" in
// from, "from+5d" in until), resolved once that other side's own value is
// known. So from=now-30d / until=from+5d and from=to-5d / until=now both
// work, but a side that only makes sense via the other — blank, itself a
// cross-reference, or otherwise unparseable — leaves that side unbounded:
// there is nothing to anchor a cross-reference to.
//
// Both ends are snapped to whole days: the start to 00:00 and the end to
// 23:59:59.999. A range is a span of dates here, not of instants, so every
// weigh-in on the named days is included — without this, "now-5d" run at
// 14:30 would silently drop that morning's entry five days ago, and "now"
// would drop this evening's. It does mean the sub-day units (s, m, h) round
// to the same day boundaries as everything else.
func customRangeWindow(fromParam, untilParam string, now time.Time) rangeWindow {
	fromT, fromOK := parseRangeBound(fromParam, now)
	untilT, untilOK := parseRangeBound(untilParam, now)

	// Second pass: whichever side didn't resolve on its own gets a chance
	// to resolve against the side that did (or that just did, above).
	if !fromOK {
		fromT, fromOK = parseCrossRef(fromParam, "to", untilT, untilOK)
	}
	if !untilOK {
		untilT, untilOK = parseCrossRef(untilParam, "from", fromT, fromOK)
	}

	var w rangeWindow
	if fromOK {
		w.from, w.hasFrom = startOfDay(fromT), true
	}
	if untilOK {
		w.until, w.hasUntil = endOfDay(untilT), true
	}
	return w
}

func emptyChartMessage(seriesParam string) string {
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
		if entryPeriod(*e) != period {
			continue
		}
		if last != nil {
			deltas[e.ID] = e.WeightG - last.WeightG
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

// filterByClass keeps only points of the given class, preserving order —
// used to compute a rolling trend per period rather than one trend blending
// morning and evening readings together.
func filterByClass(pts []chartRawPoint, class string) []chartRawPoint {
	var out []chartRawPoint
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
func trendXY(source []chartRawPoint, window rangeWindow) []XY {
	visible := filterByWindow(rollingTrend(source, trendWindowDays), window)
	if len(visible) < 2 {
		return nil
	}
	xy := make([]XY, len(visible))
	for i, p := range visible {
		xy[i] = XY{X: msOf(p.t), Y: p.val}
	}
	return xy
}

func buildChartData(allEntries []db.Entry, goals []db.Goal, markers []db.Marker, rangeParam, seriesParam, fromParam, untilParam string, today time.Time) ChartData {
	chrono, overnightByID, dailyByID := chronologicalWithDeltas(allEntries)
	window := resolveRangeWindow(rangeParam, fromParam, untilParam, today)
	isBar := seriesParam == "morning-delta" || seriesParam == "evening-delta" || seriesParam == "overnight" || seriesParam == "daily"

	var morningDeltaByID, eveningDeltaByID map[int64]int64
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
			p := chartRawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: db.GramsToKg(e.WeightG), class: period}
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
			pts = append(pts, chartRawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: db.GramsToKg(delta), class: deltaClass(delta)})
		case "evening-delta":
			if !visible {
				continue
			}
			delta, ok := eveningDeltaByID[e.ID]
			if !ok {
				continue
			}
			pts = append(pts, chartRawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: db.GramsToKg(delta), class: deltaClass(delta)})
		case "overnight":
			if !visible {
				continue
			}
			delta, ok := overnightByID[e.ID]
			if !ok {
				continue
			}
			pts = append(pts, chartRawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: db.GramsToKg(delta), class: deltaClass(delta)})
		case "daily":
			if !visible {
				continue
			}
			delta, ok := dailyByID[e.ID]
			if !ok {
				continue
			}
			pts = append(pts, chartRawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: db.GramsToKg(delta), class: deltaClass(delta)})
		default: // "all"
			p := chartRawPoint{x: dayNum(e.RecordedAt), t: e.RecordedAt, val: db.GramsToKg(e.WeightG), class: period}
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
		if seriesParam == "all" {
			// Split into a trend per period rather than one line averaging
			// morning and evening readings together, which would smooth
			// over the very gap between them that the raw lines show.
			data.TrendMorning = trendXY(filterByClass(trendSourcePts, "morning"), window)
			data.TrendEvening = trendXY(filterByClass(trendSourcePts, "evening"), window)
		} else {
			data.Trend = trendXY(trendSourcePts, window)
		}

		if len(goals) > 0 {
			for _, seg := range clipGoalSegments(buildGoalSegments(goals), pts[0].t, pts[len(pts)-1].t) {
				// Both endpoints of each segment are included so consecutive
				// segments with different goal weights connect via a
				// vertical jump at the boundary, rendering a step shape with
				// a single plain line dataset.
				data.Goals = append(data.Goals,
					XY{X: msOf(seg.From), Y: db.GramsToKg(seg.WeightG)},
					XY{X: msOf(seg.Until), Y: db.GramsToKg(seg.WeightG)},
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

func deltaClass(deltaG int64) string {

	if deltaG < 0 {
		return "loss"
	}
	return "gain"
}
