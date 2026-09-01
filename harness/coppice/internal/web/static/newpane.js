// Start a pane from the kitchen. The directory is the one field that must
// not have a default: a pane started in the wrong repository does real
// work in the wrong place.

const MAX_RECENT = 10;

export function createCommand(form) {
  const cwd = (form.cwd || '').trim();
  if (!cwd) throw new Error('Pick a directory first.');
  const req = {
    cmd: 'pane.create',
    cwd,
    label: (form.label || '').trim(),
    kind: form.kind === 'headless' ? 'headless' : 'pty',
    env: {},
  };
  if (req.kind === 'headless') {
    req.harness = form.harness;
    req.cmd_argv = [];
  } else {
    const command = (form.command || '').trim();
    if (!command) throw new Error('Type a command first.');
    req.cmd_argv = command.split(/\s+/);
  }
  return req;
}

// recentCwds is the picker's list: the directories panes are running in
// today, then the ones this phone used before.
export function recentCwds(panes, stored) {
  const out = [];
  for (const cwd of [...(panes || []).map((p) => p.cwd), ...(stored || [])]) {
    if (cwd && !out.includes(cwd)) out.push(cwd);
  }
  return out.slice(0, MAX_RECENT);
}

function readStored() {
  try { return JSON.parse(localStorage.getItem('coppice.cwds') || '[]'); } catch { return []; }
}

export function mountNew() {
  const kind = document.getElementById('new-kind');
  const command = document.getElementById('new-command');
  const commandLabel = document.getElementById('new-command-label');
  const harness = document.getElementById('new-harness');
  const harnessLabel = document.getElementById('new-harness-label');
  const sync = () => {
    const headless = kind.value === 'headless';
    command.hidden = headless;
    commandLabel.hidden = headless;
    harness.hidden = !headless;
    harnessLabel.hidden = !headless;
  };
  kind.addEventListener('change', sync);
  sync();

  window.addEventListener('coppice:route', () => {
    if (window.coppice.route(location.hash).screen !== 'new') return;
    const list = document.getElementById('recent-cwds');
    list.replaceChildren(...recentCwds(window.coppice.state.panes, readStored()).map((cwd) => {
      const o = document.createElement('option');
      o.value = cwd;
      return o;
    }));
  });

  document.getElementById('create').addEventListener('click', async () => {
    let req;
    try {
      req = createCommand({
        cwd: document.getElementById('new-cwd').value,
        label: document.getElementById('new-label').value,
        kind: kind.value,
        command: command.value,
        harness: harness.value,
      });
    } catch (e) {
      window.coppice.status(e.message);
      return;
    }
    const { cmd, ...fields } = req;
    try {
      const result = await window.coppice.rpc(cmd, fields);
      const stored = recentCwds([{ cwd: req.cwd }], readStored());
      localStorage.setItem('coppice.cwds', JSON.stringify(stored));
      location.hash = '#/pane/' + encodeURIComponent(result.pane);
    } catch (e) {
      window.coppice.status(e.message);
    }
  });
}
