self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));

const STATIC_CACHE = 'wt-static-v1';
const STATIC_ASSETS = ['/static/style.css', '/static/htmx.min.js', '/static/icon.svg'];

self.addEventListener('fetch', (event) => {
	if (event.request.method !== 'GET') return; // never intercept POST/PUT/DELETE

	const url = new URL(event.request.url);
	if (!STATIC_ASSETS.includes(url.pathname)) {
		// Everything else (/, /chart, /entries, /summary, ...) is always live —
		// this is a data-entry app, so caching dynamic responses would risk
		// showing stale weigh-ins or a silently-failed submit while offline.
		return;
	}

	event.respondWith(
		caches.open(STATIC_CACHE).then((cache) =>
			cache.match(event.request).then(
				(cached) =>
					cached ||
					fetch(event.request).then((res) => {
						cache.put(event.request, res.clone());
						return res;
					})
			)
		)
	);
});
