// Piccolo PWA service worker — network-first with offline fallback.
// __CACHE_VERSION__ is replaced at build time by the Makefile.
const CACHE = 'piccolo-__CACHE_VERSION__';

self.addEventListener('install', event => {
  self.skipWaiting();
  event.waitUntil(
    caches.open(CACHE).then(cache => cache.add('/entry.html'))
  );
});

self.addEventListener('activate', event => {
  event.waitUntil(
    caches.keys()
      .then(keys => Promise.all(
        keys.filter(k => k.startsWith('piccolo-') && k !== CACHE)
          .map(k => caches.delete(k))
      ))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', event => {
  const { request } = event;
  if (request.method !== 'GET') return;

  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;
  if (url.pathname.startsWith('/api/') || url.pathname.startsWith('/oauth/') ||
      url.pathname.startsWith('/health/') || url.pathname === '/version') return;

  event.respondWith(
    fetch(request)
      .then(response => {
        if (response.ok) {
          const clone = response.clone();
          caches.open(CACHE).then(c => c.put(request, clone)).catch(() => {});
        }
        return response;
      })
      .catch(() =>
        caches.match(request).then(r =>
          r || (request.mode === 'navigate' ? caches.match('/entry.html') : undefined)
        )
      )
  );
});
