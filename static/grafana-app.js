// Drives the embedded Grafana panel in the trend card.
//
// Nothing here draws anything: the app owns the controls (which are far
// better on a phone than Grafana's own time picker in a narrow iframe) and
// Grafana owns the rendering. The controls' only job is to rewrite the
// iframe URL.
(function () {
	const frame = document.getElementById('graph-frame');
	if (!frame) return; // Grafana disabled, so the card renders an explanation instead

	const DASHBOARD = '/grafana/d-solo/weight-tracker/weight';

	// Each series is its own panel rather than one panel driven by a
	// template variable, because a panel's *type* cannot be switched by a
	// variable and the delta views are bar charts while the weight views
	// are lines. Swapping panelId is simpler than the alternative and needs
	// no dashboard variables at all.
	const PANEL_IDS = {
		all: 1,
		morning: 2,
		evening: 3,
		'morning-delta': 4,
		'evening-delta': 5,
	};

	// A panel's series are fixed in its JSON, so each combination of the two
	// toggles is its own panel (see grafana/dashboards/weight.json):
	//
	//   base +  0  raw only
	//   base + 10  raw and trend
	//   base + 20  trend only
	//
	// A dashboard variable would be tidier, but Grafana interpolates those in
	// its frontend where a script cannot check them, whereas a panel id is
	// verified against the dashboard JSON by
	// TestDashboardHasEveryPanelTheAppCanRequest.
	const RAW_AND_TREND_OFFSET = 10;
	const TREND_ONLY_OFFSET = 20;
	// The delta panels are bar charts of day-over-day change; neither a
	// rolling mean nor a "raw" variant means anything for them.
	const TOGGLEABLE = ['all', 'morning', 'evening'];

	const seriesSelect = document.getElementById('graph-series');
	const trendCheckbox = document.getElementById('graph-show-trend');
	const rawCheckbox = document.getElementById('graph-show-raw');
	const markerLegend = document.getElementById('marker-legend');
	const rangeInput = document.getElementById('graph-range-input');
	const fromInput = document.getElementById('graph-from-input');
	const untilInput = document.getElementById('graph-until-input');

	function currentTheme() {
		// Follow the app, which follows the OS. Grafana takes its theme as a
		// URL parameter, so this is re-read every time the URL is rebuilt.
		return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
	}

	// Grafana accepts either relative expressions ("now-30d") or epoch
	// milliseconds. Presets use the relative form so the window keeps
	// tracking as time passes; custom ranges and "all" need absolute bounds.
	// Returns what Grafana needs (from/to, possibly relative) alongside the
	// absolute millisecond bounds, which the marker legend needs in order to
	// hide markers that fall outside the visible window.
	function timeRange() {
		const range = rangeInput.value;
		const now = Date.now();
		if (range === 'custom') {
			const from = fromInput.value ? Date.parse(fromInput.value) : window.EARLIEST_MS;
			// An until date means the whole of that day.
			const to = untilInput.value ? Date.parse(untilInput.value) + 86399999 : now;
			return { from: from, to: to, fromMs: from, toMs: to };
		}
		if (range === 'all') {
			// Ask for exactly the span that holds data. Guessing something
			// wide instead would squash every point against the right edge.
			return { from: window.EARLIEST_MS, to: 'now', fromMs: window.EARLIEST_MS, toMs: now };
		}
		const days = Number(range);
		return {
			// Relative, so the window keeps tracking as time passes.
			from: 'now-' + range + 'd', to: 'now',
			fromMs: now - days * 86400000, toMs: now,
		};
	}

	// The vertical annotation lines and these labels are the same markers, so
	// the labels have to describe exactly what is on screen: only the markers
	// inside the range, in the same left-to-right order as their lines.
	// Listing all of them regardless is what made the lines hard to match up.
	function updateMarkerLegend(range) {
		if (!markerLegend) return;
		const items = Array.from(markerLegend.children);
		items.sort((a, b) => Number(a.dataset.ms) - Number(b.dataset.ms));
		let visible = 0;
		for (const item of items) {
			const ms = Number(item.dataset.ms);
			const inRange = ms >= range.fromMs && ms <= range.toMs;
			item.hidden = !inRange;
			if (inRange) visible++;
			// Re-appending in sorted order matches the on-chart ordering.
			markerLegend.appendChild(item);
		}
		markerLegend.hidden = visible === 0;
	}

	function rebuild() {
		const series = seriesSelect.value;
		let panelId = PANEL_IDS[series] || PANEL_IDS.all;
		if (TOGGLEABLE.includes(series)) {
			let showRaw = rawCheckbox ? rawCheckbox.checked : true;
			const showTrend = trendCheckbox ? trendCheckbox.checked : false;
			// Hiding both would leave an empty chart, so raw wins and the
			// checkbox is corrected to match — the UI should never claim a
			// state the graph is not in.
			if (!showRaw && !showTrend) {
				showRaw = true;
				if (rawCheckbox) rawCheckbox.checked = true;
			}
			if (showRaw && showTrend) {
				panelId += RAW_AND_TREND_OFFSET;
			} else if (showTrend) {
				panelId += TREND_ONLY_OFFSET;
			}
		}
		const range = timeRange();
		updateMarkerLegend(range);
		const params = new URLSearchParams({
			panelId: String(panelId),
			from: String(range.from),
			to: String(range.to),
			theme: currentTheme(),
			kiosk: '',
		});
		const next = DASHBOARD + '?' + params.toString();
		// Assigning an identical src reloads the iframe and makes the panel
		// visibly flicker, so only touch it when something actually changed.
		if (frame.getAttribute('src') !== next) {
			frame.setAttribute('src', next);
		}
	}

	seriesSelect.addEventListener('change', rebuild);
	if (trendCheckbox) trendCheckbox.addEventListener('change', rebuild);
	if (rawCheckbox) rawCheckbox.addEventListener('change', rebuild);

	// Follow the OS theme while the page is open, not just at load.
	const darkQuery = window.matchMedia('(prefers-color-scheme: dark)');
	darkQuery.addEventListener('change', rebuild);

	// --- time range picker -------------------------------------------------
	const rangeBtn = document.getElementById('graph-range-btn');
	const rangeLabel = document.getElementById('graph-range-label');
	const popover = document.getElementById('graph-range-popover');

	function closePopover() {
		popover.hidden = true;
		rangeBtn.setAttribute('aria-expanded', 'false');
	}

	rangeBtn.addEventListener('click', () => {
		const open = popover.hidden;
		popover.hidden = !open;
		rangeBtn.setAttribute('aria-expanded', String(open));
	});

	document.addEventListener('click', (event) => {
		if (!popover.hidden && !popover.contains(event.target) && event.target !== rangeBtn && !rangeBtn.contains(event.target)) {
			closePopover();
		}
	});

	popover.querySelectorAll('.time-range-preset').forEach((preset) => {
		preset.addEventListener('click', () => {
			popover.querySelectorAll('.time-range-preset').forEach((p) => p.classList.remove('active'));
			preset.classList.add('active');
			rangeInput.value = preset.dataset.range;
			fromInput.value = '';
			untilInput.value = '';
			rangeLabel.textContent = preset.dataset.label;
			closePopover();
			rebuild();
		});
	});

	document.getElementById('graph-custom-apply').addEventListener('click', () => {
		const from = document.getElementById('graph-custom-from').value;
		const until = document.getElementById('graph-custom-until').value;
		if (!from && !until) return;
		rangeInput.value = 'custom';
		fromInput.value = from;
		untilInput.value = until;
		popover.querySelectorAll('.time-range-preset').forEach((p) => p.classList.remove('active'));
		rangeLabel.textContent = (from || 'the beginning') + ' → ' + (until || 'now');
		closePopover();
		rebuild();
	});

	// Belt and braces with autocomplete="off" on the controls: browsers
	// restore form state across reloads, and the range *label* is plain text
	// that cannot be restored. If a restored value disagrees with the label,
	// the graph draws one window while the button names another. Deriving the
	// label from the value on load makes that impossible.
	function syncRangeLabel() {
		const current = rangeInput.value;
		if (current === 'custom') {
			rangeLabel.textContent = (fromInput.value || 'the beginning') + ' → ' + (untilInput.value || 'now');
			popover.querySelectorAll('.time-range-preset').forEach((p) => p.classList.remove('active'));
			return;
		}
		popover.querySelectorAll('.time-range-preset').forEach((preset) => {
			const match = preset.dataset.range === current;
			preset.classList.toggle('active', match);
			if (match) rangeLabel.textContent = preset.dataset.label;
		});
	}

	// The trend card is the first thing on the default tab, so the panel
	// loads straight away rather than waiting for an interaction.
	syncRangeLabel();
	rebuild();

	// A weigh-in, goal or marker changing means the panel is out of date.
	['entries-changed', 'goals-changed', 'markers-changed'].forEach((event) => {
		document.body.addEventListener(event, rebuild);
	});
})();
