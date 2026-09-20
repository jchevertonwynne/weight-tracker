// Package chart builds the JSON payload the client-side Chart.js instance
// renders directly — no server-side pixel math, just points, a rolling
// trend line, and clipped goal/marker overlays.
package chart

import (
	"fmt"
	"math"
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

const (
	// projectionFitDays is the trailing span of readings the forward
	// projection fits its rate to. Long enough that one noisy day can't tilt
	// the slope, short enough that it reflects the current trajectory rather
	// than an older one that has since changed.
	projectionFitDays = 28.0
	// projectionHorizonDays is how far ahead the projection is drawn when no
	// goal pulls it further — six weeks is long enough to be worth reading
	// and short enough that the widening band hasn't yet said everything.
	projectionHorizonDays = 42.0
	// projectionMaxDays caps the horizon when an active goal would otherwise
	// stretch it out. Past this the band is so wide it makes no claim worth
	// drawing, so a goal that far off is left off the projection.
	projectionMaxDays = 180.0
	// projectionMaxAreaFraction caps the projection at this share of the whole
	// chart width, so it never dwarfs the readings it grows from — a 42-day
	// projection tacked onto a 7-day view looks absurd. A projection that is a
	// fraction f of the total spans f/(1-f) of the visible data, so the day
	// cap is derived from the visible span accordingly.
	projectionMaxAreaFraction = 0.25
	// projectionSigma is how many prediction standard errors the band spans
	// on each side of the centre. ±1 matches the overnight chart's ±1 SD, so
	// the two views make the same kind of claim about how much of the spread
	// they cover rather than dressing one up as a fixed confidence percentage.
	projectionSigma = 1.0
	// projectionSamples is how many segments the curved band edges are drawn
	// from; the interval widens as a square root, so too few would visibly
	// cut the corner.
	projectionSamples = 24
	// projectionMinPoints and projectionMinSpanDays gate the fit: too few
	// readings, or all bunched into a day or two, and the slope is noise
	// dressed as a direction.
	projectionMinPoints   = 4
	projectionMinSpanDays = 10.0
)

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
	// Projection is the forward extension of the trend, present only when the
	// client asked for it (see Build's showProjection) and there is enough
	// recent data to fit one. When set, the axis (XMax) has already been
	// widened to show its full horizon.
	Projection *ProjectionBand `json:"projection,omitempty"`
}

// ProjectionBand is the forward extension of the trend: a centre line
// continuing the current rate, wrapped in a band that widens with the
// horizon. The band is an ordinary-least-squares prediction interval, so it
// widens for two honest reasons at once — a single reading scatters around
// the line, and the slope itself is only estimated from a finite window — and
// answers "where is this heading, and how sure can the data be?" rather than
// projecting one confident line into the future.
type ProjectionBand struct {
	Center    []XY   `json:"center"`
	Upper     []XY   `json:"upper"`
	Lower     []XY   `json:"lower"`
	RateLabel string `json:"rateLabel"` // current fitted rate, e.g. "-0.4 kg/week"
}

func dayNum(t time.Time) float64 {
	return float64(t.Unix()) / 86400.0
}

// dayNumToTime is the inverse of dayNum, used to place projected points back
// on the time axis. Sub-day precision is irrelevant to a forward projection,
// so the round-trip through whole days costs nothing.
func dayNumToTime(x float64) time.Time {
	return time.Unix(int64(math.Round(x*86400)), 0).UTC()
}

