import test from 'node:test';
import assert from 'node:assert/strict';
import './browser-stub.mjs';
import { changedLine } from '../floor.js';

// FN-R-13: a fenced run that changed a git hook or Claude's project
// settings shows on its pane.
test('changedLine names the changed workspace files', () => {
  assert.equal(changedLine(['.git/hooks/pre-commit', '.git/config']), 'workspace config changed: .git/hooks/pre-commit, .git/config');
  assert.equal(changedLine([]), '');
  assert.equal(changedLine(undefined), '');
  assert.equal(changedLine([3, '', '.claude/settings.json']), 'workspace config changed: .claude/settings.json');
});
