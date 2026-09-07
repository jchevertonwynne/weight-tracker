(function () {
	const canvas = document.getElementById('weight-chart');
	if (!canvas) return;
	const emptyEl = document.getElementById('chart-empty');
	const markerNoteEl = document.getElementById('marker-note');
	const markerLegendEl = document.getElementById('marker-legend');
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

	// The time-range-picker itself (button, popover, presets, custom
	// from/until) is a shared component initialized once per instance by
	// static/app.js — see initTimeRangePicker there. It fires a bubbling
	// 'change' event on its hidden range input when applied, which the
	// form.addEventListener('change', refreshChart) below picks up like any
	// other control change; this file doesn't need to know it exists.

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

// Time-axis ticks that land on calendar boundaries rather than on round
// numbers of milliseconds, which is what Chart.js does by default and means
// nothing to a reader — a 30-day view came out labelled 9, 6, 6 and 8 days
// apart.
//
// The unit is chosen to suit the span: a fortnight gets days, a couple of
// months gets weeks, a year gets quarters. Ticks then sit on the first of
// the month, or a Monday, so a label is a date someone can reason about
// instead of an arbitrary offset from wherever the data happens to begin.
//
// Finest-first, so the axis carries as much detail as fits rather than
// defaulting to the coarsest unit that technically works.
const tickScales = [
	{ unit: 'day', steps: [1, 2] },
	{ unit: 'week', steps: [1, 2] },
	{ unit: 'month', steps: [1, 2, 3, 6] },
	{ unit: 'year', steps: [1, 2, 5] },
];
// What fits across a phone without the labels colliding.
const maxTimeTicks = 5;
function startOfUnit(ms, unit) {
	const d = new Date(ms);
	d.setHours(0, 0, 0, 0);
	if (unit === 'week') {
		// Weeks start Monday; getDay() is Sunday-based.
		d.setDate(d.getDate() - ((d.getDay() + 6) % 7));
	} else if (unit === 'month') {
		d.setDate(1);
	} else if (unit === 'year') {
		d.setMonth(0, 1);
	}
	return d;
}

// Calendar arithmetic, not fixed millisecond offsets, so months of
// different lengths and DST changes don't drift the ticks off their
// boundaries.
function advanceUnit(date, unit, step) {
	const d = new Date(date);
	if (unit === 'day') d.setDate(d.getDate() + step);
	else if (unit === 'week') d.setDate(d.getDate() + 7 * step);
	else if (unit === 'month') d.setMonth(d.getMonth() + step);
	else d.setFullYear(d.getFullYear() + step);
	return d;
}

function boundariesBetween(min, max, unit, step) {
	const out = [];
	let d = startOfUnit(min, unit);
	if (d.getTime() < min) d = advanceUnit(d, unit, step);
	// The cap is a guard against a pathological range spinning this loop,
	// not a display choice — anything over the limit is rejected anyway.
	while (d.getTime() <= max && out.length <= 64) {
		out.push(d.getTime());
		d = advanceUnit(d, unit, step);
	}
	return out;
}

// chooseTickScale returns the finest unit/step whose boundaries fit within
// maxTimeTicks, so the axis carries as much detail as it has room for.
function chooseTickScale(min, max) {
	for (const scale of tickScales) {
		for (const step of scale.steps) {
			if (boundariesBetween(min, max, scale.unit, step).length <= maxTimeTicks) {
				return { unit: scale.unit, step };
			}
		}
	}
	return { unit: 'year', step: 5 };
}

// axisMinFor nudges the left edge back to a calendar boundary when the
// first reading falls on the same day as one.
//
// Without it, a boundary a few hours before the first reading is simply
// outside the axis and never drawn: readings start 1 Jan at 11:02, the
// 1 Jan boundary is at 00:00, and the all-time axis lost its 1 Jan label
// despite the data starting precisely there.
//
// Bounded to the same calendar day, so this can only ever add a few hours
// of empty space, and only when the data genuinely reaches that boundary.
// A range starting on 3 Jan gains nothing, which is correct — 1 Jan is
// outside it.
function axisMinFor(min, max) {
	if (!Number.isFinite(min) || !Number.isFinite(max) || max <= min) return min;
	const { unit } = chooseTickScale(min, max);
	const boundary = startOfUnit(min, unit).getTime();
	const sameDay = new Date(boundary).toDateString() === new Date(min).toDateString();
	return boundary < min && sameDay ? boundary : min;
}

function calendarTicks(axis) {
	const { min, max } = axis;
	if (!Number.isFinite(min) || !Number.isFinite(max) || max <= min) return;

	const { unit, step } = chooseTickScale(min, max);
	const chosen = boundariesBetween(min, max, unit, step);

	// Only the calendar boundaries get labelled — the range's own start and
	// end are not pinned. This is how Grafana's time axis behaves, and the
	// reason is that pinning them fights the boundaries: an end sitting a few
	// days from the first of the month means one of the two has to go, and
	// dropping the boundary is what made a 90-day view skip 1 Jun. Leaving
	// the ends off keeps every label on a date worth reading and evenly
	// spaced; the range itself is already named on the picker above.
	//
	// The exception is a span too short to contain any boundary at all, where
	// the ends are better than an axis with no labels.
	axis.ticks = (chosen.length ? chosen : [min, max]).map((value) => ({ value }));
}

	function buildConfig(data) {
		const hasSplitTrend = (data.trendMorning && data.trendMorning.length >= 2) || (data.trendEvening && data.trendEvening.length >= 2);
		const trendAvailable = !data.isBar && ((data.trend && data.trend.length >= 2) || hasSplitTrend);
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
				if (data.trendMorning && data.trendMorning.length >= 2) {
					// Colored to match its raw line (same hue, no points, a
					// thicker smoothed curve) so it's clear which period's
					// trend it is, rather than one line blending both
					// periods' readings together.
					datasets.push({
						type: 'line',
						label: 'Morning trend',
						data: data.trendMorning,
						borderColor: cssVar('--morning'),
						borderWidth: 2.5,
						pointRadius: 0,
						tension: 0.2,
					});
				}
				if (data.trendEvening && data.trendEvening.length >= 2) {
					datasets.push({
						type: 'line',
						label: 'Evening trend',
						data: data.trendEvening,
						borderColor: cssVar('--evening'),
						borderWidth: 2.5,
						pointRadius: 0,
						tension: 0.2,
					});
				}
				if (data.trend && data.trend.length >= 2) {
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
						// Extended to a calendar boundary when the first
						// reading sits on one; see axisMinFor.
						min: axisMinFor(data.xMin, data.xMax),
						max: data.xMax,
						grid: { color: cssVar('--chart-grid') },
						// See calendarTicks: ticks land on calendar
						// boundaries in a unit suited to the span, rather than
						// on round numbers of milliseconds.
						afterBuildTicks: calendarTicks,
						ticks: {
							color: cssVar('--on-surface-muted'),
							// The tick list is already the intended one; letting
							// Chart.js thin it would drop the pinned end dates.
							autoSkip: false,
							maxRotation: 0,
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
							// Raw weigh-ins arrive with their date and value
							// preformatted by the server. The trend and goal
							// series are plain {x, y}, so they had neither: the
							// tooltip lost its date entirely once raw data was
							// hidden, and the value showed as a bare number.
							// Both fall back to deriving it from the point.
							title: (items) => {
								const point = items[0] && items[0].raw;
								if (!point) return '';
								// Matches timerange.DateLabel, which formats the
								// raw points server-side.
								return point.date || new Date(point.x).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
							},
							label: (item) => {
								const value = (item.raw && item.raw.value) || `${Number(item.parsed.y).toFixed(1)} kg`;
								// Naming the series matters once more than one
								// is on the chart — otherwise a weigh-in, its
								// trend and the goal are three unlabelled
								// numbers.
								const name = item.dataset && item.dataset.label;
								return name ? `${name}: ${value}` : value;
							},
						},
					},
					markerLines: { markers: data.markers || [] },
				},
			},
		};
	}

	// renderMarkerLegend lists every marker in view, in date order, each dot
	// the colour of its own line on the chart. The inline labels the chart
	// draws are dropped whenever dates crowd together, and on a touch screen
	// reading one otherwise means tapping its line — this is always there and
	// always complete.
	function renderMarkerLegend(markers) {
		if (!markerLegendEl) return;
		markerLegendEl.replaceChildren();
		const list = (markers || []).slice().sort((a, b) => a.x - b.x);
		markerLegendEl.hidden = list.length === 0;
		list.forEach((m) => {
			const item = document.createElement('li');
			const dot = document.createElement('span');
			dot.className = 'marker-legend-dot';
			dot.style.background = colorForMarker(m.id);
			dot.setAttribute('aria-hidden', 'true');
			item.appendChild(dot);
			item.appendChild(document.createTextNode(`${m.date}: ${m.note}`));
			markerLegendEl.appendChild(item);
		});
	}

	function renderChart(data) {
		if (chart) {
			chart.destroy();
			chart = null;
		}
		if (markerNoteEl) markerNoteEl.hidden = true;
		renderMarkerLegend(data.hasData ? data.markers : []);
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

	refreshChart();
})();

