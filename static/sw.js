self.addEventListener('install', () => self.skipWaiting());

// Bumped from v1 when the strategy below changed from cache-first to
// network-first. activate deletes every cache that isn't this one, so the
// stale v1 entries go away on their own — an already-installed browser
// recovers on its next visit without anyone clearing site data.
const STATIC_CACHE = 'wt-static-v2';
const STATIC_ASSETS = ['/static/style.css', '/static/htmx.min.js', '/static/icon.svg'];

self.addEventListener('activate', (event) => {
	event.waitUntil(
		caches
			.keys()
			.then((names) =>
				Promise.all(names.filter((name) => name !== STATIC_CACHE).map((name) => caches.delete(name)))
			)
			.then(() => self.clients.claim())
	);
});

self.addEventListener('fetch', (event) => {
	if (event.request.method !== 'GET') return; // never intercept POST/PUT/DELETE

	const url = new URL(event.request.url);
	if (url.origin !== self.location.origin || !STATIC_ASSETS.includes(url.pathname)) {
		// Everything else (/, /chart, /entries, /summary, ...) is always live —
		// this is a data-entry app, so caching dynamic responses would risk
		// showing stale weigh-ins or a silently-failed submit while offline.
		return;
	}

	// Network-first. The previous cache-first version meant a browser that had
	// installed this worker never saw a CSS or htmx change again: a cache hit
	// always won, and nothing ever invalidated the entry. Since the app is
	// server-rendered and shows nothing useful offline anyway, the cache earns
	// its keep only as a fallback, not as the primary source.
	event.respondWith(
		fetch(event.request)
			.then((res) => {
				// Only store a real success. Caching an error page here would
				// keep serving it for as long as the device stayed offline.
				if (res && res.ok) {
					const copy = res.clone();
					caches.open(STATIC_CACHE).then((cache) => cache.put(event.request, copy));
				}
				return res;
			})
			.catch(() =>
				caches
					.open(STATIC_CACHE)
					.then((cache) => cache.match(event.request))
					// Response.error() rather than undefined: resolving
					// respondWith with undefined is a TypeError, which would
					// bury the real "offline, and never cached" cause.
					.then((cached) => cached || Response.error())
			)
	);
});
