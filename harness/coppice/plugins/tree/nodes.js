// The delegation tree's nodes and asks as lines of the tree view. They come
// from `daisugi tree status --json`, which the floor page posts as tree.

// nodeText is a delegation-tree node without its prefix: the session, its
// state, tokens used of reserved with the overrun, and whether the daisugi
// proved its envelope against its parent's. An operator allow is not
// proved, and the child must still fit its branch's root (TR-R-7).
export function nodeText(n) {
  const parts = [n.session, n.state];
  const t = n.tokens || {};
  if (typeof t.budget === 'number') {
    const used = typeof t.used === 'number' ? t.used : 0;
    parts.push('tokens ' + used + '/' + t.budget);
    if (used > t.budget) parts.push('over by ' + (used - t.budget));
  }
  parts.push(n.proved === false ? 'not proved against its parent' : 'proved');
  if (n.refused) parts.push(n.refused + ' refused');
  return parts.join('  ');
}

// askText is an open ask: a parent that wants a child daisugi refused.
export function askText(a) {
  const why = Array.isArray(a.reasons) && a.reasons.length ? ': ' + a.reasons[0] : '';
  return 'ask ' + a.ask_id + '  ' + a.parent + ' asks to start ' + a.session + why;
}

// nodeLines lists the nodes below session (itself first) with the indent
// their depth gives, from the nodes daisugi tree status gave.
export function nodeLines(nodes, session, indent) {
  const by = new Map((nodes || []).map((n) => [n.session, n]));
  const out = [];
  const seen = new Set();
  const walk = (id, depth) => {
    const n = by.get(id);
    if (!n || seen.has(id)) return;
    seen.add(id);
    out.push({ text: indent + '  '.repeat(depth) + '· ' + nodeText(n), task: '', pane: '', node: id });
    for (const c of Array.isArray(n.children) ? n.children : []) walk(c, depth + 1);
  };
  walk(session, 0);
  return out;
}
