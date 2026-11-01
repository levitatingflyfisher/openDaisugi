// envcard.js: the cards that show an envelope the steward proposed. The
// owner reads what the project may read, write, run and reach, and its
// budget, and approves or declines. Approve writes the file on the server,
// as the operator. The steward cannot.

const make = (tag, cls, text) => {
  const el = document.createElement(tag);
  if (cls) el.className = cls;
  if (text !== undefined) el.textContent = text;
  return el;
};

// list is "none" or the items, joined.
const list = (items) => (Array.isArray(items) && items.length ? items.join(', ') : 'nothing');

// runLine says what may run.
export function runLine(run) {
  const r = run || {};
  const parts = [];
  if (r.shell) parts.push(Array.isArray(r.allow) && r.allow.length ? 'shell: ' + r.allow.join(', ') : 'any shell command');
  if (Array.isArray(r.mcp) && r.mcp.length) parts.push('tools: ' + r.mcp.join(', '));
  return parts.length ? parts.join('; ') : 'nothing';
}

// reachLine says what may be reached on the network.
export function reachLine(reach) {
  const r = reach || {};
  if (!r.network) return 'no network';
  return Array.isArray(r.hosts) && r.hosts.length ? r.hosts.join(', ') : 'any host';
}

// budgetLine says the budget, or that there is none.
export function budgetLine(p) {
  const parts = [];
  if (p.tokens > 0) parts.push(p.tokens.toLocaleString('en-US') + ' tokens');
  if (p.turns > 0) parts.push(p.turns + ' turns');
  return parts.length ? parts.join(', ') : 'no budget set';
}

// card draws one proposal. act has approve(p) and decline(p).
export function envelopeCard(p, act) {
  const li = make('li', 'card envelope');
  li.dataset.project = p.project;
  li.append(make('strong', 'label', 'Envelope for ' + p.project));
  if (p.replaces) li.append(make('span', 'muted', ' (replaces the one in use)'));
  if (p.task) li.append(make('p', 'muted', p.task));
  // What it asks for past the project's default permission: the reason
  // there is a card at all.
  if (Array.isArray(p.wider) && p.wider.length) {
    li.append(make('p', 'envelope-wider', 'Asks for more than the default:'));
    const ul = make('ul', 'envelope-wider');
    for (const w of p.wider) ul.append(make('li', '', w));
    li.append(ul);
  }
  const rows = [['May read', list(p.read)], ['May write', list(p.write)], ['May run', runLine(p.run)],
    ['May reach', reachLine(p.reach)], ['Budget', budgetLine(p)]];
  // Everything else that widens what the project may do.
  if (Array.isArray(p.also) && p.also.length) rows.push(['Also', p.also.join('; ')]);
  if (Array.isArray(p.denied) && p.denied.length) rows.push(['Still denied', p.denied.join(', ')]);
  const dl = make('dl', 'envelope-facts');
  for (const [k, v] of rows) {
    dl.append(make('dt', '', k), make('dd', '', v));
  }
  li.append(dl);
  const bar = make('p', 'envelope-actions');
  const approve = make('button', 'fix', 'Approve');
  approve.type = 'button';
  approve.addEventListener('click', () => act.approve(p));
  const decline = make('button', 'ghost', 'Decline');
  decline.type = 'button';
  decline.addEventListener('click', () => act.decline(p));
  bar.append(approve, decline);
  li.append(bar);
  return li;
}

// mountEnvelopes keeps the list el in step with envelope.pending. read()
// asks the server; it runs when the page opens and on every beat. A read
// that shows the same cards draws nothing.
export function mountEnvelopes({ el, rpc, connected, onNote }) {
  let sig = '';
  const draw = (cards) => {
    const next = JSON.stringify(cards);
    el.hidden = cards.length === 0;
    if (next === sig) return;
    sig = next;
    const act = {
      approve: async (p) => { await answer('envelope.approve', p); },
      decline: async (p) => { await answer('envelope.decline', p); },
    };
    el.replaceChildren(...cards.map((p) => envelopeCard(p, act)));
  };
  const answer = async (cmd, p) => {
    try {
      // at names the proposal that was read, so a replaced one is not approved by a stale card.
      await rpc(cmd, { project: p.project, at: p.at });
      if (onNote) onNote(cmd === 'envelope.approve' ? 'Approved the envelope for ' + p.project + '.' : 'Declined the envelope for ' + p.project + '.');
    } catch (e) {
      if (onNote) onNote((e && e.message) || 'The envelope was not answered.');
    }
    await read();
  };
  const read = async () => {
    if (connected && !connected()) return;
    try {
      const r = await rpc('envelope.pending', {});
      draw(Array.isArray(r && r.proposals) ? r.proposals : []);
    } catch {
      // A named sign-in or an older server has none to show.
      draw([]);
    }
  };
  return { read };
}
