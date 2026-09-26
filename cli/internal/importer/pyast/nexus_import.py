# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Static analysis of Airflow DAG files and Prefect flow files for `nexusctl import`.

    python3 nexus_import.py airflow|prefect FILE...

Prints ONE JSON document on stdout: the intermediate representation (IR) that
the Go side of nexusctl turns into WorkflowSpecs and a report.

This program NEVER imports, executes or evals the files it analyses. It parses
them with `ast` and evaluates only literals and an allow-list of pure
constructors (timedelta, datetime, textwrap.dedent, a few builtins over
literals). Anything else -- a loop that creates tasks, a Variable.get(), a value
computed at run time -- is reported as unsupported with its line, not guessed.
That is the guarantee `workflow compile` makes too: only the user's own process
runs the user's code, and a converter is not the user's process.

Standard library only; Python >= 3.8. nexusctl embeds this file and its
siblings (nx_values.py, nx_core.py, nx_airflow.py, nx_prefect.py) and runs them with the
interpreter named by --python.
"""

from __future__ import annotations

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
sys.dont_write_bytecode = True

from nx_airflow import Airflow  # noqa: E402
from nx_prefect import Prefect  # noqa: E402

IR_VERSION = 1


def main(argv):
    if len(argv) < 2 or argv[0] not in ("airflow", "prefect"):
        sys.stderr.write("usage: nexus_import.py airflow|prefect FILE...\n")
        return 2
    mode, paths = argv[0], argv[1:]
    cls = Airflow if mode == "airflow" else Prefect
    out = {"ir_version": IR_VERSION, "source": mode, "python": sys.version.split()[0], "files": []}
    for p in paths:
        entry = {"path": p, "workflows": [], "findings": []}
        try:
            with open(p, "r", encoding="utf-8") as fh:
                src = fh.read()
        except (OSError, UnicodeDecodeError) as e:
            entry["error"] = "cannot read %s: %s" % (p, e)
            out["files"].append(entry)
            continue
        a = cls(p, src, mode)
        try:
            a.run()
        except RecursionError:
            entry["error"] = "%s is too deeply nested to analyse" % p
            out["files"].append(entry)
            continue
        entry["workflows"] = [w.to_ir() for w in a.workflows]
        entry["findings"] = a.findings
        out["files"].append(entry)
    json.dump(out, sys.stdout, sort_keys=True)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
