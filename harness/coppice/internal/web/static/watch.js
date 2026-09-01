import { applyFrame, gridToText } from './grid.js';

// A view may watch up to WATCH_MAX panes. The floor attaches each one
// view-only, which never resizes a pane and may not type into it, and
// posts the view a picture of each frame. Every attach and detach waits
// for the reply to the call before it, so two calls for one pane never
// meet on the wire in the wrong order. A pane whose attach was refused is
// not tried again until the pane list changes.

export const WATCH_MAX = 9;

// listKey is what makes one pane list differ from another for a retry:
// the ids, and which of them are closed.
function listKey(panes) {
  return (panes || []).map((p) => p.id + (p.closed ? ':closed' : '')).join(',');
}

// mountWatch keeps the attaches one view host asked for. rpc sends one
// request on the floor page's socket and returns its reply. changed runs
// whenever the panes shown live or refused change.
export function mountWatch(rpc, changed) {
  let chain = Promise.resolve();
  let open = 0;
  let want = [];
  // attached holds each pane with an attach sent or on its way, and live
  // each pane whose attach was answered.
  const attached = new Set();
  const live = new Set();
  const refused = new Set();
  let lastList = null;
  const grids = new Map();
  // wire holds each pane whose view-only attach is sent and not answered.
  const wire = new Set();
  // sent holds each pane with a view-only attach sent and no detach since.
  const sent = new Set();
  // epoch changes when the socket drops, so a call queued for the dead
  // socket is never sent on the new one.
  let epoch = 0;
  const tell = () => { if (changed) changed(); };

  const queue = (fn) => {
    open += 1;
    chain = chain.then(fn).catch(() => {}).then(() => { open -= 1; });
    return chain;
  };

  const sync = () => {
    const keep = new Set(want);
    for (const pane of [...attached]) {
      if (keep.has(pane)) continue;
      attached.delete(pane);
      live.delete(pane);
      grids.delete(pane);
      const e = epoch;
      // An attach that was dropped before it went out needs no detach.
      queue(() => {
        if (e !== epoch || !sent.has(pane)) return null;
        sent.delete(pane);
        return rpc('pane.detach', { pane });
      });
    }
    for (const pane of want) {
      if (attached.has(pane) || refused.has(pane)) continue;
      attached.add(pane);
      const e = epoch;
      queue(() => {
        // A pane released while its attach waited here belongs to a window
        // now, and a view-only attach sent after the window's own would
        // leave that window unable to type.
        if (e !== epoch || !attached.has(pane)) return null;
        wire.add(pane);
        sent.add(pane);
        const done = () => wire.delete(pane);
        return rpc('pane.attach', { pane, view_only: true }).then(() => {
          done();
          if (e !== epoch || !attached.has(pane)) return;
          live.add(pane);
          tell();
        }, () => {
          done();
          if (e !== epoch) return;
          attached.delete(pane);
          refused.add(pane);
          tell();
        });
      });
    }
  };

  return {
    // set replaces the watched panes.
    set(panes) {
      want = [...new Set(panes)].slice(0, WATCH_MAX);
      sync();
      tell();
    },
    // release forgets panes with no detach, and stops wanting them. A
    // window of the floor holds each such pane on the same socket now, so
    // a detach here would end the window's own attach. It returns the
    // panes whose view-only attach is on the wire: the server may take it
    // after the window's own, so the window must attach again once quiet
    // settles.
    release(panes) {
      const racing = (panes || []).filter((p) => wire.has(p));
      let any = false;
      for (const pane of panes || []) {
        if (want.includes(pane) || attached.has(pane)) any = true;
        attached.delete(pane);
        live.delete(pane);
        grids.delete(pane);
        refused.delete(pane);
        sent.delete(pane);
      }
      const drop = new Set(panes || []);
      want = want.filter((p) => !drop.has(p));
      if (any) tell();
      return racing;
    },
    // quiet settles once every call queued so far is answered.
    quiet() {
      return chain.then(() => {});
    },
    // gate makes every later call wait for p first.
    gate(p) {
      queue(() => p);
    },
    // end stops every watch. It returns a promise that settles once each
    // detach is answered, or null when no call is on its way.
    end() {
      want = [];
      sync();
      return open > 0 ? chain : null;
    },
    // refresh takes a fresh pane list. It drops a pane the list no longer
    // holds. When the list changed, a refused pane is tried again.
    refresh(panes) {
      const key = listKey(panes);
      if (lastList !== null && key !== lastList) refused.clear();
      lastList = key;
      const known = new Set((panes || []).map((p) => p.id));
      const before = want.length;
      want = want.filter((p) => known.has(p));
      for (const p of [...refused]) if (!known.has(p)) refused.delete(p);
      sync();
      if (want.length !== before) tell();
    },
    // status is the watched panes shown live and the ones refused.
    status() {
      return { live: want.filter((p) => live.has(p)), refused: want.filter((p) => refused.has(p)) };
    },
    // frame folds one frame event into the picture of a watched pane. It
    // returns what the view is posted, or null for a pane not watched.
    frame(msg) {
      if (!msg || !want.includes(msg.pane) || !attached.has(msg.pane)) return null;
      const grid = applyFrame(grids.get(msg.pane) || null, msg);
      grids.set(msg.pane, grid);
      return { pane: msg.pane, cols: grid.cols, rows: grid.rows, lines: gridToText(grid).split('\n') };
    },
    // dropped forgets every attach and sends nothing: the socket that held
    // them is gone. The next refresh attaches again.
    dropped() {
      epoch += 1;
      attached.clear();
      wire.clear();
      sent.clear();
      live.clear();
      refused.clear();
      grids.clear();
      tell();
    },
  };
}
