"""The console entry: ``daisugi`` runs ``main`` here.

``daisugi --version`` must answer at once. The real CLI (``opendaisugi.cli``)
imports typer, click, yaml, asyncio and pydantic at the top, and that is most
of a third of a second. So this module checks for a lone ``--version`` first,
with no import beyond the package itself, and loads the CLI only for anything
else. The text is the one the CLI prints: the version and a newline.
"""

from __future__ import annotations

import sys


def main() -> None:
    if sys.argv[1:] == ["--version"]:
        from opendaisugi import __version__

        sys.stdout.write(f"{__version__}\n")
        sys.stdout.flush()
        raise SystemExit(0)
    from opendaisugi.cli import main as cli_main

    cli_main()


if __name__ == "__main__":  # enable `python -m opendaisugi.cli_entry ...`
    main()
