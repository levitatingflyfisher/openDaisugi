import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

import { headAllowed } from "../headAllowed.js";
import { pathMatchesAny } from "../pathScopes.js";
import { parseInterpreter } from "../interpreterParse.js";
import { extractShellHead } from "../shellHead.js";
import { SHELL_METACHAR_RE } from "../shellMetachar.js";
import { resolveStrict } from "../resolveStrict.js";
import type { Stakes } from "../models.js";

const here = path.dirname(fileURLToPath(import.meta.url));
const fixturePath = path.resolve(here, "../../../fixtures/semantics.json");
const fixture = JSON.parse(readFileSync(fixturePath, "utf-8"));

test("head_allowed fixture", () => {
  for (const c of fixture.head_allowed) {
    assert.equal(
      headAllowed(c.head, c.allowlist),
      c.allowed,
      `head=${JSON.stringify(c.head)} allowlist=${JSON.stringify(c.allowlist)}`
    );
  }
});

test("path_match fixture", () => {
  for (const c of fixture.path_match) {
    assert.equal(
      pathMatchesAny(c.path, c.globs),
      c.matched,
      `path=${JSON.stringify(c.path)} globs=${JSON.stringify(c.globs)}`
    );
  }
});

test("extract_head fixture", () => {
  for (const c of fixture.extract_head) {
    assert.equal(
      extractShellHead(c.line.trim()),
      c.head,
      `line=${JSON.stringify(c.line)}`
    );
  }
});

test("metachar fixture", () => {
  for (const c of fixture.metachar) {
    assert.equal(
      SHELL_METACHAR_RE.test(c.command),
      c.hit,
      `command=${JSON.stringify(c.command)}`
    );
  }
});

test("interpreter fixture", () => {
  for (const c of fixture.interpreter) {
    const got = parseInterpreter(c.command);
    if (c.payload === null) {
      assert.equal(got, null, `command=${JSON.stringify(c.command)}`);
    } else {
      assert.ok(got, `command=${JSON.stringify(c.command)} expected non-null`);
      assert.equal(got!.head, c.payload.head);
      assert.equal(got!.opaque, c.payload.opaque);
      assert.deepEqual(got!.innerCommands, c.payload.inner_commands);
    }
  }
});

test("resolve_strict fixture", () => {
  for (const c of fixture.resolve_strict) {
    assert.equal(
      resolveStrict(c.strict, c.stakes as Stakes),
      c.effective,
      `strict=${c.strict} stakes=${c.stakes}`
    );
  }
});
