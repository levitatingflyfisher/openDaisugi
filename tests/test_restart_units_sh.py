"""scripts/restart-units.sh: install.sh restarts what it replaced.

A fake systemctl on PATH answers is-active and show from files in a
scratch HOME and records every call; a fake coppice reports the live
panes. Nothing real is started or stopped.
"""

from __future__ import annotations

import subprocess
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
SCRIPT = REPO / "scripts" / "restart-units.sh"
INSTALL = REPO / "scripts" / "install.sh"

FAKE_SYSTEMCTL = """#!/bin/sh
echo "$*" >> "$HOME/systemctl.log"
[ "$1" = --user ] || exit 9
shift
case "$1" in
  is-active) [ -e "$HOME/active/$3" ] ;;
  show) cat "$HOME/execstart/$5" 2>/dev/null ;;
  restart) [ ! -e "$HOME/fail/$2" ] ;;
  *) exit 9 ;;
esac
"""

# Like the real binary: with no server on the socket it starts one, unless
# COPPICE_NO_AUTOSTART is set (review r9, finding 3).
FAKE_COPPICE = """#!/bin/sh
echo "$*" >> "$HOME/coppice.log"
if [ ! -e "$HOME/live" ]; then
  if [ -z "$COPPICE_NO_AUTOSTART" ]; then echo started >> "$HOME/autostart.log"; echo 0 > "$HOME/live"; else exit 1; fi
fi
printf '{"panes":3,"panes_live":%s,"pid":7}\\n' "$(cat "$HOME/live")"
"""


def _world(tmp_path: Path) -> tuple[Path, Path, dict[str, str]]:
    home = tmp_path / "home"
    fakes = tmp_path / "fakes"
    bin_ = home / ".local" / "bin"
    for d in (home / "active", home / "execstart", home / "fail", fakes, bin_):
        d.mkdir(parents=True)
    (fakes / "systemctl").write_text(FAKE_SYSTEMCTL)
    (fakes / "systemctl").chmod(0o755)
    (bin_ / "coppice").write_text(FAKE_COPPICE)
    (bin_ / "coppice").chmod(0o755)
    env = {"HOME": str(home), "PATH": f"{fakes}:/usr/bin:/bin"}
    return home, bin_, env


def _unit(home: Path, unit: str, argv: str, active: bool = True) -> None:
    exe = argv.split()[0]
    (home / "execstart" / unit).write_text(
        f"{{ path={exe} ; argv[]={argv} ; ignore_errors=no ; start_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }}\n"
    )
    if active:
        (home / "active" / unit).write_text("")


