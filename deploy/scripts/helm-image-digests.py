#!/usr/bin/env python3
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Turn a release digest map into chart values, and prove the chart uses them.

A release publishes a digest map, one entry per image it built:

    {"api-gateway": "ghcr.io/<owner>/kgf-nexus/api-gateway@sha256:...", "agent": ..., ...}

This repository's release workflow (.github/workflows/release.yml) publishes
one for GHCR; KGF Cloud's own release pipeline publishes one of the same shape
for its ACR (<acr>/nexus/<image>).

`collect` assembles that map from one file per image, as the release
workflow's build jobs leave them, and refuses it unless it is exactly the
expected image set, every reference is digest-pinned and all of them share one
repository base.

`values` turns it into a Helm values file: image.registry and
image.repositoryPrefix from the common repository base, and image.digests keyed
by IMAGE name, the key the chart's nexus.image helper reads. (It used to be
passed as `services.<image-name>.image.digest`; the chart's `services` keys are
camelCase and nothing read a digest there, so every release ran by tag.)

`check` renders the chart with those values (and any others) through
`helm template` and FAILS unless:

  * every rendered image under the release's repository base is in the map and
    pinned to exactly the map's digest, and
  * every image in the map is rendered somewhere, except the ones named with
    --not-in-chart (images another tool deploys, e.g. cluster-controller, which
    the kustomize base in deploy/k8s/cluster-controller releases).

Usage:
    helm-image-digests.py collect DIR --expect NAME[,NAME...] > digests.json
    helm-image-digests.py values DIGESTS_JSON > release-images.yaml
    helm-image-digests.py check --chart deploy/helm/kgf-nexus DIGESTS_JSON \\
        [--values FILE]... [--set K=V]... [--not-in-chart NAME]...
    helm-image-digests.py self-test --chart deploy/helm/kgf-nexus

DIGESTS_JSON is a path, or the JSON text itself when it starts with "{".
`self-test` builds a synthetic map for every image the chart renders by
default and runs `check` on it, then proves `check` fails when a digest is
dropped or altered. CI runs it, so a template that stops honouring
image.digests fails before any release does.

