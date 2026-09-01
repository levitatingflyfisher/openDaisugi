// The shell is cached so the app opens with no network. Pane content never
// is. A stale frame is worse than an empty one, and a cached ask is a lie
// about what the gate is holding right now.
export const SHELL = [
  '/', '/app.css', '/app.js', '/grid.js', '/chips.js', '/roster.js', '/pane.js',
  '/tiles.js', '/floor.js', '/windows.js', '/keys.js', '/rail.js', '/dock.js',
  '/newpane.js', '/settings.js', '/views.js', '/watch.js', '/record.js', '/messages.js', '/stackbar.js', '/overlay.js', '/sw-policy.js', '/manifest.webmanifest',
  '/icons/icon-192.png', '/icons/icon-512.png',
];

export function shouldCache(path) {
  return SHELL.includes(path);
}
