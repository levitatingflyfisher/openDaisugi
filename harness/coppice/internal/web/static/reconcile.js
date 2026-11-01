// reconcile.js: draw without tearing down what did not change.
//
// A browser keeps focus, a text selection, a hover, a scroll position and a
// click in progress on the nodes that stay. A draw that empties a list and
// fills it again loses them all, and moves the scroll to the top between
// the two steps. These helpers touch only the nodes that differ.

// reconcile makes the children of parent exactly nodes, in order. A node
// that is a child already, at the right place, is not touched: it is the
// same node, so its scroll and focus stay. Other children are removed, and
// new ones are put in.
export function reconcile(parent, nodes) {
  const want = nodes.filter(Boolean);
  if (typeof parent.insertBefore !== 'function' || typeof parent.removeChild !== 'function') {
    parent.replaceChildren(...want);
    return;
  }
  const keep = new Set(want);
  for (const c of Array.from(parent.children || [])) if (!keep.has(c)) parent.removeChild(c);
  want.forEach((n, i) => {
    const at = parent.children[i];
    if (at !== n) parent.insertBefore(n, at || null);
  });
}

// changed reports whether sig is not what slot last drew, and remembers it.
// slot is any object the caller keeps for one place on the page. owner is
// the element drawn into: another element than last time is always a change.
export function changed(slot, sig, owner) {
  if (slot.owner === owner && slot.sig === sig) return false;
  slot.owner = owner;
  slot.sig = sig;
  return true;
}
