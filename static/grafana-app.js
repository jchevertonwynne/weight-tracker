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

	const seriesSelect = document.getElementById('graph-series');
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
	function timeRange() {
		const range = rangeInput.value;
		if (range === 'custom') {
			const from = fromInput.value ? Date.parse(fromInput.value) : window.EARLIEST_MS;
			// An until date means the whole of that day, matching how the
			// app's own chart treats the custom range.
			const until = untilInput.value ? Date.parse(untilInput.value) + 86399999 : Date.now();
			return { from: from, to: until };
		}
		if (range === 'all') {
			// Ask for exactly the span that holds data. Guessing something
			// wide instead would squash every point against the right edge.
			return { from: window.EARLIEST_MS, to: 'now' };
		}
		return { from: 'now-' + range + 'd', to: 'now' };
	}

	function rebuild() {
		const panelId = PANEL_IDS[seriesSelect.value] || PANEL_IDS.all;
		const range = timeRange();
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

	// The trend card is the first thing on the default tab, so the panel
	// loads straight away rather than waiting for an interaction.
	rebuild();

	// A weigh-in, goal or marker changing means the panel is out of date.
	['entries-changed', 'goals-changed', 'markers-changed'].forEach((event) => {
		document.body.addEventListener(event, rebuild);
	});
})();
