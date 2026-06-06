"""Console-script entry point: locate the bundled observer binary and
exec it, forwarding argv (and on POSIX, signals via process replacement).

Mirrors the @superbased/observer Node shim. Each platform-tagged wheel
ships its own binary under observer/_bin/.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path
from typing import NoReturn


def _binary_path() -> Path:
    name = "observer.exe" if sys.platform == "win32" else "observer"
    return Path(__file__).resolve().parent / "_bin" / name


def _report_missing(path: Path) -> NoReturn:
    sys.stderr.write(
        "superbased-observer: bundled binary not found at "
        f"{path}\n"
        "This usually means the installed wheel is for a different\n"
        "platform than the one Python is running on. Re-install with:\n"
        "  pip install --force-reinstall superbased-observer\n"
        "or report at https://github.com/marmutapp/superbased-observer/issues\n"
    )
    raise SystemExit(1)


def main() -> NoReturn:
    binary = _binary_path()
    if not binary.is_file():
        _report_missing(binary)
    argv = [str(binary), *sys.argv[1:]]
    if sys.platform == "win32":
        import subprocess

        raise SystemExit(subprocess.call(argv))
    os.execv(str(binary), argv)


if __name__ == "__main__":
    main()
