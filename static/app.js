// The active tab lives in the URL hash, so a reload keeps you where you
// were instead of dropping you back on Log, and the back button steps
// through the tabs you visited. A hash rather than a query parameter
// because this is purely a client-side view choice — the server renders
// every panel regardless, and nothing here needs a round-trip.
//
// This runs synchronously at the end of <body>, so the correct panel is
// showing before the first paint; there's no flash of the Log tab.
const tabButtons = Array.from(document.querySelectorAll('.tab-btn'));
const tabNames = tabButtons.map((btn) => btn.dataset.tab);

// An unknown or absent hash falls back to the first tab rather than
// leaving every panel hidden — a hand-edited or stale link should still
// land somewhere usable.
function tabFromURL() {
	const name = window.location.hash.replace(/^#/, '');
	return tabNames.includes(name) ? name : tabNames[0];
}

function activateTab(name) {
	tabButtons.forEach((btn) => btn.classList.toggle('active', btn.dataset.tab === name));
	document.querySelectorAll('.tab-panel').forEach((panel) => {
		panel.hidden = panel.id !== 'tab-' + name;
	});
}

tabButtons.forEach((btn) => {
	btn.addEventListener('click', () => {
		const name = btn.dataset.tab;

		if (tabFromURL() === name) {
			window.scrollTo(0, 0);
		} else {
			// Re-tapping the current tab shouldn't stack up history entries that
			// the back button then has to walk through one by one.
			history.pushState(null, '', '#' + name);
		}
		activateTab(name);
	});
});

// pushState doesn't fire hashchange, so back/forward is handled here.
window.addEventListener('popstate', () => activateTab(tabFromURL()));

activateTab(tabFromURL());

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

// The last range copied from any picker on this page, as the JSON that was
// put on the clipboard. The clipboard itself is the real transport — this is
// only there for the browsers that let a page write it but not read it back
// (Firefox before 125, and anything where the read prompt is declined), so
// that the common case of moving a range from one tab of this app to another
// keeps working when navigator.clipboard.readText does not.
let copiedRangeJSON = null;

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
	const copyBtn = root.querySelector('[data-role="copy-range"]');
	const pasteBtn = root.querySelector('[data-role="paste-range"]');
	const clipStatus = root.querySelector('[data-role="clip-status"]');
	const urlParam = root.dataset.urlParam;
	const defaultRange = root.dataset.defaultRange;

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

	// syncURL puts the applied range in the query string so a reload, a
	// bookmark or a shared link comes back to the same view — the server
	// reads these back on the way in (see timerange.Picker) and renders the
	// picker and its content from them, so there is no flash of the default
	// range before the client corrects it.
	//
	// A picker sitting at its own default writes nothing at all, which keeps
	// "/" the canonical address of an untouched page rather than one URL
	// among several that mean the same thing.
	//
	// replaceState rather than pushState: the back button belongs to the tabs
	// (see the top of this file), and stepping it back through every range a
	// user tried on the way to the one they wanted is not what it is for.
	function syncURL() {
		if (!urlParam) return;
		const url = new URL(window.location.href);
		const atDefault = rangeInput.value === defaultRange && !fromInput.value && !untilInput.value;
		[urlParam, urlParam + '_from', urlParam + '_until'].forEach((name) => url.searchParams.delete(name));
		if (!atDefault) {
			url.searchParams.set(urlParam, rangeInput.value);
			if (fromInput.value) url.searchParams.set(urlParam + '_from', fromInput.value);
			if (untilInput.value) url.searchParams.set(urlParam + '_until', untilInput.value);
		}
		window.history.replaceState(null, '', url);
	}

	function apply() {
		syncRangeLabel();
		syncURL();
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

	function applyCustomRange() {
		if (!customFromInput.value && !customUntilInput.value) return;
		rangeInput.value = 'custom';
		fromInput.value = customFromInput.value;
		untilInput.value = customUntilInput.value;
		apply();
	}

	customApplyBtn.addEventListener('click', applyCustomRange);

	// Enter applies the range, which is what a text box in a popover invites
	// you to do. preventDefault matters: both pickers sit inside a form — the
	// chart's controls and the history filter — so the browser's implicit
	// submission would otherwise fire first, reloading the page or sending
	// the filter's htmx request with the old range still in the hidden
	// inputs.
	[customFromInput, customUntilInput].forEach((input) => {
		input.addEventListener('keydown', (event) => {
			if (event.key !== 'Enter') return;
			event.preventDefault();
			applyCustomRange();
		});
	});

	// The clipboard payload is the exact range/from/until triple the form
	// submits, so there is nothing to translate in either direction and what
	// lands on the clipboard reads as the query the app would run.
	function currentRangeJSON() {
		return JSON.stringify({
			range: rangeInput.value,
			from: fromInput.value,
			until: untilInput.value,
		});
	}

	let clipStatusTimer = null;
	function showClipStatus(message) {
		clipStatus.textContent = message;
		clipStatus.hidden = false;
		clearTimeout(clipStatusTimer);
		clipStatusTimer = setTimeout(() => {
			clipStatus.hidden = true;
		}, 3000);
	}

	// navigator.clipboard exists only in a secure context, which this app has
	// over the tunnel but not when it is opened at a bare LAN address. The
	// textarea-and-execCommand path is deprecated, and it is still the only
	// thing that copies there.
	function writeClipboard(text) {
		if (navigator.clipboard && navigator.clipboard.writeText) {
			return navigator.clipboard.writeText(text);
		}
		return new Promise((resolve, reject) => {
			const scratch = document.createElement('textarea');
			scratch.value = text;
			scratch.setAttribute('readonly', '');
			// Off-screen rather than hidden: a display:none textarea cannot be
			// selected, and selecting it is the whole mechanism.
			scratch.style.position = 'fixed';
			scratch.style.top = '-1000px';
			document.body.appendChild(scratch);
			scratch.select();
			const copied = document.execCommand('copy');
			document.body.removeChild(scratch);
			if (copied) {
				resolve();
			} else {
				reject(new Error('execCommand copy was refused'));
			}
		});
	}

	// Reading is the half browsers guard hardest: Firefox withheld readText
	// from pages entirely until 125, and Safari prompts. A null here is not
	// an error, just a signal to fall back to the in-page copy.
	function readClipboard() {
		if (navigator.clipboard && navigator.clipboard.readText) {
			return navigator.clipboard.readText().catch(() => null);
		}
		return Promise.resolve(null);
	}

	const presetRanges = presetButtons.map((presetBtn) => presetBtn.dataset.range);

	// parseRangeJSON accepts only what this app itself emits. An unrecognized
	// preset resolves server-side to "all time" (see timerange.Resolve) while
	// the button would go on claiming whatever was pasted, so a range that
	// doesn't check out is refused rather than half-applied.
	function parseRangeJSON(text) {
		if (!text) return null;
		let parsed;
		try {
			parsed = JSON.parse(text);
		} catch (err) {
			return null;
		}
		if (!parsed || typeof parsed !== 'object') return null;
		const range = typeof parsed.range === 'string' ? parsed.range.trim() : '';
		const from = typeof parsed.from === 'string' ? parsed.from.trim() : '';
		const until = typeof parsed.until === 'string' ? parsed.until.trim() : '';
		// Bounds are free text ("now-5d", "2026-01-01", "from+5d"), so the
		// only thing worth asserting is that they are text of a sane length.
		if (from.length > 64 || until.length > 64) return null;
		if (range === 'custom') {
			return from || until ? { range, from, until } : null;
		}
		return presetRanges.includes(range) ? { range, from: '', until: '' } : null;
	}

	copyBtn.addEventListener('click', () => {
		const json = currentRangeJSON();
		copiedRangeJSON = json;
		writeClipboard(json)
			.then(() => showClipStatus('Range copied'))
			// Still pasteable into the other pickers on this page, which is
			// what it is mostly for — just not into anything outside it.
			.catch(() => showClipStatus('Copied within this page only — the browser blocked the clipboard'));
	});

	pasteBtn.addEventListener('click', () => {
		readClipboard().then((text) => {
			// The clipboard wins whenever it holds a range, so a copy made in
			// another tab or another window still lands. Anything else on it —
			// or no read at all — falls back to the last copy made here.
			const pasted = parseRangeJSON(text) || parseRangeJSON(copiedRangeJSON);
			if (!pasted) {
				showClipStatus('No range to paste');
				return;
			}
			rangeInput.value = pasted.range;
			fromInput.value = pasted.from;
			untilInput.value = pasted.until;
			// syncRangeLabel leaves a custom range's boxes alone on purpose,
			// so a pasted one has to fill them itself; otherwise the popover
			// would keep showing whatever was in them before.
			if (pasted.range === 'custom') {
				customFromInput.value = pasted.from;
				customUntilInput.value = pasted.until;
			}
			// No success message: apply() closes the popover, and the view
			// changing under a relabelled button is the confirmation.
			apply();
		});
	});

	syncRangeLabel();
}

document.querySelectorAll('[data-time-range-picker]').forEach(initTimeRangePicker);
