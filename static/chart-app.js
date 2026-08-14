(function () {
	const canvas = document.getElementById('weight-chart');
	if (!canvas) return;
	const emptyEl = document.getElementById('chart-empty');
	const markerNoteEl = document.getElementById('marker-note');
	const form = document.getElementById('chart-controls');
	let chart = null;

	function cssVar(name) {
		return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
	}

	// markerLabelMaxWidth caps how wide a single marker's label is allowed to
	// be before it gets truncated with an ellipsis — keeps a long note from
	// crowding out its neighbors.
	const markerLabelMaxWidth = 90;
	// markerLabelGap is the minimum pixel gap required between two labels'
	// bounding boxes; anything tighter than this and the later (rightward)
	// label is skipped rather than overlapping the one before it.
	const markerLabelGap = 4;

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
	// readable at a glance rather than only on tap. Labels are drawn
	// left-to-right and skipped if they'd overlap the previous one —
	// skipped/truncated notes are still readable via tap, using the pixel
	// positions recorded in chart.$markerPositions.
	const markerLinesPlugin = {
		id: 'markerLines',
		afterDatasetsDraw(chartInstance) {
			const markers = (chartInstance.options.plugins.markerLines || {}).markers || [];
			chartInstance.$markerPositions = [];
			if (!markers.length) return;
			const { ctx, chartArea, scales } = chartInstance;
			ctx.save();
			ctx.strokeStyle = cssVar('--marker');
			ctx.setLineDash([3, 3]);
			ctx.lineWidth = 1;
			const positioned = [];
			markers.forEach((m) => {
				const x = scales.x.getPixelForValue(m.x);
				if (x < chartArea.left || x > chartArea.right) return;
				ctx.beginPath();
				ctx.moveTo(x, chartArea.top);
				ctx.lineTo(x, chartArea.bottom);
				ctx.stroke();
				positioned.push({ x, date: m.date, note: m.note });
			});
			chartInstance.$markerPositions = positioned;

			ctx.setLineDash([]);
			ctx.font = '10px "Roboto", "Segoe UI", system-ui, -apple-system, sans-serif';
			ctx.fillStyle = cssVar('--marker');
			ctx.textAlign = 'center';
			ctx.textBaseline = 'bottom';
			const labelY = chartArea.top - 6;
			let rightEdge = -Infinity;
			positioned
				.slice()
				.sort((a, b) => a.x - b.x)
				.forEach((m) => {
					const label = truncateToWidth(ctx, m.note, markerLabelMaxWidth);
					if (!label) return;
					const width = ctx.measureText(label).width;
					const left = m.x - width / 2;
					if (left < rightEdge + markerLabelGap) return;
					ctx.fillText(label, m.x, labelY);
					rightEdge = m.x + width / 2;
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
				// labels, so they never overlap the topmost data points.
				layout: { padding: { top: 18 } },
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
		chart = new Chart(canvas, buildConfig(data));
	}

	function refreshChart() {
		const params = new URLSearchParams(new FormData(form));
		fetch('/chart?' + params.toString())
			.then((res) => res.json())
			.then(renderChart)
			.catch((err) => console.error('chart refresh failed', err));
	}

	canvas.addEventListener('click', (event) => {
		if (!chart || !chart.$markerPositions || !chart.$markerPositions.length || !markerNoteEl) return;
		const pos = Chart.helpers.getRelativePosition(event, chart);
		const nearest = chart.$markerPositions.find((m) => Math.abs(m.x - pos.x) < 15);
		if (nearest) {
			markerNoteEl.hidden = false;
			markerNoteEl.textContent = nearest.date + ': ' + nearest.note;
		} else {
			markerNoteEl.hidden = true;
		}
	});

	form.addEventListener('change', refreshChart);
	document.body.addEventListener('entries-changed', refreshChart);
	document.body.addEventListener('goals-changed', refreshChart);
	document.body.addEventListener('markers-changed', refreshChart);
	window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', refreshChart);

	refreshChart();
})();
