// Shell-only cache. Mission Control is entirely live over WebSocket, so there is
// no offline DATA mode — the service worker exists for installability and a fast
// cold start of the static shell. Network-first, cache fallback for navigations.
const CACHE = 'flow-shell-v1'
self.addEventListener('install', (e) => { self.skipWaiting() })
self.addEventListener('activate', (e) => {
  e.waitUntil(caches.keys().then((ks) => Promise.all(ks.filter((k) => k !== CACHE).map((k) => caches.delete(k)))))
})
self.addEventListener('fetch', (e) => {
  const req = e.request
  if (req.method !== 'GET' || new URL(req.url).pathname.startsWith('/ws')) return
  e.respondWith(
    fetch(req).then((res) => {
      const copy = res.clone()
      caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {})
      return res
    }).catch(() => caches.match(req).then((m) => m || caches.match('/')))
  )
})
