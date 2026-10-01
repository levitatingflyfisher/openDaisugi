// One chain of attach and detach per agent, on one socket, for the sheet and
// the floor together.
//
// The server runs a connection's requests on separate goroutines, so a
// detach sent while an attach is unanswered can arrive first, find nothing
// attached, and leave the attach, and its fit, holding the agent. So neither
// the sheet nor the floor sends pane.attach or pane.detach on its own.
//
// The floor's windows hand their work to run(id, fn): fn runs when every
// request ahead of it for that agent has been answered. The sheet says what
// it wants for an agent (nothing, or the screen at a fit) and each step
// reads what is wanted when it runs, not when it was asked for, so a quick
// leave and return become no requests, or one, in the right order.
//
// An agent the sheet hands to the floor (handOver) keeps its chain. Only the
// owner changes, so the floor's detach waits behind the attach the sheet
// sent, and a reply that lands later is not the sheet's to take back.
//
// The queue belongs to one socket. alive() says whether that socket is still
// the page's; once it is not, nothing more is sent.

const same = (a, b) => (!a && !b) || (a && b && a.cols === b.cols && a.rows === b.rows);

// createQueue makes a queue that sends through rpc(cmd, fields), which
// returns a promise, while alive() is true. open() says whether the socket
// is open now: a request made while it is not never leaves the page.
export function createQueue(rpc, alive = () => true, open = () => true) {
  const panes = new Map();

  // An entry per agent. want is null, or {fit}, where fit is {cols, rows} or
  // null. actual is 'none' when the sheet may hold no attach, 'attached'
  // after an answered attach, and 'maybe' while an attach is unanswered or
  // failed without a refusal, since the server may have run it, or when
  // the sheet takes an agent whose chain may hold the floor's attach. gen counts
  // the sheet's wishes, so a failure of an older one is told apart.
  const entry = (id) => {
    let e = panes.get(id);
    if (!e) {
      e = { id, want: null, actual: 'none', force: false, last: null, tail: Promise.resolve(), busy: 0, owner: 'sheet', gen: 0, epoch: 0, onReply: null, onError: null };
      panes.set(id, e);
    }
    return e;
  };

  // exec runs work behind everything ahead on the entry's chain, at once
  // when nothing is ahead, and returns a promise for this job alone.
  function exec(e, work) {
    e.busy += 1;
    const go = () => Promise.resolve(alive() ? work() : undefined).catch(() => {}).finally(() => { e.busy -= 1; });
    const job = e.busy === 1 ? go() : e.tail.then(go);
    e.tail = job;
    return job;
  }

  async function step(e) {
    if (e.owner !== 'sheet') return;
    if (!e.want) {
      if (e.actual === 'none') return;
      e.actual = 'none';
      e.last = null;
      await rpc('pane.detach', { pane: e.id }).catch(() => {});
      return;
    }
    const fit = e.want.fit;
    if (e.actual === 'attached' && !e.force && same(fit, e.last && e.last.fit)) {
      // Already as wanted: tell the sheet what the last attach answered.
      if (e.onReply && e.last) e.onReply(e.last.res, e.last.fit);
      return;
    }
    e.force = false;
    e.actual = 'maybe';
    const gen = e.gen;
    // handOver counts an epoch, so an attach that was on its way when the
    // floor took the agent is the floor's, whoever wants it by the answer.
    const epoch = e.epoch;
    const req = { pane: e.id };
    if (fit) req.fit = { cols: fit.cols, rows: fit.rows };
    const sendable = open();
    let res;
    try {
      res = await rpc('pane.attach', req);
    } catch (err) {
      // A refusal from the server, or a socket that was not open, means
      // nothing attached. Anything else, a timeout or a socket that closed
      // on the way, may have been a request the server still runs, so it is
      // taken back unless a newer wish wants it.
      if (epoch !== e.epoch) return;
      if ((err && err.code) || !sendable) e.actual = 'none';
      if (gen === e.gen) {
        e.want = null;
        if (e.owner === 'sheet' && e.onError) e.onError(err);
        if (e.actual === 'maybe') exec(e, () => step(e));
      }
      return;
    }
    if (e.owner !== 'sheet' || epoch !== e.epoch) return;
    e.actual = 'attached';
    e.last = { res, fit };
    if (e.want && gen === e.gen && e.onReply) e.onReply(res, fit);
    // A wish that changed while the attach was on its way is reconciled by
    // the step queued for it. Nothing is sent from here.
  }

  return {
    // want asks for the screen of id at fit. onReply(res, fit) hears each
    // answer, onError(err) a failed attach.
    want(id, fit, onReply, onError) {
      const e = entry(id);
      if (e.owner !== 'sheet' || e.busy > 0) {
        // The floor held it, or work is still on the chain: an attach ahead
        // may land. The sheet takes it as maybe attached, so a later leave
        // queues a detach behind it. A spare detach does no harm; an attach
        // nobody owns keeps the agent held.
        if (e.owner !== 'sheet') e.last = null;
        e.owner = 'sheet';
        if (e.actual !== 'attached' || !e.last) e.actual = 'maybe';
      }
      e.gen += 1;
      e.onReply = onReply;
      e.onError = onError;
      e.want = { fit: fit || null };
      exec(e, () => step(e));
    },
    // refit sends the fit again, as when the width changed or someone else
    // came or went: the server decides afresh whether the agent is alone.
    refit(id, fit) {
      const e = panes.get(id);
      if (!e || !e.want) return;
      e.want = { fit: fit || null };
      e.force = true;
      exec(e, () => step(e));
    },
    // leave says nothing is wanted for id. The detach goes behind any
    // request still on its way, and is not sent for an agent known to hold
    // no attach.
    leave(id) {
      const e = panes.get(id);
      if (!e) return Promise.resolve();
      e.gen += 1;
      e.want = null;
      return exec(e, () => step(e));
    },
    // handOver gives the attach to the floor. The chain stays, and a request
    // still on its way does nothing for the sheet when it answers.
    handOver(id) {
      const e = panes.get(id);
      if (!e) return;
      e.owner = 'floor';
      e.epoch += 1;
      e.gen += 1;
      e.want = null;
    },
    // run puts fn on the chain of id: it runs after every request ahead of
    // it has been answered, and not at all once the socket is not the
    // page's. It returns a promise that settles when fn's own promise does.
    run(id, fn) {
      return exec(entry(id), fn);
    },
    // idle settles when every request for id has been answered.
    idle(id) {
      const e = panes.get(id);
      return e ? e.tail : Promise.resolve();
    },
  };
}
