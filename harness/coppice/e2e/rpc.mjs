// rpc.mjs: speak coppice's socket protocol the way a gate hook does, so the
// suite can put gate facts on the floor with no real agent behind them.
//
//   node rpc.mjs SOCK report PANE FIELDS_JSON [STATE] [DETAIL]
//   node rpc.mjs SOCK call CMD FIELDS_JSON
//
// report sends hello as the pane, then one pane.report_state event built
// from FIELDS_JSON. call sends one command. Each prints the replies as JSON
// and exits 1 when a reply carries an error.
import net from 'node:net';

const [sock, what, ...args] = process.argv.slice(2);
if (!sock || !what) {
  console.error('usage: node rpc.mjs SOCK report PANE FIELDS_JSON [STATE] [DETAIL] | call CMD FIELDS_JSON');
  process.exit(2);
}

function session(lines) {
  return new Promise((resolve, reject) => {
    const c = net.connect(sock);
    let buf = '';
    const out = [];
    let i = 0;
    const next = () => {
      if (i >= lines.length) {
        c.end();
        resolve(out);
        return;
      }
      c.write(JSON.stringify(lines[i++]) + '\n');
    };
    c.setTimeout(10000, () => reject(new Error('no reply within 10 s')));
    c.on('data', (d) => {
      buf += d;
      let at;
      while ((at = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, at);
        buf = buf.slice(at + 1);
        let m;
        try {
          m = JSON.parse(line);
        } catch {
          continue;
        }
        // Lines with no id are events the server pushes, not replies.
        if (!m.id) continue;
        out.push(m);
        next();
      }
    });
    c.on('error', reject);
    c.on('connect', next);
  });
}

let replies;
if (what === 'report') {
  const [pane, fields, state = 'working', detail = 'verdict=allow'] = args;
  const ev = {
    v: 1,
    ts: Date.now() / 1000,
    session_id: 'e2e-' + pane,
    harness_session_id: null,
    harness: 'claude-code',
    pane,
    state,
    source: 'gate',
    detail,
    ...JSON.parse(fields || '{}'),
  };
  replies = await session([
    { id: 'h', cmd: 'hello', role: 'pane', pane },
    { id: 'r', cmd: 'pane.report_state', pane, event: ev },
  ]);
} else if (what === 'call') {
  const [cmd, fields] = args;
  replies = await session([{ id: 'c', cmd, ...JSON.parse(fields || '{}') }]);
} else {
  console.error(`unknown verb ${what}`);
  process.exit(2);
}
console.log(JSON.stringify(replies));
process.exit(replies.some((r) => r.error || r.ok === false) ? 1 : 0);