// buildProjection fits an ordinary-least-squares line to the most recent
// projectionFitDays of source (the same per-period readings the trend is
// smoothed from) and extends it forward from the last trend value. It returns
// the band, how far the axis must reach to show it, and ok=false when there
// is too little recent data to project honestly.
//
// The centre is anchored at the last trend value rather than the fitted
// line's own endpoint, so it continues the drawn trend line without a step;
// the slope and the band width both come from the fit. The band is a
// prediction interval, widening with the horizon because the residual scatter
// and the slope uncertainty both push it out (the (x-x̄)²/Sxx term).
//
// maxHorizonDays caps how far ahead the band reaches, keeping it from dwarfing
// the readings on a short range (see projectionMaxAreaFraction).
func buildProjection(source, trend []rawPoint, allGoals []db.Goal, maxHorizonDays float64, today time.Time) (*ProjectionBand, time.Time, bool) {
	if len(trend) == 0 {
		return nil, time.Time{}, false
	}
	last := trend[len(trend)-1]

	cutoff := last.x - projectionFitDays
	var fit []rawPoint
	for _, p := range source {
		if p.x >= cutoff {
			fit = append(fit, p)
		}
	}
	n := len(fit)
	// n-2 degrees of freedom for the residual standard error, so three points
	// is the floor; projectionMinPoints keeps a step above that, and the span
	// gate rejects a fit made from readings all clustered into a day or two.
	if n < projectionMinPoints || fit[n-1].x-fit[0].x < projectionMinSpanDays {
		return nil, time.Time{}, false
	}

	var sumX, sumY float64
	for _, p := range fit {
		sumX += p.x
		sumY += p.val
	}
	meanX, meanY := sumX/float64(n), sumY/float64(n)

	var sxx, sxy float64
	for _, p := range fit {
		dx := p.x - meanX
		sxx += dx * dx
		sxy += dx * (p.val - meanY)
	}
	if sxx == 0 {
		return nil, time.Time{}, false
	}
	slope := sxy / sxx
	intercept := meanY - slope*meanX

	var sse float64
	for _, p := range fit {
		resid := p.val - (intercept + slope*p.x)
		sse += resid * resid
	}
	s := math.Sqrt(sse / float64(n-2))

	horizon := projectionHorizonDays
	if goal, ok := goals.Current(allGoals, today); ok && slope != 0 {
		// Extend the horizon to meet the goal only when the current rate is
		// actually closing on it and it lands within the cap; a slope heading
		// away, or a goal too far off to project usefully, keeps the default.
		if d := (db.GramsToKg(goal.WeightG) - last.val) / slope; d > horizon && d <= projectionMaxDays {
			horizon = d
		}
	}
	// Never let the projection outgrow its share of the chart — on a short
	// range this cap dominates, pulling a six-week default down to a few days.
	if maxHorizonDays > 0 && horizon > maxHorizonDays {
		horizon = maxHorizonDays
	}

	band := &ProjectionBand{RateLabel: fmt.Sprintf("%+.1f kg/week", slope*7)}
	for i := 0; i <= projectionSamples; i++ {
		d := horizon * float64(i) / float64(projectionSamples)
		x := last.x + d
		center := last.val + slope*d
		se := s * math.Sqrt(1+1/float64(n)+(x-meanX)*(x-meanX)/sxx)
		half := projectionSigma * se
		ms := timerange.MsOf(dayNumToTime(x))
		band.Center = append(band.Center, XY{X: ms, Y: center})
		band.Upper = append(band.Upper, XY{X: ms, Y: center + half})
		band.Lower = append(band.Lower, XY{X: ms, Y: center - half})
	}
	return band, dayNumToTime(last.x + horizon), true
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
// When showProjection is set, a single-period series (morning/evening) is
// extended with a forward projection band and the axis widened to show it;
// the flag is ignored for the delta bar charts and the split "all" view,
// which have no single trend line to continue.
func Build(allEntries []db.Entry, allGoals []db.Goal, allMarkers []db.Marker, rangeParam, seriesParam, fromParam, untilParam string, showProjection bool, today time.Time) Data {
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
			// A single-period trend can be extended forward. Do this before
			// clipping the goal line below so both reach the same widened
			// axisUntil — that way the cone visibly runs up to the goal it is
			// heading for, rather than the goal line stopping where the
			// projection begins.
			if showProjection {
				fullTrend := rollingTrend(trendSourcePts, trendWindowDays)
				// Cap the horizon at a share of the visible span so the
				// projection stays at most projectionMaxAreaFraction of the
				// whole chart once it is tacked on the end.
				visibleDays := axisUntil.Sub(axisFrom).Hours() / 24
				maxHorizon := visibleDays * projectionMaxAreaFraction / (1 - projectionMaxAreaFraction)
				if band, end, ok := buildProjection(trendSourcePts, fullTrend, allGoals, maxHorizon, today); ok {
					data.Projection = band
					if end.After(axisUntil) {
						axisUntil = end
						data.XMax = timerange.MsOf(axisUntil)
					}
				}
			}
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