Output is JSON (valid YAML), so no YAML library is needed.
"""

import argparse
import json
import os
import re
import subprocess
import sys
import tempfile

DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
# `image: <ref>` and `- image: <ref>` in rendered manifests, optionally quoted.
IMAGE_LINE_RE = re.compile(r"""^\s*(?:-\s+)?image:\s*["']?([^"'\s#]+)["']?\s*$""")


def fail(msg):
    print(f"helm-image-digests: {msg}", file=sys.stderr)
    sys.exit(1)


def load_digests(arg):
    text = arg if arg.lstrip().startswith("{") else open(arg, encoding="utf-8").read()
    try:
        data = json.loads(text)
    except json.JSONDecodeError as e:
        fail(f"digest map is not JSON: {e}")
    if not isinstance(data, dict) or not data:
        fail("digest map must be a non-empty JSON object")
    return data


def split_map(digests):
    """Return (base, {name: digest}) or fail with every problem found."""
    bases, out, problems = set(), {}, []
    for name, ref in sorted(digests.items()):
        if not isinstance(ref, str) or "@" not in ref:
            problems.append(f"{name}: not a digest-pinned reference: {ref!r}")
            continue
        repo, digest = ref.split("@", 1)
        if not DIGEST_RE.match(digest):
            problems.append(f"{name}: malformed digest {digest!r}")
            continue
        repo = repo.rsplit(":", 1)[0] if ":" in repo.rsplit("/", 1)[-1] else repo
        if not repo.endswith("/" + name):
            problems.append(f"{name}: repository {repo!r} does not end in /{name}")
            continue
        bases.add(repo[: -len(name)])
        out[name] = digest
    if len(bases) > 1:
        problems.append(f"images come from different repository bases: {sorted(bases)}")
    if problems:
        fail("the digest map is not usable:\n  " + "\n  ".join(problems))
    return bases.pop(), out


def collect(directory, expect):
    """Return the map assembled from DIRECTORY or fail with every problem found.

    Each regular file in DIRECTORY is named for one image and holds that image's
    digest-pinned reference. A build job that failed or never ran leaves no
    file, which is the missing case below; a map with a hole in it must never
    reach a release, because whatever deploys it would run that image by tag.
    """
    found, empty, problems = {}, set(), []
    for name in sorted(os.listdir(directory)):
        path = os.path.join(directory, name)
        if not os.path.isfile(path):
            continue
        with open(path, encoding="utf-8") as fh:
            ref = fh.read().strip()
        if ref:
            found[name] = ref
        else:
            empty.add(name)
            problems.append(f"{name}: the file is empty (did its push step run?)")
    for name in sorted(set(expect) - set(found) - empty):
        problems.append(f"{name}: no digest was published (did its build job run?)")
    for name in sorted(set(found) - set(expect)):
        problems.append(f"{name}: not in the expected image set {sorted(expect)}")
    if problems:
        fail("the digest map is not releasable:\n  " + "\n  ".join(problems))
    split_map(found)  # every reference pinned, named for its image, one base
    return found


def values_for(digests):
    base, pinned = split_map(digests)
    # Split "<host>/<path>/" into registry "<host>/" and prefix "<path>/", the
    # chart's two knobs; their concatenation is all that matters.
    host, _, rest = base.partition("/")
    return {"image": {"registry": host + "/", "repositoryPrefix": rest, "digests": pinned}}


def parse_ref(ref):
    repo, _, digest = ref.partition("@")
    last = repo.rsplit("/", 1)[-1]
    if ":" in last:
        repo = repo[: len(repo) - len(last)] + last.split(":", 1)[0]
    return repo, digest


def render(chart, values_files, sets):
    cmd = ["helm", "template", "digest-check", chart]
    for f in values_files:
        cmd += ["--values", f]
    for s in sets:
        cmd += ["--set", s]
    res = subprocess.run(cmd, capture_output=True, text=True)
    if res.returncode != 0:
        fail(f"helm template failed:\n{res.stderr.strip()}")
    return [m.group(1) for line in res.stdout.splitlines() if (m := IMAGE_LINE_RE.match(line))]


def check(chart, digests, values_files, sets, not_in_chart, quiet=False):
    """Return a list of problems (empty = pass)."""
    base, pinned = split_map(digests)
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as fh:
        json.dump(values_for(digests), fh)
        release_values = fh.name
    # The release values go LAST so an environment file cannot silently undo them,
    # exactly as the pipeline orders them.
    images = render(chart, list(values_files) + [release_values], sets)
    problems, seen, bad = [], set(), set()
    for ref in images:
        repo, digest = parse_ref(ref)
        if not repo.startswith(base):
            continue  # a third-party image (postgres, nats, silo, ...)
        name = repo[len(base):]
        seen.add(name)
        if name not in pinned:
            problems.append(f"{ref}: rendered from the release repository but {name!r} is not in the digest map")
            bad.add(name)
        elif digest != pinned[name]:
            problems.append(f"{ref}: expected @{pinned[name]}")
            bad.add(name)
    for name in sorted(set(pinned) - seen - set(not_in_chart)):
        problems.append(f"{name}: in the digest map but no rendered image uses {base}{name} "
                        "(chart does not deploy it, or its repository base differs)")
    if not quiet:
        for name in sorted((seen & set(pinned)) - bad):
            print(f"  ok  {name:<20} {base}{name}@{pinned[name][:19]}...")
        for name in sorted(set(not_in_chart) & set(pinned) - seen):
            print(f"  --  {name:<20} not deployed by this chart (--not-in-chart)")
    return list(dict.fromkeys(problems))  # one line per distinct problem


def self_test(chart, sets):
    base = "digest-check.example/nexus/"
    fake = lambda i: "sha256:" + f"{i:064x}"
    probe = render(chart, [], sets)
    names = sorted({parse_ref(r)[0].rsplit("/", 1)[-1] for r in probe if parse_ref(r)[0].startswith("kgf-nexus/")})
    if not names:
        fail("self-test: the chart rendered no kgf-nexus/ images with default values")
    digests = {n: f"{base}{n}@{fake(i + 1)}" for i, n in enumerate(names)}
    print(f"self-test: {len(names)} chart images: {', '.join(names)}")
    problems = check(chart, digests, [], sets, [])
    if problems:
        fail("self-test: a correct digest map did not pass:\n  " + "\n  ".join(problems))
    # The check must also be able to FAIL: an extra image the chart does not
    # render, and a wrong digest, must each be reported.
    extra = dict(digests, **{"not-a-chart-image": f"{base}not-a-chart-image@{fake(999)}"})
    if not check(chart, extra, [], sets, [], quiet=True):
        fail("self-test: an image missing from the chart was not reported")
    # --set outranks every values file, so this renders one image with a digest
    # the map does not have: exactly what a template ignoring the map produces.
    victim = names[0]
    if not check(chart, digests, [], sets + [f"image.digests.{victim}={fake(998)}"], [], quiet=True):
        fail("self-test: an image rendered with the wrong digest was not reported")
    print("self-test: passed (correct map accepted; unrendered image and altered digest detected)")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    g = sub.add_parser("collect")
    g.add_argument("dir")
    g.add_argument("--expect", action="append", required=True,
                   help="image names, comma-separated or repeated")
    v = sub.add_parser("values")
    v.add_argument("digests")
    c = sub.add_parser("check")
    c.add_argument("digests")
    c.add_argument("--chart", required=True)
    c.add_argument("--values", action="append", default=[])
    c.add_argument("--set", action="append", default=[])
    c.add_argument("--not-in-chart", action="append", default=[])
    t = sub.add_parser("self-test")
    t.add_argument("--chart", required=True)
    t.add_argument("--set", action="append", default=[])
    a = ap.parse_args()

    if a.cmd == "collect":
        expect = [n.strip() for arg in a.expect for n in arg.split(",") if n.strip()]
        if not expect:
            fail("--expect named no images")
        json.dump(collect(a.dir, expect), sys.stdout, indent=2, sort_keys=True)
        print()
    elif a.cmd == "values":
        json.dump(values_for(load_digests(a.digests)), sys.stdout, indent=2, sort_keys=True)
        print()
    elif a.cmd == "check":
        problems = check(a.chart, load_digests(a.digests), a.values, a.set, a.not_in_chart)
        if problems:
            fail("rendered images do not match the release digests:\n  " + "\n  ".join(problems))
        print("helm-image-digests: every Nexus image resolves to its release digest")
    else:
        self_test(a.chart, a.set)


if __name__ == "__main__":
    main()
