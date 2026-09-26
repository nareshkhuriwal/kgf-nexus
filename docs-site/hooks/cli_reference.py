# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Generate the nexusctl reference from the binary's own help text.

The reference is the output of ``nexusctl help ...`` for every command, taken
from a binary built from this checkout at build time. There is no hand-kept
copy to drift: if a flag is added, the next ``make docs`` shows it.

The binary is taken from ``$NEXUS_DOCS_NEXUSCTL`` when set (``make docs``
builds it once), otherwise built here with ``go build`` into a temporary
directory.

Commands are discovered by walking the command tree the same way shell
completion does (``nexusctl __complete``), and a command is a group when its
help lists ``Commands:``. The hidden ``__complete`` and ``help`` are skipped.
"""

from __future__ import annotations

import logging
import os
import re
import subprocess
import tempfile
from pathlib import Path

from mkdocs.structure.files import File, Files

log = logging.getLogger("mkdocs.plugins.nexus.cli_reference")

SITE_PATH = "reference/cli.md"
SKIP = {"help", "__complete"}


def _repo_root(config) -> Path:
    return Path(config["config_file_path"]).resolve().parent.parent


def _binary(config, tmp: str) -> str:
    env = os.environ.get("NEXUS_DOCS_NEXUSCTL")
    if env:
        return env
    out = os.path.join(tmp, "nexusctl")
    subprocess.run(["go", "build", "-o", out, "./cli/cmd/nexusctl"], cwd=_repo_root(config), check=True)
    return out


def _run(binary: str, *args: str) -> str:
    # No endpoint, key or config file: help must never need one, and a
    # developer's ~/.nexus must not leak into the published reference.
    env = {"PATH": os.environ.get("PATH", ""), "HOME": tempfile.gettempdir(), "NEXUS_CONFIG": os.devnull}
    p = subprocess.run([binary, *args], capture_output=True, text=True, env=env, timeout=30)
    text = (p.stdout + p.stderr).rstrip()
    if p.returncode != 0:
        raise RuntimeError(f"nexusctl {' '.join(args)} exited {p.returncode}: {text[:400]}")
    return text


def _subcommands(help_text: str) -> list[str]:
    m = re.search(r"^Commands:\n((?:  \S.*\n?)+)", help_text, re.M)
    if not m:
        return []
    return [line.split()[0] for line in m.group(1).splitlines() if line.strip()]


def _fence(text: str) -> str:
    return "```text\n" + text + "\n```\n"


def generate(binary: str) -> str:
    version = _run(binary, "version")
    top = [w for w in _run(binary, "__complete", "").split() if w not in SKIP]
    out = [
        "# nexusctl reference",
        "",
        "Generated from `nexusctl help` of the binary built from this checkout "
        f"(`{version.splitlines()[0]}`). Do not edit: change the help text in "
        "[cli/internal/cmd](repo:cli/internal/cmd) and rebuild the site.",
        "",
        "Configuration precedence, destructive-command confirmation and the "
        "quick-start internals are described in the [CLI README](repo:cli/README.md).",
        "",
        "## Overview",
        "",
        _fence(_run(binary, "help")),
    ]
    for name in sorted(top):
        text = _run(binary, "help", name)
        out += [f"## `{name}`", "", _fence(text)]
        for sub in _subcommands(text):
            out += [f"### `{name} {sub}`", "", _fence(_run(binary, "help", name, sub))]
    return "\n".join(out)


def on_files(files: Files, config, **_):
    if files.get_file_from_path(SITE_PATH) is not None:
        raise ValueError(f"cli_reference hook: {SITE_PATH} is generated; remove the hand-written copy")
    with tempfile.TemporaryDirectory() as tmp:
        content = generate(_binary(config, tmp))
    files.append(File.generated(config, SITE_PATH, content=content))
    return files