// Overnight tab's "Range by timescale" chart: one column per fixed window
// (7d/30d/90d/1y) the server has data for, spanning the smallest to the
// largest overnight change seen in that window and shaded green at the safe
// end through to red at the risky one.
//
// It was a box plot — ±1 SD box, min/max whiskers, a tick at the mean — and
// the shape lied about what the numbers mean. A box centred on the mean
// invites you to read the middle as the value to aim for, when the only
// figure with a real guarantee behind it is the safe end: land there and
// every night you have logged would have made the target. The odds decay
// continuously from there, so the chart is now continuous too, and no single
// point on it is nominated as the answer.
//
// Also drives the "Will I make it?" calculator below it: given tonight's
// actual weight, project the plausible morning-weight range per window and
// compare it against the currently active goal (if any) — both pull from
// the same fetched data, so the chart and the calculator always show the
// same set of timescales.
//
// The canvas sits outside #overnight-content, the fragment the range filter
// swaps, because none of this depends on that filter. Elements are still
// looked up fresh on every refresh and any previous Chart instance torn down
// first: htmx swaps elsewhere on the page are what this listens to, and
// caching a node across one is how you end up drawing into a canvas that was
// detached from the document.
(function () {
	if (typeof Chart === 'undefined') return;

	function cssVar(name) {
		return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
	}

	// columnGradient paints one window's column from green at its best
	// recorded night to red at its worst, with the amber stop placed at the
	// mean rather than halfway up: the mean is the coin flip, not a value to
	// aim for, and putting it at the geometric middle would imply the odds
	// change evenly across the column when they don't.
	//
	// Bottom-to-top is safe-to-risky in both modes. Projected, a lower
	// bedtime weight always makes the target easier; un-projected, a bigger
	// overnight loss always leaves more headroom.
	function columnGradient(chartInstance, p) {
		const { ctx, scales } = chartInstance;
		const amber = cssVar('--borderline');
		const yBottom = scales.y.getPixelForValue(p.minKg);
		const yTop = scales.y.getPixelForValue(p.maxKg);
		// One night logged collapses min/mean/max onto each other: there is no
		// span to fade across and no basis for calling it safe or risky, so it
		// gets the flat "could go either way" colour.
		if (!(yBottom > yTop)) return amber;
		const gradient = ctx.createLinearGradient(0, yBottom, 0, yTop);
		gradient.addColorStop(0, cssVar('--loss'));
		const meanStop = (p.meanKg - p.minKg) / (p.maxKg - p.minKg);
		if (meanStop > 0 && meanStop < 1) gradient.addColorStop(meanStop, amber);
		gradient.addColorStop(1, cssVar('--gain'));
		return gradient;
	}

	// overnightRangeBandsPlugin extends each column past the range it was
	// actually built from — solid green below the best night on record, solid
	// red above the worst — and draws the target line. All three are things a
	// bar dataset can't express: the bar stops at the data, but the verdict
	// doesn't. Registered once at load, same as yearBoundariesPlugin above;
	// which points to draw is passed fresh through plugin options on every
	// render, the same way markerLines below takes `markers` from
	// `data.markers`.
	const overnightRangeBandsPlugin = {
		id: 'overnightRangeBands',

		// Everything below a column is at least as good as the column, and
		// everything above it is worse — a bar that simply stops at the lowest
		// and highest night on record says neither, and reads instead as a band
		// you have to land inside. These bands carry the gradient's meaning out
		// to the edges of the plot: at or below the green, every night you have
		// logged would have made the target; up in the red, none of them would.
		// Drawn in beforeDatasetsDraw so the columns stay legible on top.
		//
		// Only in projected mode: without a target there is no making or
		// missing anything, so there is no verdict to extend.
		beforeDatasetsDraw(chartInstance, _args, opts) {
			const { points, targetKg } = opts;
			if (!points || !points.length) return;
			if (typeof targetKg !== 'number' || !Number.isFinite(targetKg)) return;
			const { ctx, chartArea, scales } = chartInstance;
			const meta = chartInstance.getDatasetMeta(0);
			ctx.save();
			points.forEach((p, i) => {
				const element = meta && meta.data && meta.data[i];
				const halfWidth = ((element && element.width) || 40) / 2;
				const x = scales.x.getPixelForValue(i);
				const ySafe = scales.y.getPixelForValue(p.minKg);
				ctx.fillStyle = cssVar('--loss-zone');
				ctx.fillRect(x - halfWidth, ySafe, halfWidth * 2, chartArea.bottom - ySafe);
				const yMiss = scales.y.getPixelForValue(p.maxKg);
				ctx.fillStyle = cssVar('--gain-zone');
				ctx.fillRect(x - halfWidth, chartArea.top, halfWidth * 2, yMiss - chartArea.top);
			});
			ctx.restore();
		},

		// Two overlays on top of the columns: a black line at each window's
		// average night, and the target line.
		//
		// The average sits exactly where the gradient turns amber, so the
		// line names the colour rather than adding a claim of its own — it is
		// the coin flip, not somewhere to aim. Black because it is only ever
		// drawn across a saturated column, where it reads in either theme.
		//
		// The target line, once the columns are projected onto bedtime
		// weight, is what makes the axis mean anything: the distance from a
		// column down to it *is* the overnight loss that column assumes.
		// Dashed in --goal to match the goal line on the main chart, so the
		// two read as the same kind of reference.
		afterDatasetsDraw(chartInstance, _args, opts) {
			const { points, targetKg } = opts;
			const { ctx, chartArea, scales } = chartInstance;
			const meta = chartInstance.getDatasetMeta(0);

			if (points && points.length) {
				ctx.save();
				ctx.strokeStyle = '#000000';
				ctx.lineWidth = 2;
				points.forEach((p, i) => {
					const element = meta && meta.data && meta.data[i];
					const halfWidth = ((element && element.width) || 40) / 2;
					const x = scales.x.getPixelForValue(i);
					const yMean = scales.y.getPixelForValue(p.meanKg);
					ctx.beginPath();
					ctx.moveTo(x - halfWidth, yMean);
					ctx.lineTo(x + halfWidth, yMean);
					ctx.stroke();
				});
				ctx.restore();
			}

			if (typeof targetKg !== 'number' || !Number.isFinite(targetKg)) return;
			const y = scales.y.getPixelForValue(targetKg);
			ctx.save();
			ctx.strokeStyle = cssVar('--goal');
			ctx.lineWidth = 1.5;
			ctx.setLineDash([6, 3]);
			ctx.beginPath();
			ctx.moveTo(chartArea.left, y);
			ctx.lineTo(chartArea.right, y);
			ctx.stroke();
			ctx.setLineDash([]);

			ctx.fillStyle = cssVar('--goal');
			ctx.font = '10px "Roboto", "Segoe UI", system-ui, -apple-system, sans-serif';
			ctx.textAlign = 'right';
			// Sits above the line, unless the line is close enough to the top
			// that the label would be clipped by the plot area.
			const above = y - 4 > chartArea.top + 10;
			ctx.textBaseline = above ? 'bottom' : 'top';
			ctx.fillText(`Target ${targetKg.toFixed(1)} kg`, chartArea.right - 4, above ? y - 4 : y + 4);
			ctx.restore();
		},
	};
	Chart.register(overnightRangeBandsPlugin);

	let chart = null;
	let cachedData = null; // last successful /overnight/windows fetch

	// visiblePoints keeps only windows that actually have at least one pair —
	// a window with zero pairs has no meaningful mean/min/max to plot or
	// project from, so it's dropped rather than drawn as a degenerate
	// zero-height box at 0kg. Which windows exist at all is the server's
	// call: a 90-day window over ten days of data repeats the 30-day one's
	// figures, so /overnight/windows leaves it out rather than the client
	// filtering it here.
	function visiblePoints(data) {
		return data.points.filter((p) => p.count > 0);
	}

	// projectOntoTarget shifts each point's overnight-change figures
	// (in delta-kg, e.g. "-2.0 to -1.0") onto an absolute bedtime-weight
	// scale anchored at targetKg (the desired morning weight): bedtime
	// weight = target - delta, since a negative (loss) delta means you can
	// weigh correspondingly more at bedtime. That subtraction flips which
	// edge is "low" vs "high" — the smallest loss (highKg/maxKg, less
	// negative) becomes the smallest bedtime allowance, and the largest
	// loss (lowKg/minKg) becomes the largest — so each pair is re-sorted
	// with Math.min/max rather than assumed to keep its original order.
	function projectOntoTarget(points, targetKg) {
		return points.map((p) => {
			const mean = targetKg - p.meanKg;
			const sdA = targetKg - p.lowKg;
			const sdB = targetKg - p.highKg;
			const extremeA = targetKg - p.minKg;
			const extremeB = targetKg - p.maxKg;
			return {
				label: p.label,
				name: p.name,
				count: p.count,
				hasRange: p.hasRange,
				meanKg: mean,
				meanLabel: mean.toFixed(1) + ' kg',
				lowKg: Math.min(sdA, sdB),
				highKg: Math.max(sdA, sdB),
				minKg: Math.min(extremeA, extremeB),
				maxKg: Math.max(extremeA, extremeB),
			};
		});
	}

	// currentTargetKg reads the "Weigh-in calculator" target field, if it
	// holds a usable positive number.
	function currentTargetKg() {
		const input = document.getElementById('overnight-calc-target');
		if (!input) return null;
		const value = parseFloat(input.value);
		return !Number.isNaN(value) && value > 0 ? value : null;
	}

	// yAxisBounds pads the plotted range instead of letting Chart.js do what
	// it does for bar charts and anchor the axis at zero. On the projected
	// bedtime-weight scale that wasted the bottom 108kg of the panel on
	// values that will never occur, squashing a ~1kg spread into a sliver.
	//
	// Every plotted value is folded in rather than just the column's own
	// [min, max], so nothing a caller decides to draw ends up clipped.
	// Padding is a fraction of the spread rather than a fixed number of
	// kilograms: these columns are usually a kilogram or two tall, and a
	// fixed pad wide enough to look right on a 5kg spread leaves a 1kg one
	// stranded in the middle of a mostly empty panel. The floor keeps a
	// degenerate case readable — one night logged collapses the column to a
	// single value, and a purely proportional pad would give it a
	// zero-height axis.
	const axisPaddingFraction = 0.1;
	const minAxisPaddingKg = 0.2;
	// targetKg is folded in so the target line is always on screen. It is
	// usually below every column — that gap is the overnight loss — so without
	// this the axis would stop short and the line would be drawn outside the
	// plot area.
	function yAxisBounds(points, targetKg) {
		let lo = Infinity;
		let hi = -Infinity;
		if (typeof targetKg === 'number' && Number.isFinite(targetKg)) {
			lo = targetKg;
			hi = targetKg;
		}
		points.forEach((p) => {
			[p.minKg, p.maxKg, p.lowKg, p.highKg, p.meanKg].forEach((value) => {
				if (typeof value === 'number' && Number.isFinite(value)) {
					lo = Math.min(lo, value);
					hi = Math.max(hi, value);
				}
			});
		});
		// No finite values at all: leave the axis to Chart.js rather than
		// handing it min: Infinity.
		if (lo === Infinity) return {};
		const pad = Math.max((hi - lo) * axisPaddingFraction, minAxisPaddingKg);
		return { min: lo - pad, max: hi + pad };
	}

	function renderChart(data) {
		const canvas = document.getElementById('overnight-window-chart');
		if (!canvas) return;
		const emptyEl = document.getElementById('overnight-window-empty');
		const zoneKey = document.getElementById('overnight-zone-key');
		// The fixed-height box, not the canvas, is what gets hidden — hiding
		// the canvas alone would leave its 320px box as a gap.
		const box = canvas.closest('.chart-box') || canvas;

		if (chart) {
			chart.destroy();
			chart = null;
		}
		if (!data.hasData) {
			box.hidden = true;
			if (emptyEl) {
				emptyEl.hidden = false;
				emptyEl.textContent = data.empty;
			}
			return;
		}

		const points = visiblePoints(data);
		if (!points.length) {
			box.hidden = true;
			if (emptyEl) {
				emptyEl.hidden = false;
				emptyEl.textContent = 'No timescale has an overnight pair in it yet.';
			}
			return;
		}
		box.hidden = false;
		if (emptyEl) emptyEl.hidden = true;

		const targetKg = currentTargetKg();
		const plotPoints = targetKg === null ? points : projectOntoTarget(points, targetKg);
		// The verdict bands only exist once the columns mean bedtime weight.
		if (zoneKey) zoneKey.hidden = targetKg === null;

		const labels = plotPoints.map((p) => p.label);
		// The column spans the whole observed range — best night on record to
		// worst — rather than the ±1 SD box it used to. The box put the mean
		// in the middle of the shape, which reads as the value to aim for
		// when it is really the coin flip; the gradient says the same thing
		// about the odds without nominating a number.
		const columns = plotPoints.map((p) => [p.minKg, p.maxKg]);

		try {
			chart = new Chart(canvas, {
				data: {
					labels,
					datasets: [
						{
							type: 'bar',
							label: targetKg === null ? 'Overnight change' : 'Bedtime weight',
							data: columns,
							// Scriptable because the gradient is built from pixel
							// positions, which don't exist until the scales have
							// been laid out; Chart.js re-runs this once they have.
							backgroundColor: (context) => {
								const p = plotPoints[context.dataIndex];
								if (!p || !context.chart.chartArea) return;
								return columnGradient(context.chart, p);
							},
							borderRadius: 4,
							barThickness: 40,
							// One night logged gives a zero-height column, which
							// would draw as nothing at all and take no tooltip.
							minBarLength: 3,
						},
					],
				},
				options: {
					responsive: true,
					maintainAspectRatio: false,
					scales: {
						x: {
							grid: { color: cssVar('--chart-grid') },
							ticks: { color: cssVar('--on-surface-muted') },
						},
						y: {
							...yAxisBounds(plotPoints, targetKg),
							title: {
								display: true,
								text: targetKg === null ? 'Overnight change (kg)' : 'Bedtime weight (kg)',
								color: cssVar('--on-surface-muted'),
							},
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
								title: (items) => plotPoints[items[0].dataIndex].name || plotPoints[items[0].dataIndex].label,
								label: (item) => {
									const p = plotPoints[item.dataIndex];
									if (targetKg === null) {
										const lines = [`Mean: ${p.meanLabel}`];
										lines.push(
											p.hasRange
												? `±1 SD: ${p.lowKg.toFixed(1)} to ${p.highKg.toFixed(1)} kg`
												: 'Not enough nights yet for a range',
										);
										lines.push(`Widest ever: ${p.minKg.toFixed(1)} to ${p.maxKg.toFixed(1)} kg`);
										lines.push(`${p.count} night${p.count === 1 ? '' : 's'} logged`);
										return lines;
									}
									// Read bottom-up, matching the column: the
									// green end is the only figure every logged
									// night clears, and each line above it is a
									// weaker claim than the one below.
									const lines = [`${p.minKg.toFixed(1)} kg or under: every night on record makes it`];
									// Being a standard deviation to the safe side
									// of the mean is roughly five nights in six
									// for a normal spread.
									if (p.hasRange) lines.push(`${p.lowKg.toFixed(1)} kg: about 5 nights in 6`);
									lines.push(`${p.meanLabel}: a coin flip`);
									lines.push(`Over ${p.maxKg.toFixed(1)} kg: no night on record makes it`);
									lines.push(`${p.count} night${p.count === 1 ? '' : 's'} logged`);
									return lines;
								},
							},
						},
						overnightRangeBands: { points: plotPoints, targetKg },
					},
				},
			});
		} catch (err) {
			console.error('overnight window chart build failed', err);
		}
	}

	// recomputeTonightCalculator projects tonight's entered weight forward
	// through each window's mean ± 1 SD to get a plausible morning
	// range, then — if a goal is currently active — judges whether that
	// range clears it: comfortably (the whole range is at/under goal), at
	// risk (the whole range is over), or borderline (goal falls inside the
	// range, so it could go either way).
	function recomputeTonightCalculator(data) {
		const resultsEl = document.getElementById('overnight-tonight-results');
		const emptyEl = document.getElementById('overnight-tonight-empty');
		const input = document.getElementById('overnight-tonight-input');
		if (!resultsEl || !emptyEl || !input) return;

		resultsEl.innerHTML = '';
		const tonightKg = parseFloat(input.value);
		if (!data || !data.hasData) {
			emptyEl.hidden = false;
			emptyEl.textContent = 'Not enough data yet to project a morning range.';
			return;
		}
		const points = visiblePoints(data);
		if (!points.length) {
			emptyEl.hidden = false;
			emptyEl.textContent = 'No timescale has an overnight pair in it yet.';
			return;
		}
		if (Number.isNaN(tonightKg) || tonightKg <= 0) {
			emptyEl.hidden = false;
			emptyEl.textContent = "Enter tonight's weight above to see a projected range.";
			return;
		}
		emptyEl.hidden = true;

		points.forEach((p) => {
			const lowKg = tonightKg + p.lowKg;
			const highKg = tonightKg + p.highKg;

			const tile = document.createElement('div');
			tile.className = 'stat-tile';

			const label = document.createElement('span');
			label.className = 'stat-label';
			label.textContent = p.label;
			tile.appendChild(label);

			const chip = document.createElement('span');
			chip.className = 'chip';
			chip.textContent = p.hasRange
				? `${lowKg.toFixed(1)} – ${highKg.toFixed(1)} kg`
				: `~${lowKg.toFixed(1)} kg (not enough nights for a range)`;

			if (data.hasGoal) {
				if (highKg <= data.goalKg) {
					chip.classList.add('chip-loss');
					chip.textContent += ' — comfortably makes it';
				} else if (lowKg > data.goalKg) {
					chip.classList.add('chip-gain');
					chip.textContent += ' — at risk of missing it';
				} else {
					chip.classList.add('chip-goal-current');
					chip.textContent += ' — borderline, could go either way';
				}
			}
			tile.appendChild(chip);
			resultsEl.appendChild(tile);
		});
	}

	function renderAll() {
		if (!cachedData) return;
		renderChart(cachedData);
		recomputeTonightCalculator(cachedData);
	}

	// autofillGoalTarget seeds the "Weigh-in calculator" target field with
	// the current goal weight, if there is one, the first time it's empty —
	// most people running this calculator want to know "can I make my goal
	// weight", not start from a blank field every time. Only fires when the
	// field is empty, so it never overwrites a value the user typed, and
	// hx-preserve="true" on the input means htmx swaps keep whatever's
	// already there rather than resetting it back to blank.
	function autofillGoalTarget(data) {
		const input = document.getElementById('overnight-calc-target');
		if (!input || input.value !== '' || !data.hasGoal) return;
		input.value = data.goalKg.toFixed(1);
	}

	// force distinguishes "the data or the colours may have changed, rebuild"
	// from "something on the page was swapped, check whether it was us".
	//
	// htmx:afterSwap fires on the body for every swap anywhere on the page,
	// including the range filter below, which now replaces only the stats and
	// the pairs table. Refetching and rebuilding on those was work with a
	// visible flash and nothing to show for it: the fixed-window data this
	// chart draws does not depend on the range filter at all. A live chart
	// still holding the canvas that is in the document means the swap missed
	// us, so there is nothing to do.
	function refresh(force) {
		const canvas = document.getElementById('overnight-window-chart');
		if (!canvas) return;
		if (!force && chart && chart.canvas === canvas) return;
		fetch('/overnight/windows')
			.then((res) => {
				if (!res.ok) throw new Error('server returned ' + res.status);
				return res.json();
			})
			.then((data) => {
				cachedData = data;
				autofillGoalTarget(data);
				renderAll();
			})
			.catch((err) => console.error('overnight window chart refresh failed', err));
	}

	// A swap only matters if it replaced the canvas; new entries and a theme
	// change always do, since both change what should be drawn.
	document.body.addEventListener('htmx:afterSwap', () => refresh(false));
	document.body.addEventListener('entries-changed', () => refresh(true));
	window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => refresh(true));
	document.body.addEventListener('input', (event) => {
		if (event.target.id === 'overnight-tonight-input') recomputeTonightCalculator(cachedData);
		// Re-render the chart itself so entering/clearing a target
		// immediately projects the columns onto (or back off of) bedtime
		// weight — this is the whole point of merging the two cards.
		if (event.target.id === 'overnight-calc-target') renderAll();
	});
	refresh(true);
})();
