#!/usr/bin/env python3
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Summarise JUnit XML test reports as Markdown, for a CI job summary.

GitHub Actions has no test-results tab of its own. So every CI job that
writes a JUnit report appends this summary to $GITHUB_STEP_SUMMARY -- totals
per report, then the failing tests by name -- and uploads the report itself as
an artifact.

Usage:
    junit-summary.py --title TITLE [--require] REPORT.xml... >> "$GITHUB_STEP_SUMMARY"

The counts come from the <testcase> elements, not from the suite attributes,
which not every reporter fills in; time is the sum of the test cases' own
times. The exit status is 0 whatever the tests did: the test step owns that
verdict. With --require it is 1 when a report is missing or unreadable,
because a suite that wrote no report proved nothing.
"""

import argparse
import os
import sys
import xml.etree.ElementTree as ET

# Enough to find the failure in the summary; the full list is in the report.
MAX_LISTED = 50


def summarise(path):
    """Return (tests, failed, skipped, seconds, [failed test names])."""
    root = ET.parse(path).getroot()
    tests = failed = skipped = 0
    seconds = 0.0
    names = []
    for case in root.iter("testcase"):
        tests += 1
        try:
            seconds += float(case.get("time") or 0)
        except ValueError:
            pass
        if case.find("failure") is not None or case.find("error") is not None:
            failed += 1
            cls, name = case.get("classname") or "", case.get("name") or "?"
            names.append(f"{cls} › {name}" if cls else name)
        elif case.find("skipped") is not None:
            skipped += 1
    return tests, failed, skipped, seconds, names


def duration(seconds):
    minutes, secs = divmod(int(round(seconds)), 60)
    return f"{minutes}m {secs:02d}s" if minutes else f"{seconds:.1f}s"


def code(text):
    # An inline code span must not be cut short by its own content.
    return text.replace("`", "'").replace("\n", " ")


def cell(text):
    # Nor may a table cell.
    return code(text).replace("|", "\\|")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--title", required=True)
    ap.add_argument("--require", action="store_true", help="fail when a report is missing or unreadable")
    ap.add_argument("reports", nargs="+")
    a = ap.parse_args()

    rows, failures, problems = [], [], []
    for path in a.reports:
        label = os.path.basename(path)
        if not os.path.isfile(path):
            problems.append(f"{path}: no report was written")
            continue
        try:
            tests, failed, skipped, seconds, names = summarise(path)
        except (ET.ParseError, OSError) as e:
            problems.append(f"{path}: unreadable: {e}")
            continue
        status = "❌" if failed else "✅"
        rows.append(f"| {status} {cell(label)} | {tests} | {tests - failed - skipped} | {failed} | {skipped} | {duration(seconds)} |")
        failures += names

    print(f"### {a.title}\n")
    if rows:
        print("| report | tests | passed | failed | skipped | test time |")
        print("|---|---:|---:|---:|---:|---:|")
        print("\n".join(rows))
        print()
    if failures:
        print(f"**Failed ({len(failures)}):**\n")
        for name in failures[:MAX_LISTED]:
            print(f"- `{code(name)}`")
        if len(failures) > MAX_LISTED:
            print(f"- … and {len(failures) - MAX_LISTED} more (see the uploaded report)")
        print()
    for p in problems:
        print(f"> ⚠️ {code(p)}\n")
        print(f"junit-summary: {p}", file=sys.stderr)
    return 1 if a.require and problems else 0


if __name__ == "__main__":
    sys.exit(main())
