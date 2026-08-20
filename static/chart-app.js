(function () {
	const canvas = document.getElementById('weight-chart');
	if (!canvas) return;
	const emptyEl = document.getElementById('chart-empty');
	const markerNoteEl = document.getElementById('marker-note');
	const form = document.getElementById('chart-controls');
	let chart = null;

	// showFailure puts the reason in the card instead of leaving a blank
	// rectangle. Every failure below used to be silent: the canvas kept its
	// size, nothing drew, and the only clue was a console message — which is
	// no clue at all on a phone.
	function showFailure(message) {
		canvas.hidden = true;
		if (emptyEl) {
			emptyEl.hidden = false;
			emptyEl.textContent = message;
		}
	}

	// Chart.register below runs at load and throws if the library is missing,
	// aborting this whole script — including the code that would have
	// reported the problem. Check first so that failure is visible.
	if (typeof Chart === 'undefined') {
		showFailure('Chart library failed to load. Reload the page; if it persists, check /static/chart.min.js.');
		return;
	}

	function cssVar(name) {
		return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
	}

	// parseDateInput reads a "2026-01-01" bound as a local date rather than
	// UTC midnight — new Date('2026-01-01') parses as UTC, which can display
	// as the previous day in negative UTC-offset zones.
	function parseDateInput(value) {
		const [y, m, d] = value.split('-').map(Number);
		return new Date(y, m - 1, d);
	}

	function formatDateLabel(date) {
		return date.toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
	}

	// Grafana-style time range picker: a single button showing the active
	// range, opening a popover with quick presets alongside a custom
	// from/until section — rather than a <select> plus a permanently-visible
	// (or awkwardly toggled) pair of date fields.
	const timeRangeBtn = document.getElementById('time-range-btn');
	const timeRangeLabel = document.getElementById('time-range-label');
	const timeRangePopover = document.getElementById('time-range-popover');
	const rangeInput = document.getElementById('range-input');
	const fromInput = document.getElementById('from-input');
	const untilInput = document.getElementById('until-input');
	const customFromInput = document.getElementById('custom-range-from');
	const customUntilInput = document.getElementById('custom-range-until');
	const customApplyBtn = document.getElementById('custom-range-apply');
	const presetButtons = Array.from(document.querySelectorAll('.time-range-preset'));

	// syncRangeLabel renders the button's label and the active preset from
	// the values that are actually submitted, rather than trusting them to
	// have been set together.
	//
	// The label is static text in the template but the range is a hidden
	// input, and browsers restore form-control state across a reload while
	// leaving the text alone. A restored "90" therefore left the button
	// still reading "Last 30 days" while the chart drew ninety — the control
	// lying about what it was showing. Deriving one from the other makes
	// that impossible, and autocomplete="off" on the controls stops the
	// restore happening in the first place.
	// customValuesFor writes a preset out in the same syntax the custom boxes
	// accept, so selecting one fills them in and tweaking an end becomes an
	// edit rather than a retype. "now" is the default end for every range:
	// it reads as the intent, and it keeps tracking rather than freezing on
	// the date the box happened to be filled.
	function customValuesFor(range) {
		if (range === 'all') {
			return { from: '', until: 'now' };
		}
		if (range === 'this-year') {
			return { from: `${new Date().getFullYear()}-01-01`, until: 'now' };
		}
		return { from: `now-${range}d`, until: 'now' };
	}

	// syncCustomInputs mirrors the active range into the custom boxes. A
	// custom range is left alone — the boxes are already the source of truth
	// for it, and rewriting them would fight the user's own text.
	function syncCustomInputs() {
		if (rangeInput.value === 'custom') return;
		const values = customValuesFor(rangeInput.value);
		customFromInput.value = values.from;
		customUntilInput.value = values.until;
	}

	// A custom bound is either a calendar date or a relative expression like
	// "now-5d". Only the date form is worth prettifying — a relative
	// expression already reads as what the user typed, and echoing it back
	// verbatim is also how a typo becomes visible, since the server treats an
	// unparseable bound as simply unbounded on that side.
	const dateOnlyBound = /^\d{4}-\d{2}-\d{2}$/;
	function formatBound(value) {
		return dateOnlyBound.test(value) ? formatDateLabel(parseDateInput(value)) : value;
	}

	function syncRangeLabel() {
		if (rangeInput.value === 'custom') {
			presetButtons.forEach((b) => b.classList.remove('active'));
			const from = fromInput.value;
			const until = untilInput.value;
			if (from && until) {
				timeRangeLabel.textContent = `${formatBound(from)} – ${formatBound(until)}`;
			} else if (from) {
				timeRangeLabel.textContent = `Since ${formatBound(from)}`;
			} else if (until) {
				timeRangeLabel.textContent = `Until ${formatBound(until)}`;
			}
			return;
		}
		presetButtons.forEach((btn) => {
			const match = btn.dataset.range === rangeInput.value;
			btn.classList.toggle('active', match);
			if (match) timeRangeLabel.textContent = btn.dataset.label;
		});
		syncCustomInputs();
	}

	function openTimeRangePopover() {
		timeRangePopover.hidden = false;
		timeRangeBtn.setAttribute('aria-expanded', 'true');
	}

	function closeTimeRangePopover() {
		timeRangePopover.hidden = true;
		timeRangeBtn.setAttribute('aria-expanded', 'false');
	}

	timeRangeBtn.addEventListener('click', (event) => {
		event.stopPropagation();
		if (timeRangePopover.hidden) {
			openTimeRangePopover();
		} else {
			closeTimeRangePopover();
		}
	});

	document.addEventListener('click', (event) => {
		if (!timeRangePopover.hidden && !event.composedPath().includes(timeRangePopover) && event.target !== timeRangeBtn) {
			closeTimeRangePopover();
		}
	});

	document.addEventListener('keydown', (event) => {
		if (event.key === 'Escape' && !timeRangePopover.hidden) closeTimeRangePopover();
	});

	presetButtons.forEach((btn) => {
		btn.addEventListener('click', () => {
			rangeInput.value = btn.dataset.range;
			fromInput.value = '';
			untilInput.value = '';
			syncRangeLabel();
			closeTimeRangePopover();
			refreshChart();
		});
	});

	customApplyBtn.addEventListener('click', () => {
		if (!customFromInput.value && !customUntilInput.value) return;
		rangeInput.value = 'custom';
		fromInput.value = customFromInput.value;
		untilInput.value = customUntilInput.value;
		syncRangeLabel();
		closeTimeRangePopover();
		refreshChart();
	});

	// markerLabelMaxWidth caps how wide a single marker's label is allowed to
	// be before it gets truncated with an ellipsis — keeps a long note from
	// crowding out its neighbors.
	const markerLabelMaxWidth = 90;
	// markerLabelGap is the minimum pixel gap required between two labels'
	// bounding boxes; anything tighter than this and they can't share a row.
	const markerLabelGap = 4;
	// markerLabelRowHeight is the vertical spacing between stacked label
	// rows (see markerLabelRows below).
	const markerLabelRowHeight = 12;
	// markerLabelRows bounds how many labels can stack vertically when their
	// dates are too close together to sit on one row — e.g. two markers 9
	// days apart on a 90-day view. A marker that doesn't fit in any row
	// still gets its line drawn and stays reachable via tap; it just goes
	// without an inline label.
	const markerLabelRows = 2;
	// markerPalette assigns each marker a distinct color (by id, so it's
	// stable across range changes) so its line, label, and tapped-note text
	// all visibly pair up — otherwise two markers close enough to stack
	// their labels are hard to tell apart.
	const markerPalette = ['--marker', '--marker-2', '--marker-3', '--marker-4'];
	function colorForMarker(id) {
		const idx = ((id % markerPalette.length) + markerPalette.length) % markerPalette.length;
		return cssVar(markerPalette[idx]);
	}

	// yearBoundaries returns {year, x} for each January 1 (in the browser's
	// local time zone, matching the x-axis tick labels) strictly between
	// minMs and maxMs — the boundary at the very edge of the visible range
	// isn't included since a line right at the chart's edge adds nothing.
	function yearBoundaries(minMs, maxMs) {
		const boundaries = [];
		const startYear = new Date(minMs).getFullYear();
		const endYear = new Date(maxMs).getFullYear();
		for (let year = startYear + 1; year <= endYear; year++) {
			const x = new Date(year, 0, 1).getTime();
			if (x >= minMs && x <= maxMs) boundaries.push({ year, x });
		}
		return boundaries;
	}

	// yearBoundariesPlugin draws a solid vertical line (distinct from
	// markers' dashed, colored lines) at each new-year boundary crossed by
	// the visible range, labeled with the year in its own row above all
	// marker label rows — years are rare enough (once per calendar year)
	// that they never need the markers' overlap/stacking logic. Drawn
	// beforeDatasetsDraw so it reads as background structure, not data.
	const yearBoundariesPlugin = {
		id: 'yearBoundaries',
		beforeDatasetsDraw(chartInstance) {
			const { ctx, chartArea, scales } = chartInstance;
			const boundaries = yearBoundaries(scales.x.min, scales.x.max);
			if (!boundaries.length) return;
			ctx.save();
			ctx.strokeStyle = cssVar('--year-boundary');
			ctx.fillStyle = cssVar('--year-boundary');
			ctx.lineWidth = 1;
			ctx.font = '10px "Roboto", "Segoe UI", system-ui, -apple-system, sans-serif';
			ctx.textAlign = 'center';
			ctx.textBaseline = 'bottom';
			const labelY = chartArea.top - 6 - markerLabelRows * markerLabelRowHeight;
			boundaries.forEach(({ year, x: boundaryMs }) => {
				const x = scales.x.getPixelForValue(boundaryMs);
				if (x < chartArea.left || x > chartArea.right) return;
				ctx.beginPath();
				ctx.moveTo(x, chartArea.top);
				ctx.lineTo(x, chartArea.bottom);
				ctx.stroke();
				ctx.fillText(String(year), x, labelY);
			});
			ctx.restore();
		},
	};
	Chart.register(yearBoundariesPlugin);

	// markerHitThreshold is how close (in pixels) the pointer needs to be to
	// a marker's line for it to count as hovered/clicked — shared by the
	// hover highlight and the click-to-reveal handler so "looks clickable"
	// and "is clickable" always agree.
	const markerHitThreshold = 15;

	function findNearestMarker(chartInstance, pos) {
		if (!chartInstance || !chartInstance.$markerPositions) return null;
		return chartInstance.$markerPositions.find((m) => Math.abs(m.x - pos.x) < markerHitThreshold) || null;
	}

	// truncateToWidth shortens text with a trailing ellipsis until it fits
	// within maxWidth for the canvas context's current font, via binary
	// search over the cut point. Returns '' if even a single character plus
	// the ellipsis doesn't fit.
	function truncateToWidth(ctx, text, maxWidth) {
		if (ctx.measureText(text).width <= maxWidth) return text;
		let lo = 0;
		let hi = text.length;
		while (lo < hi) {
			const mid = (lo + hi + 1) >> 1;
			if (ctx.measureText(text.slice(0, mid) + '…').width <= maxWidth) {
				lo = mid;
			} else {
				hi = mid - 1;
			}
		}
		return lo === 0 ? '' : text.slice(0, lo) + '…';
	}

	// markerLinesPlugin draws a dashed vertical line for each marker date,
	// spanning the full plot height, plus a short truncated label above the
	// plot area (in the top layout padding reserved for it) so markers are
	// readable at a glance rather than only on tap. Labels are placed
	// left-to-right, stacking onto a further-up row (see markerLabelRows)
	// when they'd otherwise overlap a label already on the current row;
	// anything that doesn't fit any row is still readable via tap, using
	// the pixel positions recorded in chart.$markerPositions. Whichever
	// marker is currently hovered (chart.$hoveredMarkerId, kept in sync by
	// the mousemove listener below) is drawn thicker/bolder, so it's
	// obvious *before* clicking that a marker is about to be selected.
	const markerLinesPlugin = {
		id: 'markerLines',
		afterDatasetsDraw(chartInstance) {
			const markers = (chartInstance.options.plugins.markerLines || {}).markers || [];
			chartInstance.$markerPositions = [];
			if (!markers.length) return;
			const { ctx, chartArea, scales } = chartInstance;
			const hoveredId = chartInstance.$hoveredMarkerId;
			ctx.save();
			ctx.setLineDash([3, 3]);
			const positioned = [];
			markers.forEach((m) => {
				const x = scales.x.getPixelForValue(m.x);
				if (x < chartArea.left || x > chartArea.right) return;
				const hovered = m.id === hoveredId;
				const color = colorForMarker(m.id);
				ctx.strokeStyle = color;
				ctx.lineWidth = hovered ? 2.5 : 1;
				ctx.beginPath();
				ctx.moveTo(x, chartArea.top);
				ctx.lineTo(x, chartArea.bottom);
				ctx.stroke();
				positioned.push({ id: m.id, x, date: m.date, note: m.note, color });
			});
			chartInstance.$markerPositions = positioned;

			ctx.setLineDash([]);
			ctx.textAlign = 'center';
			ctx.textBaseline = 'bottom';
			// Each row tracks the right edge of the last label placed on it;
			// a label goes on the first row it doesn't overlap, so two
			// close-together markers still both get a legible label instead
			// of the second one silently disappearing.
			const rowRightEdge = new Array(markerLabelRows).fill(-Infinity);
			positioned
				.slice()
				.sort((a, b) => a.x - b.x)
				.forEach((m) => {
					const hovered = m.id === hoveredId;
					ctx.font = (hovered ? 'bold ' : '') + '10px "Roboto", "Segoe UI", system-ui, -apple-system, sans-serif';
					const label = truncateToWidth(ctx, m.note, markerLabelMaxWidth);
					if (!label) return;
					const width = ctx.measureText(label).width;
					const left = m.x - width / 2;
					const row = rowRightEdge.findIndex((edge) => left >= edge + markerLabelGap);
					if (row === -1) return;
					ctx.fillStyle = m.color;
					ctx.fillText(label, m.x, chartArea.top - 6 - row * markerLabelRowHeight);
					rowRightEdge[row] = m.x + width / 2;
				});
			ctx.restore();
		},
	};
	Chart.register(markerLinesPlugin);

	function colorFor(key) {
		switch (key) {
			case 'morning': return cssVar('--morning');
			case 'evening': return cssVar('--evening');
			case 'loss': return cssVar('--loss');
			case 'gain': return cssVar('--gain');
			default: return cssVar('--on-surface-muted');
		}
	}

	// pointRadiusFor shrinks the marker size as point count grows, so a
	// year of daily entries doesn't render as an unreadable wall of dots.
	// pointHoverRadius stays constant since Chart.js's nearest-point
	// interaction mode doesn't depend on the visible marker size for
	// tap/hover accuracy.
	function pointRadiusFor(count) {
		if (count > 200) return 1;
		if (count > 90) return 2;
		if (count > 30) return 3;
		return 4;
	}

	// lineWidthFor thins the raw-data connecting line as point count grows —
	// a thick line over a year of daily ups and downs visually obscures the
	// actual trend more than it helps.
	function lineWidthFor(count) {
		if (count > 200) return 0.75;
		if (count > 90) return 1;
		if (count > 30) return 1.5;
		return 2;
	}

	function buildConfig(data) {
		const trendAvailable = !data.isBar && data.trend && data.trend.length >= 2;
		const rawCheckbox = form.elements['show-raw'];
		const trendCheckbox = form.elements['show-trend'];
		let showRaw = rawCheckbox.checked;
		const showTrend = trendCheckbox.checked && trendAvailable;

		// Never let both the raw series and the trend line be hidden at
		// once — fall back to raw data, and reflect that back into the
		// checkbox so the UI doesn't silently disagree with what's shown.
		if (!showRaw && !showTrend) {
			showRaw = true;
			rawCheckbox.checked = true;
		}

		const datasets = [];
		if (data.isBar) {
			if (showRaw) {
				datasets.push({
					type: 'bar',
					label: 'Delta',
					data: data.points,
					backgroundColor: data.points.map((p) => colorFor(p.color)),
					borderRadius: 3,
					maxBarThickness: 24,
				});
			}
		} else {
			if (showRaw) {
				const morningPoints = data.points.filter((p) => p.color === 'morning');
				const eveningPoints = data.points.filter((p) => p.color === 'evening');
				if (morningPoints.length && eveningPoints.length) {
					// Both periods are present (the "all" series) — one line
					// per period, rather than a single line connecting
					// alternating morning/evening readings directly to each
					// other, which zig-zags between the two instead of
					// showing either trend on its own.
					datasets.push({
						type: 'line',
						label: 'Morning',
						data: morningPoints,
						borderColor: cssVar('--morning'),
						borderWidth: lineWidthFor(morningPoints.length),
						pointBackgroundColor: cssVar('--morning'),
						pointRadius: pointRadiusFor(morningPoints.length),
						pointHoverRadius: 6,
						tension: 0,
					});
					datasets.push({
						type: 'line',
						label: 'Evening',
						data: eveningPoints,
						borderColor: cssVar('--evening'),
						borderWidth: lineWidthFor(eveningPoints.length),
						pointBackgroundColor: cssVar('--evening'),
						pointRadius: pointRadiusFor(eveningPoints.length),
						pointHoverRadius: 6,
						tension: 0,
					});
				} else {
					datasets.push({
						type: 'line',
						label: 'Weight',
						data: data.points,
						borderColor: cssVar('--chart-line'),
						borderWidth: lineWidthFor(data.points.length),
						pointBackgroundColor: data.points.map((p) => colorFor(p.color)),
						pointRadius: pointRadiusFor(data.points.length),
						pointHoverRadius: 6,
						tension: 0,
					});
				}
			}
			if (showTrend) {
				datasets.push({
					type: 'line',
					label: 'Trend',
					data: data.trend,
					borderColor: cssVar('--primary'),
					borderWidth: 2.5,
					pointRadius: 0,
					tension: 0.2,
				});
			}
			if (data.goals && data.goals.length >= 2) {
				datasets.push({
					type: 'line',
					label: 'Goal',
					data: data.goals,
					borderColor: cssVar('--goal'),
					borderDash: [6, 3],
					borderWidth: 1.5,
					pointRadius: 0,
					tension: 0,
				});
			}
		}

		return {
			type: data.isBar ? 'bar' : 'line',
			data: { datasets },
			options: {
				responsive: true,
				animation: false,
				interaction: { mode: 'nearest', intersect: false, axis: 'x' },
				// Reserves room above the plot area for markerLinesPlugin's
				// (possibly stacked, see markerLabelRows) labels plus one
				// more row above those for yearBoundariesPlugin's year
				// labels, so neither ever overlaps the topmost data points.
				layout: { padding: { top: 18 + markerLabelRows * markerLabelRowHeight } },
				scales: {
					x: {
						type: 'linear',
						min: data.xMin,
						max: data.xMax,
						grid: { color: cssVar('--chart-grid') },
						ticks: {
							color: cssVar('--on-surface-muted'),
							callback: (value) => new Date(value).toLocaleDateString(undefined, { month: 'short', day: 'numeric' }),
						},
					},
					y: {
						beginAtZero: data.isBar,
						grace: data.isBar ? undefined : '10%',
						grid: { color: cssVar('--chart-grid') },
						ticks: {
							color: cssVar('--on-surface-muted'),
							callback: (value) => value.toFixed(1) + ' kg',
						},
					},
				},
				plugins: {
					legend: { display: false },
					tooltip: {
						callbacks: {
							title: (items) => (items[0] && items[0].raw.date) || '',
							label: (item) => (item.raw && item.raw.value) || item.formattedValue,
						},
					},
					markerLines: { markers: data.markers || [] },
				},
			},
		};
	}

	function renderChart(data) {
		if (chart) {
			chart.destroy();
			chart = null;
		}
		if (markerNoteEl) markerNoteEl.hidden = true;
		if (!data.hasData) {
			canvas.hidden = true;
			if (emptyEl) {
				emptyEl.hidden = false;
				emptyEl.textContent = data.empty;
			}
			return;
		}
		canvas.hidden = false;
		if (emptyEl) emptyEl.hidden = true;
		canvas.style.cursor = '';
		try {
			chart = new Chart(canvas, buildConfig(data));
		} catch (err) {
			console.error('chart build failed', err);
			showFailure('Could not draw the chart: ' + (err && err.message ? err.message : err));
		}
	}

	function refreshChart() {
		const params = new URLSearchParams(new FormData(form));
		fetch('/chart?' + params.toString())
			.then((res) => {
				if (!res.ok) throw new Error('server returned ' + res.status);
				return res.json();
			})
			.then(renderChart)
			.catch((err) => {
				console.error('chart refresh failed', err);
				showFailure('Could not load chart data: ' + (err && err.message ? err.message : err));
			});
	}

	canvas.addEventListener('click', (event) => {
		if (!chart || !chart.$markerPositions || !chart.$markerPositions.length || !markerNoteEl) return;
		const pos = Chart.helpers.getRelativePosition(event, chart);
		const nearest = findNearestMarker(chart, pos);
		if (nearest) {
			markerNoteEl.hidden = false;
			markerNoteEl.style.color = nearest.color;
			markerNoteEl.textContent = nearest.date + ': ' + nearest.note;
		} else {
			markerNoteEl.hidden = true;
		}
	});

	// Hovering near a marker's line switches the cursor to a pointer and
	// redraws that marker's line/label bolder, so it's clear *before*
	// clicking that doing so will select it — previously the only way to
	// find that out was to click around and see what happened.
	canvas.addEventListener('mousemove', (event) => {
		if (!chart || !chart.$markerPositions || !chart.$markerPositions.length) return;
		const pos = Chart.helpers.getRelativePosition(event, chart);
		const nearest = findNearestMarker(chart, pos);
		const hoveredId = nearest ? nearest.id : null;
		if (chart.$hoveredMarkerId === hoveredId) return;
		chart.$hoveredMarkerId = hoveredId;
		canvas.style.cursor = hoveredId === null ? '' : 'pointer';
		chart.draw();
	});

	canvas.addEventListener('mouseleave', () => {
		if (!chart || chart.$hoveredMarkerId == null) return;
		chart.$hoveredMarkerId = null;
		canvas.style.cursor = '';
		chart.draw();
	});

	form.addEventListener('change', refreshChart);
	document.body.addEventListener('entries-changed', refreshChart);
	document.body.addEventListener('goals-changed', refreshChart);
	document.body.addEventListener('markers-changed', refreshChart);
	window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', refreshChart);

	syncRangeLabel();
	refreshChart();
})();
