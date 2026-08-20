document.querySelectorAll('.tab-btn').forEach((btn) => {
	btn.addEventListener('click', () => {
		document.querySelectorAll('.tab-btn').forEach((b) => b.classList.remove('active'));
		document.querySelectorAll('.tab-panel').forEach((p) => { p.hidden = true; });
		btn.classList.add('active');
		document.getElementById('tab-' + btn.dataset.tab).hidden = false;
	});
});

const logDialog = document.getElementById('log-dialog');
const recordedAtDate = document.getElementById('recorded-at-date');
const recordedAtTime = document.getElementById('recorded-at-time');
function pad(n) { return String(n).padStart(2, '0'); }
document.getElementById('log-fab').addEventListener('click', () => {
	const d = new Date();
	recordedAtDate.value = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
	recordedAtTime.value = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
	logDialog.showModal();
});
document.getElementById('log-cancel').addEventListener('click', () => logDialog.close());

const confirmInput = document.getElementById('confirm-delete-input');
const confirmBtn = document.getElementById('confirm-delete-btn');
confirmInput.addEventListener('input', () => {
	confirmBtn.disabled = confirmInput.value !== 'DELETE';
});

if ('serviceWorker' in navigator) {
	window.addEventListener('load', () => navigator.serviceWorker.register('/sw.js'));
}

// initTimeRangePicker wires up one instance of the shared Grafana-style
// time-range-picker (templates/time_range_picker.html): a button showing
// the active range, opening a popover of quick presets plus a custom
// from/until section. The chart and the history filter each embed their
// own instance of the exact same markup, scoped here via [data-role]
// rather than ids, so both behave identically with no divergence between
// them — root is just whichever DOM node carries data-time-range-picker.
//
// Applying a range fires a bubbling 'change' event on the hidden range
// input rather than calling some page-specific refresh function directly,
// so this component doesn't need to know or care what page it's on:
// whichever ancestor form is listening — chart-app.js's own
// addEventListener('change', refreshChart), or htmx's
// hx-trigger="change" on the history filter form — picks it up and
// refreshes on its own terms.
function initTimeRangePicker(root) {
	const btn = root.querySelector('[data-role="btn"]');
	const label = root.querySelector('[data-role="label"]');
	const popover = root.querySelector('[data-role="popover"]');
	const rangeInput = root.querySelector('[data-role="range-input"]');
	const fromInput = root.querySelector('[data-role="from-input"]');
	const untilInput = root.querySelector('[data-role="until-input"]');
	const customFromInput = root.querySelector('[data-role="custom-from"]');
	const customUntilInput = root.querySelector('[data-role="custom-until"]');
	const customApplyBtn = root.querySelector('[data-role="custom-apply"]');
	const presetButtons = Array.from(root.querySelectorAll('.time-range-preset'));

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

	// A custom bound is a calendar date, a relative expression ("now-5d"),
	// or a cross-reference to the other bound ("to-5d", "from+5d"). Only the
	// date form is worth prettifying — the others already read as what the
	// user typed, and echoing them back verbatim is also how a typo becomes
	// visible, since the server treats an unresolvable bound as simply
	// unbounded on that side.
	const dateOnlyBound = /^\d{4}-\d{2}-\d{2}$/;
	function formatBound(value) {
		return dateOnlyBound.test(value) ? formatDateLabel(parseDateInput(value)) : value;
	}

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
	function syncRangeLabel() {
		if (rangeInput.value === 'custom') {
			presetButtons.forEach((b) => b.classList.remove('active'));
			const from = fromInput.value;
			const until = untilInput.value;
			if (from && until) {
				label.textContent = `${formatBound(from)} – ${formatBound(until)}`;
			} else if (from) {
				label.textContent = `Since ${formatBound(from)}`;
			} else if (until) {
				label.textContent = `Until ${formatBound(until)}`;
			}
			return;
		}
		presetButtons.forEach((presetBtn) => {
			const match = presetBtn.dataset.range === rangeInput.value;
			presetBtn.classList.toggle('active', match);
			if (match) label.textContent = presetBtn.dataset.label;
		});
		syncCustomInputs();
	}

	function openPopover() {
		popover.hidden = false;
		btn.setAttribute('aria-expanded', 'true');
	}

	function closePopover() {
		popover.hidden = true;
		btn.setAttribute('aria-expanded', 'false');
	}

	btn.addEventListener('click', (event) => {
		event.stopPropagation();
		if (popover.hidden) {
			openPopover();
		} else {
			closePopover();
		}
	});

	document.addEventListener('click', (event) => {
		if (!popover.hidden && !event.composedPath().includes(popover) && event.target !== btn) {
			closePopover();
		}
	});

	document.addEventListener('keydown', (event) => {
		if (event.key === 'Escape' && !popover.hidden) closePopover();
	});

	function apply() {
		syncRangeLabel();
		closePopover();
		rangeInput.dispatchEvent(new Event('change', { bubbles: true }));
	}

	presetButtons.forEach((presetBtn) => {
		presetBtn.addEventListener('click', () => {
			rangeInput.value = presetBtn.dataset.range;
			fromInput.value = '';
			untilInput.value = '';
			apply();
		});
	});

	customApplyBtn.addEventListener('click', () => {
		if (!customFromInput.value && !customUntilInput.value) return;
		rangeInput.value = 'custom';
		fromInput.value = customFromInput.value;
		untilInput.value = customUntilInput.value;
		apply();
	});

	syncRangeLabel();
}

document.querySelectorAll('[data-time-range-picker]').forEach(initTimeRangePicker);
