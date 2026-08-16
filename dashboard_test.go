package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The app picks a Grafana panel by id: static/grafana-app.js maps its series
// select onto a base id and adds an offset for the raw/trend combination.
// Nothing at runtime notices if that id does not exist — Grafana just
// renders an empty panel, and the app looks broken for no visible reason.
// These tests keep the two files honest about the contract between them.

func readDashboardPanelIDs(t *testing.T) map[int]string {
	t.Helper()
	raw, err := os.ReadFile("grafana/dashboards/weight.json")
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	var dashboard struct {
		Panels []struct {
			ID    int    `json:"id"`
			Title string `json:"title"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatalf("parse dashboard: %v", err)
	}
	ids := make(map[int]string, len(dashboard.Panels))
	for _, p := range dashboard.Panels {
		if _, dup := ids[p.ID]; dup {
			t.Errorf("panel id %d is used twice; the app would get whichever Grafana picks", p.ID)
		}
		ids[p.ID] = p.Title
	}
	return ids
}

// readAppPanelConfig pulls the base ids and offsets straight out of the
// controller, so the test breaks if either side is edited alone.
func readAppPanelConfig(t *testing.T) (base map[string]int, rawAndTrend, trendOnly int, toggleable []string) {
	t.Helper()
	src, err := os.ReadFile("static/grafana-app.js")
	if err != nil {
		t.Fatalf("read controller: %v", err)
	}
	js := string(src)

	base = map[string]int{}
	for _, m := range regexp.MustCompile(`(?m)^\s*'?([a-z-]+)'?:\s*(\d+),`).FindAllStringSubmatch(js, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("parse panel id %q: %v", m[2], err)
		}
		base[m[1]] = n
	}
	if len(base) == 0 {
		t.Fatal("found no PANEL_IDS entries in static/grafana-app.js")
	}

	intConst := func(name string) int {
		m := regexp.MustCompile(name + `\s*=\s*(\d+)`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("could not find %s in static/grafana-app.js", name)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	rawAndTrend = intConst("RAW_AND_TREND_OFFSET")
	trendOnly = intConst("TREND_ONLY_OFFSET")

	m := regexp.MustCompile(`TOGGLEABLE\s*=\s*\[([^\]]+)\]`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("could not find TOGGLEABLE in static/grafana-app.js")
	}
	for _, s := range regexp.MustCompile(`'([a-z-]+)'`).FindAllStringSubmatch(m[1], -1) {
		toggleable = append(toggleable, s[1])
	}
	return base, rawAndTrend, trendOnly, toggleable
}

func TestDashboardHasEveryPanelTheAppCanRequest(t *testing.T) {
	panels := readDashboardPanelIDs(t)
	base, rawAndTrend, trendOnly, toggleable := readAppPanelConfig(t)

	isToggleable := map[string]bool{}
	for _, s := range toggleable {
		isToggleable[s] = true
	}

	for series, id := range base {
		// Every series must have its plain panel.
		want := map[int]string{id: series + " (raw only)"}
		if isToggleable[series] {
			want[id+rawAndTrend] = series + " (raw and trend)"
			want[id+trendOnly] = series + " (trend only)"
		}
		for panelID, what := range want {
			if _, ok := panels[panelID]; !ok {
				t.Errorf("app can request panel %d for %s but the dashboard has no such panel", panelID, what)
			}
		}
	}
}

func TestDashboardPanelsAreAllReachable(t *testing.T) {
	panels := readDashboardPanelIDs(t)
	base, rawAndTrend, trendOnly, toggleable := readAppPanelConfig(t)

	isToggleable := map[string]bool{}
	for _, s := range toggleable {
		isToggleable[s] = true
	}
	reachable := map[int]bool{}
	for series, id := range base {
		reachable[id] = true
		if isToggleable[series] {
			reachable[id+rawAndTrend] = true
			reachable[id+trendOnly] = true
		}
	}

	// A panel nothing can reach is dead weight in the dashboard, and usually
	// means a variant was added without wiring up the control for it.
	for id, title := range panels {
		if !reachable[id] {
			t.Errorf("dashboard panel %d (%q) cannot be reached from the app", id, title)
		}
	}
}

func TestDashboardQueriesUseSecondsNotMilliseconds(t *testing.T) {
	raw, err := os.ReadFile("grafana/dashboards/weight.json")
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	var dashboard struct {
		Panels []struct {
			ID      int `json:"id"`
			Targets []struct {
				QueryText string `json:"queryText"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatalf("parse dashboard: %v", err)
	}
	// Handed milliseconds, frser-sqlite-datasource overflows and plots the
	// series more than a century out. Every panel would look wrong while
	// every other test still passed.
	for _, p := range dashboard.Panels {
		for _, target := range p.Targets {
			if regexp.MustCompile(`\btime_ms\s+AS\s+time\b`).MatchString(target.QueryText) {
				t.Errorf("panel %d selects time_ms AS time; it must use time_s: %s", p.ID, target.QueryText)
			}
		}
	}
}

// A marker's line is coloured by which annotation query returned it, and its
// label in the app is coloured by the same index. Those two live in
// different files — the dashboard JSON and style.css — so nothing stops one
// being edited alone, and the symptom would be a line and its label
// disagreeing about colour, which is precisely the association this exists
// to provide.
func TestMarkerAnnotationColoursCoverEveryIndex(t *testing.T) {
	raw, err := os.ReadFile("grafana/dashboards/weight.json")
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	var dashboard struct {
		Annotations struct {
			List []struct {
				Name      string `json:"name"`
				IconColor string `json:"iconColor"`
				Target    struct {
					QueryText string `json:"queryText"`
				} `json:"target"`
			} `json:"list"`
		} `json:"annotations"`
	}
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatalf("parse dashboard: %v", err)
	}

	// v_markers.color_index is id % 4, so all four must be claimed or some
	// markers would draw no line at all.
	indexes := map[string]bool{}
	colours := map[string]string{}
	for _, a := range dashboard.Annotations.List {
		m := regexp.MustCompile(`color_index\s*=\s*(\d+)`).FindStringSubmatch(a.Target.QueryText)
		if m == nil {
			t.Errorf("annotation %q does not filter on color_index: %s", a.Name, a.Target.QueryText)
			continue
		}
		if indexes[m[1]] {
			t.Errorf("color_index %s is claimed by more than one annotation query", m[1])
		}
		indexes[m[1]] = true
		if a.IconColor == "" {
			t.Errorf("annotation %q has no iconColor, so its markers are indistinguishable", a.Name)
		}
		if prev, dup := colours[a.IconColor]; dup {
			t.Errorf("annotations %q and %q share colour %s, so their markers cannot be told apart", prev, a.Name, a.IconColor)
		}
		colours[a.IconColor] = a.Name
	}
	for i := range 4 {
		if !indexes[strconv.Itoa(i)] {
			t.Errorf("no annotation query covers color_index %d; markers with that index would draw no line", i)
		}
	}

	// The app needs a label colour for each of the same four indexes. Index
	// 0 uses the base .marker-legend-dot rule; 1-3 need their own.
	css, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read stylesheet: %v", err)
	}
	for i := 1; i < 4; i++ {
		rule := `.marker-legend-dot[data-color="` + strconv.Itoa(i) + `"]`
		if !regexp.MustCompile(regexp.QuoteMeta(rule)).Match(css) {
			t.Errorf("style.css has no %s rule, so those labels fall back to the wrong colour", rule)
		}
	}
}
