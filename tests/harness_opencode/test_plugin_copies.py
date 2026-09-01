"""The OpenCode gate plugin has copies outside the package: the Go
installer's asset, byte for byte, and coppice's pinned SHA-256, which
coppice checks before it starts an OpenCode pane. A change to the plugin
fails here, in the Python suite, until every copy follows it."""

from __future__ import annotations

import hashlib
import re
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
PLUGIN = REPO / "src" / "opendaisugi" / "harness_opencode" / "plugin" / "daisugi-gate.ts"
GO_ASSET = REPO / "clients" / "go" / "internal" / "install" / "assets" / "opencode-daisugi-gate.ts"
COPPICE = REPO / "harness" / "coppice" / "internal" / "adapters" / "opencode" / "opencode.go"


def test_the_go_asset_is_the_plugin():
    assert GO_ASSET.read_bytes() == PLUGIN.read_bytes()


def test_coppice_pins_the_plugins_hash():
    m = re.search(r'const gatePluginSHA256 = "([0-9a-f]{64})"', COPPICE.read_text(encoding="utf-8"))
    assert m, "coppice's opencode adapter no longer names gatePluginSHA256"
    assert m.group(1) == hashlib.sha256(PLUGIN.read_bytes()).hexdigest()
