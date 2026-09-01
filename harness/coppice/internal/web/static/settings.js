// Server, token, and one button that proves push works. Nothing here talks
// to anything but this box.

export function wsUrl(origin) {
  const u = new URL(origin);
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
  u.pathname = '/ws';
  u.search = '';
  u.hash = '';
  return u.toString();
}

// tokenFromHash reads the token a scanned QR puts in the fragment. A token
// that is not exactly 43 characters of unpadded base64url is junk, and
// storing junk would leave the app unable to connect with nothing on screen
// to explain why.
export function tokenFromHash(hash) {
  const m = /^#t=([A-Za-z0-9_-]{43})$/.exec(hash || '');
  return m ? m[1] : null;
}

export function mountSettings() {
  const token = document.getElementById('set-token');

  window.addEventListener('coppice:route', () => {
    if (window.coppice.route(location.hash).screen !== 'settings') return;
    // The app always talks to the origin it was served from. This is a
    // fact, not a field to edit. Editing it would need CORS support on the
    // server to mean anything, and a control that changes nothing helps
    // nobody.
    document.getElementById('set-origin').textContent = 'Server ' + location.origin;
    token.value = localStorage.getItem('coppice.token') || '';
  });

  document.getElementById('save-settings').addEventListener('click', () => {
    const tok = token.value.trim();
    if (!tok) {
      window.coppice.status('Paste a token first, or tap Forget token to clear it.');
      return;
    }
    if (!/^[A-Za-z0-9_-]{43}$/.test(tok)) {
      window.coppice.status('That token is not the right shape. Run coppice web token.');
      return;
    }
    localStorage.setItem('coppice.token', tok);
    window.coppice.reconnect(tok);
    window.coppice.status('Saved. Reconnecting.');
  });

  document.getElementById('forget-token').addEventListener('click', () => {
    localStorage.removeItem('coppice.token');
    token.value = '';
    window.coppice.status('Token forgotten. Scan the QR again.');
  });

  document.getElementById('test-push').addEventListener('click', async () => {
    try {
      await window.coppice.api('/api/push/test', { method: 'POST' });
      window.coppice.status('Sent. Watch the ntfy app.');
    } catch (e) {
      window.coppice.status(e.message);
    }
  });
}