def _run(bin_: Path, env: dict[str, str], *names: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(  # noqa: S603 - the script under test
        ["/bin/bash", str(SCRIPT), str(bin_), *names],
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )


def _log(home: Path, name: str) -> str:
    p = home / name
    return p.read_text() if p.exists() else ""


def test_the_follow_service_restarts_when_daisugi_is_replaced(tmp_path):
    home, bin_, env = _world(tmp_path)
    _unit(home, "daisugi-pando.service", f"{bin_}/daisugi pando follow")
    r = _run(bin_, env, "coppice", "sprig", "daisugi")
    assert r.returncode == 0
    assert r.stdout == "restarted daisugi-pando.service: it runs the new daisugi.\n"
    assert "--user restart daisugi-pando.service" in _log(home, "systemctl.log")


def test_an_idle_coppice_server_restarts_with_its_own_socket(tmp_path):
    home, bin_, env = _world(tmp_path)
    _unit(
        home,
        "coppice-server.service",
        f"{bin_}/coppice --socket /s/server.sock --data-dir /d server start --foreground",
    )
    (home / "live").write_text("0")
    r = _run(bin_, env, "coppice")
    assert r.stdout == "restarted coppice-server.service: it runs the new coppice.\n"
    assert (
        _log(home, "coppice.log") == "--socket /s/server.sock --data-dir /d server status --json\n"
    )
    assert "--user restart coppice-server.service" in _log(home, "systemctl.log")


def test_a_coppice_server_with_live_agents_is_not_restarted(tmp_path):
    home, bin_, env = _world(tmp_path)
    _unit(home, "coppice-server.service", f"{bin_}/coppice server start --foreground")
    (home / "live").write_text("2")
    r = _run(bin_, env, "coppice")
    assert r.stdout == (
        "coppice-server.service: not restarted: agents run in it (2 now), and a restart ends them. "
        "When they finish: systemctl --user restart coppice-server.service\n"
    )
    assert "restart" not in _log(home, "systemctl.log").replace("is-active", "")


def test_a_coppice_server_that_does_not_answer_is_not_restarted(tmp_path):
    home, bin_, env = _world(tmp_path)
    _unit(home, "coppice-server.service", f"{bin_}/coppice server start --foreground")
    r = _run(bin_, env, "coppice")
    assert "not restarted: its server does not answer, and this starts none" in r.stdout
    assert "--user restart" not in _log(home, "systemctl.log")
    assert _log(home, "autostart.log") == "", "the status call started a server"


def test_an_inactive_or_missing_unit_prints_nothing(tmp_path):
    home, bin_, env = _world(tmp_path)
    _unit(home, "daisugi-pando.service", f"{bin_}/daisugi pando follow", active=False)
    r = _run(bin_, env, "daisugi", "coppice")
    assert r.returncode == 0
    assert r.stdout == ""
    assert "--user restart" not in _log(home, "systemctl.log")


def test_a_unit_that_runs_another_binary_is_left_alone(tmp_path):
    home, bin_, env = _world(tmp_path)
    _unit(home, "daisugi-pando.service", "/usr/bin/daisugi pando follow")
    r = _run(bin_, env, "daisugi")
    assert (
        r.stdout
        == f"daisugi-pando.service: not restarted: it runs /usr/bin/daisugi, not {bin_}/daisugi.\n"
    )
    assert "--user restart" not in _log(home, "systemctl.log")


def test_a_unit_whose_binary_was_kept_is_not_restarted(tmp_path):
    # install.sh keeps a Python daisugi script unless --replace-daisugi:
    # it then passes no daisugi, and the follow service stays as it is.
    home, bin_, env = _world(tmp_path)
    _unit(home, "daisugi-pando.service", f"{bin_}/daisugi pando follow")
    r = _run(bin_, env, "coppice", "sprig")
    assert r.stdout == ""


def test_a_failed_restart_says_how_to_do_it(tmp_path):
    home, bin_, env = _world(tmp_path)
    _unit(home, "daisugi-pando.service", f"{bin_}/daisugi pando follow")
    (home / "fail" / "daisugi-pando.service").write_text("")
    r = _run(bin_, env, "daisugi")
    assert r.returncode == 0
    assert (
        r.stdout
        == "daisugi-pando.service: the restart failed. Run: systemctl --user restart daisugi-pando.service\n"
    )


def test_no_systemctl_prints_nothing(tmp_path):
    home, bin_, env = _world(tmp_path)
    env["PATH"] = str(tmp_path / "empty")
    r = subprocess.run(  # noqa: S603 - the script under test
        ["/bin/bash", str(SCRIPT), str(bin_), "daisugi"],
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )
    assert r.returncode == 0
    assert r.stdout == ""


def test_install_sh_names_the_switch_and_calls_the_script():
    text = INSTALL.read_text(encoding="utf-8")
    head = text.split("set -euo pipefail", 1)[0]
    assert "--no-restart" in head
    assert '"$root/scripts/restart-units.sh" "$bin" "${installed[@]}"' in text
    assert "--no-restart) restart=0 ;;" in text


def test_a_linked_install_restarts_a_unit_that_names_the_link_target(tmp_path):
    # install.sh --link puts a link in BIN; coppice server install writes
    # the resolved build path into ExecStart (review r9, finding 4).
    home, bin_, env = _world(tmp_path)
    build = tmp_path / "build"
    build.mkdir()
    (bin_ / "coppice").rename(build / "coppice")
    (bin_ / "coppice").symlink_to(build / "coppice")
    (bin_ / "daisugi").symlink_to(build / "daisugi")
    (build / "daisugi").write_text("#!/bin/sh\n")
    _unit(home, "coppice-server.service", f"{build}/coppice server start --foreground")
    _unit(home, "daisugi-pando.service", f"{bin_}/daisugi pando follow")
    (home / "live").write_text("0")
    r = _run(bin_, env, "coppice", "daisugi")
    assert r.stdout == (
        "restarted coppice-server.service: it runs the new coppice.\n"
        "restarted daisugi-pando.service: it runs the new daisugi.\n"
    )
