// Package timerange resolves the "range"/"from"/"until" query parameters
// shared by the chart, history, and overnight views into a concrete
// [from, until] window, and provides the small time-formatting helpers
// (MsOf, DateLabel) that chart points and markers both render with.
package timerange

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Preset is one entry in the picker's quick-range list. The list lives here
// rather than as markup in the template because the server has to answer the
// same questions the popover does — which ranges exist, and what each is
// called — when it renders a picker from the URL.
type Preset struct {
	Range string
	Label string
}

var Presets = []Preset{
	{"7", "Last 7 days"},
	{"30", "Last 30 days"},
	{"90", "Last 90 days"},
	{"365", "Last year"},
	{"this-year", "This year"},
	{"all", "All time"},
}

func presetLabel(rangeParam string) (string, bool) {
	for _, p := range Presets {
		if p.Range == rangeParam {
			return p.Label, true
		}
	}
	return "", false
}

// customLabel is what a custom range's button reads before static/app.js
// initialises; the client immediately rewrites it from the bounds, which it
// can format far better (a bare date becomes "Jan 5, 2026", a relative
// expression stays verbatim).
const customLabel = "Custom range"

// maxBoundLen caps a custom bound coming off the URL. Bounds are free text —
// a date, "now-5d", "from+5d" — so length is the only thing worth asserting,
// and an unbounded one would otherwise be echoed into three attributes of
// the rendered page. Matches the cap parseRangeJSON applies to a pasted
// range in static/app.js.
const maxBoundLen = 64

// PickerConfig is the state of one instance of the shared
// "time-range-picker" template partial (see templates/time_range_picker.html)
// — the chart, the history filter, and the overnight filter each embed their
// own, with different defaults, since a chart benefits from a bounded
// default view but a list is more useful starting unfiltered.
type PickerConfig struct {
	Param   string // URL query parameter this instance reads and writes
	Default string // preset shown when the URL says nothing about it
	Range   string // the active preset value ("30", "all", ...), or "custom"
	Label   string // the button's text, matching Range
	From    string // custom bounds, empty unless Range is "custom"
	Until   string
	Presets []Preset // the popover's quick list
}

// Picker builds one picker's state from the URL, falling back to def when
// the range parameter is absent — so a bare "/" is every picker at its own
// default, and only a range that differs from it needs to appear in the URL
// at all.
//
// An unrecognized range falls back to def as well. Resolve treats anything
// it doesn't know as unbounded, so a hand-edited or truncated link would
// otherwise quietly show all time under a button still claiming something
// else — the same silent widening static/app.js refuses on a pasted range.
func Picker(param, def, rangeParam, fromParam, untilParam string) PickerConfig {
	cfg := PickerConfig{Param: param, Default: def, Range: def, Presets: Presets}
	if rangeParam == "custom" && (fromParam != "" || untilParam != "") &&
		len(fromParam) <= maxBoundLen && len(untilParam) <= maxBoundLen {
		cfg.Range, cfg.From, cfg.Until = "custom", fromParam, untilParam
		cfg.Label = customLabel
		return cfg
	}
	if _, ok := presetLabel(rangeParam); ok {
		cfg.Range = rangeParam
	}
	if label, ok := presetLabel(cfg.Range); ok {
		cfg.Label = label
	} else {
		cfg.Label = cfg.Range
	}
	return cfg
}

// Window resolves what the picker is actually showing. Rendering a page's
// content through this rather than through a separately-parsed set of query
// parameters is what keeps the content and the button that describes it from
// disagreeing.
func (c PickerConfig) Window(now time.Time) Window {
	return Resolve(c.Range, c.From, c.Until, now)
}

// Window is a resolved visible-range bound. Either side may be unbounded
// (HasFrom/HasUntil false) — both are for the "all time" preset, and either
// independently for a custom range where the user left that side blank.
type Window struct {
	From     time.Time
	HasFrom  bool
	Until    time.Time
	HasUntil bool
}

// Contains reports whether t falls within w.
func (w Window) Contains(t time.Time) bool {
	if w.HasFrom && t.Before(w.From) {
		return false
	}
	if w.HasUntil && t.After(w.Until) {
		return false
	}
	return true
}

// dateOnlyLayout matches a lone <input type="date"> value.
const dateOnlyLayout = "2006-01-02"

// Resolve turns the "range" query param into a Window:
//   - a preset day count ("7", "30", ...) is an inclusive trailing window
//     ending now
//   - "this-year" runs from midnight on 1 January
//   - "custom" reads the from/until params instead
//   - "all", or anything unrecognized, is unbounded
func Resolve(rangeParam, fromParam, untilParam string, today time.Time) Window {
	switch rangeParam {
	case "custom":
		return customWindow(fromParam, untilParam, today)
	case "this-year":
		start := time.Date(today.Year(), time.January, 1, 0, 0, 0, 0, today.Location())
		return Window{From: start, HasFrom: true}
	}
	days, err := strconv.Atoi(rangeParam)
	if err != nil || days <= 0 {
		return Window{}
	}
	y, m, d := today.Date()
	startOfToday := time.Date(y, m, d, 0, 0, 0, 0, today.Location())
	return Window{From: startOfToday.AddDate(0, 0, -days+1), HasFrom: true}
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
// other side's value too, so customWindow handles them separately.
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

// customWindow parses the "from"/"until" query params for a custom range.
// Each side is independently optional — blank or unparseable just leaves
// that end unbounded — and each accepts a date, a relative expression
// ("now-5d"), or a reference to the OTHER side ("to-5d" in from, "from+5d"
// in until), resolved once that other side's own value is known. So
// from=now-30d / until=from+5d and from=to-5d / until=now both work, but a
// side that only makes sense via the other — blank, itself a
// cross-reference, or otherwise unparseable — leaves that side unbounded:
// there is nothing to anchor a cross-reference to.
//
// Both ends are snapped to whole days: the start to 00:00 and the end to
// 23:59:59.999. A range is a span of dates here, not of instants, so every
// weigh-in on the named days is included — without this, "now-5d" run at
// 14:30 would silently drop that morning's entry five days ago, and "now"
// would drop this evening's. It does mean the sub-day units (s, m, h) round
// to the same day boundaries as everything else.
func customWindow(fromParam, untilParam string, now time.Time) Window {
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

	var w Window
	if fromOK {
		w.From, w.HasFrom = startOfDay(fromT), true
	}
	if untilOK {
		w.Until, w.HasUntil = endOfDay(untilT), true
	}
	return w
}

// MsOf converts t to Unix milliseconds, the x-axis unit Chart.js expects.
func MsOf(t time.Time) int64 {
	return t.UnixMilli()
}

// DateLabel formats t as the "Jan 2, 2006" label used in chart tooltips and
// marker points. The year is carried because these labels are read on the
// all-time chart, where two readings a year apart otherwise show the same
// "Jan 1" and nothing distinguishes them.
func DateLabel(t time.Time) string {
	return t.Format("Jan 2, 2006")
}
