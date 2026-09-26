#!/usr/bin/env python3
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Fail when a reference to a docs/OPERATIONS.md section does not resolve.

OPERATIONS.md is cited by section number from code comments, Helm templates,
migrations, ADRs, CONTRACT-FINDINGS and CI definitions. Sections get
renumbered when branches that each added "the next section" are merged, and
a stale "§20" then silently points at the wrong runbook. This check finds, in
every git-tracked text file:

  * "OPERATIONS[.md] §N[.M...]" and "OPERATIONS[.md] section N[.M...]", plus
    the §-chain that follows on the same line ("OPERATIONS §12 and §20.6");
  * "OPERATIONS[.md] N.M" (the Makefile's short form);
  * "OPERATIONS.md#anchor" links, checked against GitHub's heading slugs;
  * inside OPERATIONS.md itself, every bare "§N[.M]";

and requires each number to be a heading of OPERATIONS.md ("## N." or
"### N.M"). A reference to a whole section (§N) resolves if "## N." exists.

Usage: deploy/scripts/check-operations-refs.py [--verbose]
Exit 0 when every reference resolves, 1 otherwise (each listed as file:line).
"""

import re
import subprocess
import sys

DOC = "docs/OPERATIONS.md"
NUM = r"\d+(?:\.\d+)*"
# "OPERATIONS" (optionally ".md", optionally "docs/"), then the reference.
PREFIX = r"OPERATIONS(?:\.md)?[,)]?\s+"
SECTION_RE = re.compile(PREFIX + r"(?:§\s?|section\s+)(" + NUM + r")")
CHAIN_RE = re.compile(r"^(?:[,;]?\s*(?:and|or|&|/|,)?\s*§\s?(" + NUM + r"))")
SHORT_RE = re.compile(r"OPERATIONS(?:\.md)?\s+(\d+\.\d+(?:\.\d+)*)\b")
ANCHOR_RE = re.compile(r"OPERATIONS\.md#([A-Za-z0-9_-]+)")
BARE_RE = re.compile(r"§\s?(" + NUM + r")")
OTHER_DOC_RE = re.compile(r"(?:TLS|ARCHITECTURE|CONTRACT|CLUSTER-SCALING|MULTITENANCY|ADR-\d+|RFC \d+)(?:\.md)?[^§]*$")


def headings(text):
    nums, slugs = set(), set()
    in_code = False
    for line in text.splitlines():
        if line.startswith("```"):
            in_code = not in_code
            continue
        if in_code:
            continue
        m = re.match(r"^(#{2,4})\s+(.*)$", line)
        if not m:
            continue
        title = m.group(2).strip()
        n = re.match(r"^(" + NUM + r")\.?\s", title)
        if n:
            nums.add(n.group(1))
        slug = re.sub(r"[^\w\- ]", "", title.lower()).replace(" ", "-")
        slugs.add(slug)
    return nums, slugs


def resolves(num, nums):
    return num in nums or num.rstrip(".") in nums


def main():
    verbose = "--verbose" in sys.argv
    doc = open(DOC, encoding="utf-8").read()
    nums, slugs = headings(doc)
    files = subprocess.run(["git", "ls-files"], capture_output=True, text=True, check=True).stdout.split()
    bad, seen = [], 0
    for path in files:
        # This script's own docstring spells out the reference syntax
        # ("OPERATIONS.md#anchor"), which is not a reference.
        if path == "deploy/scripts/check-operations-refs.py":
            continue
        if path.startswith(("ui/node_modules/", "gen/")) or path.endswith((".png", ".jpg", ".svg", ".ico", ".woff2", ".lock")):
            continue
        try:
            text = open(path, encoding="utf-8").read()
        except (UnicodeDecodeError, FileNotFoundError, IsADirectoryError):
            continue
        in_code = False
        for i, line in enumerate(text.splitlines(), 1):
            refs = []
            for m in SECTION_RE.finditer(line):
                refs.append(m.group(1))
                rest = line[m.end():]
                while True:
                    c = CHAIN_RE.match(rest)
                    if not c:
                        break
                    refs.append(c.group(1))
                    rest = rest[c.end():]
            refs += [m.group(1) for m in SHORT_RE.finditer(line)]
            if path == DOC:
                if line.startswith("```"):
                    in_code = not in_code
                # Bare "§N" in the runbook is its own section, unless another
                # document is named just before it ("TLS.md §5.5",
                # "ARCHITECTURE §5.2") or it is in a heading, where "(§5.4)"
                # cites the architecture.
                if not in_code and not line.startswith("#"):
                    for m in BARE_RE.finditer(line):
                        if not OTHER_DOC_RE.search(line[max(0, m.start() - 24):m.start()]):
                            refs.append(m.group(1))
            for r in refs:
                seen += 1
                if not resolves(r, nums):
                    bad.append(f"{path}:{i}: OPERATIONS §{r} does not exist")
                elif verbose:
                    print(f"ok {path}:{i}: §{r}")
            for m in ANCHOR_RE.finditer(line):
                seen += 1
                if m.group(1).lower() not in slugs:
                    bad.append(f"{path}:{i}: OPERATIONS.md#{m.group(1)} matches no heading")
    for b in bad:
        print(b)
    print(f"check-operations-refs: {seen} references, {len(bad)} unresolved "
          f"({len(nums)} numbered headings in {DOC})")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
