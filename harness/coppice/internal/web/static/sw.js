import { SHELL, shouldCache } from './sw-policy.js';

const CACHE = 'coppice-shell-v6';

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

// The fetch handler answers from the cache first, so the shell opens with
// no network, and always starts a real fetch behind it to refresh the
// cache. Answering from the cache with nothing behind it would mean an
// installed phone never sees a new shell: a browser only reruns install
// when sw.js or a file it imports changes on the wire, and sw.js imports
// only sw-policy.js. This way the open that just ran was one version
// behind, and the next one is current.
self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== self.location.origin || !shouldCache(url.pathname)) {
    return; // straight to the network, and never stored
  }
  const fresh = caches.open(CACHE).then((c) => fetch(e.request).then((res) => {
    c.put(e.request, res.clone());
    return res;
  }));
  e.waitUntil(fresh.catch(() => {}));
  e.respondWith(caches.match(e.request).then((hit) => hit || fresh));
});
