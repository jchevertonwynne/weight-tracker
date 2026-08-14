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
