#!/usr/bin/env python3
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Build the air-gapped installation bundle (ADR-0025 §3, docs/AIRGAP.md).

    deploy/airgap/build-bundle.py --version 1.4.0 [--platforms linux/amd64,linux/arm64]

Writes dist/nexus-airgap-<version>.tar.gz (+ .sha256) holding every image the
Helm chart and the compose stack reference, the packaged chart, the CLI and
agent binaries, the SDK wheel, schemas/, load.sh, manifest.json and SHA256SUMS
(signed with cosign when it is installed).

The image list is DERIVED, never hand-maintained:

  * the chart is rendered (`helm template`) with every optional component on,
    once with PITR off and once on (PITR swaps the Postgres image), and every
    `image:` line is collected;
  * the compose files are resolved (`docker compose config`) with a clean
    environment, and every service's image or build recipe collected;
  * each third-party reference is resolved to a registry DIGEST before anything
    is saved. A reference that cannot be resolved fails the build, as does a
    pinned digest the registry no longer serves;
  * the chart is then rendered again with every image value pointed at a
    sentinel registry, and the build fails if any image escapes it -- proof that
    the values file load.sh writes redirects every image, not most of them.

Images are saved as OCI image layouts (one tar per image): skopeo when it is on
PATH (exact upstream bytes, no daemon), `docker pull` + `docker save` otherwise.
Either way the archive is normalised to one top-level entry per image, named
`nexus-airgap.local/<repo>:<tag>` (a registry host that cannot exist, so a
`docker load` on the target never clobbers a real image) and carrying
`org.opencontainers.image.ref.name` so skopeo can address it. A single-platform
image is its upstream per-platform manifest, byte for byte; a multi-platform
image is a new index over the upstream per-platform manifests (the upstream
index lists platforms the bundle does not carry, so it cannot be reused).

Standard library only: the build host needs python3, docker (with buildx),
helm and go; skopeo, uv and cosign are used when present.
"""

import argparse
import datetime as dt
import gzip
import hashlib
import io
import json
import os
import platform as pyplatform
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
CHART = os.path.join(ROOT, "deploy", "helm", "kgf-nexus")
COMPOSE_FILES = ["deploy/docker-compose.yml", "deploy/docker-compose.pitr.yml", "deploy/docker-compose.tls.yml"]
# Repository prefix of every image built from this tree, in the chart and in
# compose. The chart's image.repositoryPrefix default.
FIRST_PARTY_PREFIX = "kgf-nexus/"
# The name every image is loaded under on the target host. `.local` is reserved
# (RFC 6762) and never a real registry, so nothing the operator has is replaced.
LOAD_HOST = "nexus-airgap.local"
SENTINEL = "airgap-sentinel.invalid/"
ZERO_DIGEST = "sha256:" + "0" * 64
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
TAG_RE = re.compile(r"^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$")
IMAGE_LINE_RE = re.compile(r"""^\s*(?:-\s+)?image:\s*["']?([^"'\s#]+)["']?\s*$""")

INDEX_TYPES = {"application/vnd.oci.image.index.v1+json",
               "application/vnd.docker.distribution.manifest.list.v2+json"}
MANIFEST_TYPES = {"application/vnd.oci.image.manifest.v1+json",
                  "application/vnd.docker.distribution.manifest.v2+json"}


# ── small helpers ──────────────────────────────────────────────────────────

def log(msg):
    print(f"airgap-bundle: {msg}", file=sys.stderr, flush=True)


def die(msg):
    print(f"airgap-bundle: ERROR: {msg}", file=sys.stderr, flush=True)
    sys.exit(1)


def run(cmd, *, capture=True, env=None, cwd=ROOT, check=True, binary=False, quiet=False):
    if not quiet:
        log("$ " + " ".join(shlex.quote(c) for c in cmd))
    res = subprocess.run(cmd, cwd=cwd, env=env, capture_output=capture, text=not binary)
    if check and res.returncode != 0:
        err = res.stderr if capture else ""
        if binary and isinstance(err, bytes):
            err = err.decode(errors="replace")
        die(f"command failed ({res.returncode}): {' '.join(cmd)}\n{(err or '').strip()}")
    return res


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def have(tool):
    return shutil.which(tool) is not None


def slug(text):
    return re.sub(r"[^A-Za-z0-9_.-]+", "_", text).strip("_")


# ── image references ───────────────────────────────────────────────────────

class Ref:
    """A parsed image reference, normalised the way Docker does."""

    def __init__(self, text):
        self.text = text
        rest, _, digest = text.partition("@")
        self.digest = digest or None
        if self.digest and not DIGEST_RE.match(self.digest):
            die(f"{text}: malformed digest")
        first, _, remainder = rest.partition("/")
        if remainder and ("." in first or ":" in first or first == "localhost"):
            self.registry, path = first, remainder
        else:
            self.registry, path = "docker.io", rest
        last = path.rsplit("/", 1)[-1]
        if ":" in last:
            path, _, self.tag = path.rpartition(":")
        else:
            self.tag = None
        if self.registry == "docker.io" and "/" not in path:
            path = "library/" + path
        self.path = path

    @property
    def canonical(self):
        return f"{self.registry}/{self.path}"

    def with_digest(self, digest):
        tag = f":{self.tag}" if self.tag else ""
        return f"{self.canonical}{tag}@{digest}"

    def pull_ref(self, digest):
        return f"{self.canonical}@{digest}"


def platform_str(p):
    s = f"{p.get('os')}/{p.get('architecture')}"
    if p.get("variant"):
        s += f"/{p['variant']}"
    return s


def platform_matches(want, have_p):
    """want 'linux/arm64' matches linux/arm64/v8; 'linux/arm64/v8' needs v8."""
    w = want.split("/")
    h = platform_str(have_p).split("/")
    if w[:2] != h[:2]:
        return False
    if len(w) == 3:
        return len(h) == 3 and w[2] == h[2]
    return True


# ── chart discovery ────────────────────────────────────────────────────────

def probe_values(version, pitr):
    """Values that switch every optional image-bearing component on."""
    v = {
        "existingSecret": "airgap-probe",
        "image": {"tag": version, "registry": "", "repositoryPrefix": FIRST_PARTY_PREFIX},
        "nats": {"enabled": True, "auth": {"mode": "none"}, "configReloader": {"enabled": True}},
        "postgres": {"enabled": True},
        "minio": {"enabled": True},
        "mailpit": {"enabled": True},
        "clusterController": {"enabled": True, "clusterName": "airgap-probe"},
        "artifactGC": {"enabled": True},
    }
    if pitr:
        v["postgres"]["pitr"] = {
            "enabled": True, "existingSecret": "airgap-probe",
            "repo": {"s3": {"bucket": "airgap-probe", "endpoint": "https://airgap-probe.invalid"}},
        }
    return v


def helm_images(values_files):
    cmd = ["helm", "template", "airgap-probe", CHART]
    for f in values_files:
        cmd += ["--values", f]
    out = run(cmd, quiet=True).stdout
    return [m.group(1) for line in out.splitlines() if (m := IMAGE_LINE_RE.match(line))]


def values_image_keys(values_yaml):
    """Dotted paths of every string `*image`/`*Image` leaf in values.yaml.

    A deliberately small indentation walker (no YAML dependency): the chart's
    values.yaml is block-style. Anything it misses is caught by the sentinel
    render in verify_redirect(), which fails the build.
    """
    keys = {}
    stack = []  # (indent, key)
    for raw in open(values_yaml, encoding="utf-8"):
        line = raw.rstrip("\n")
        stripped = line.lstrip()
        if not stripped or stripped.startswith("#") or stripped.startswith("-"):
            continue
        m = re.match(r"^(\s*)([A-Za-z0-9_]+):\s*(.*)$", line)
        if not m:
            continue
        indent, key, value = len(m.group(1)), m.group(2), m.group(3)
        value = re.sub(r"\s+#.*$", "", value).strip()
        while stack and stack[-1][0] >= indent:
            stack.pop()
        if value == "":
            stack.append((indent, key))
            continue
        if re.search(r"(^|[a-z])(image|Image)$", key) and not value.startswith(("{", "[")):
            path = ".".join([k for _, k in stack] + [key])
            keys[path] = value.strip("'\"")
    return keys


def set_path(d, dotted, value):
    parts = dotted.split(".")
    for p in parts[:-1]:
        d = d.setdefault(p, {})
    d[parts[-1]] = value


def merge(a, b):
    out = json.loads(json.dumps(a))
    for k, v in b.items():
        if isinstance(v, dict) and isinstance(out.get(k), dict):
            out[k] = merge(out[k], v)
        else:
            out[k] = v
    return out


def discover_chart(version, work):
    probes = []
    for pitr in (False, True):
        path = os.path.join(work, f"probe-{'pitr' if pitr else 'base'}.json")
        with open(path, "w") as fh:
            json.dump(probe_values(version, pitr), fh, indent=2, sort_keys=True)
        probes.append(path)
    rendered = []
    for p in probes:
        rendered += helm_images([p])
    rendered = sorted(set(rendered))
    keys = values_image_keys(os.path.join(CHART, "values.yaml"))
    by_value = {}
    for k, v in keys.items():
        by_value.setdefault(v, []).append(k)

    components, others, problems = {}, {}, []
    for ref in rendered:
        if ref.startswith(FIRST_PARTY_PREFIX) and ref.endswith(":" + version) and "@" not in ref:
            name = ref[len(FIRST_PARTY_PREFIX):-len(":" + version)]
            components[name] = ref
        elif ref in by_value:
            others[ref] = sorted(by_value[ref])
        else:
            problems.append(f"{ref}: rendered by the chart but not the default of any *image value "
                            "(the air-gap values file could not redirect it)")
    if problems:
        die("chart image discovery:\n  " + "\n  ".join(problems))
    log(f"chart: {len(components)} Nexus components, {len(others)} other images "
        f"({', '.join(sorted(others))})")
    return probes, components, others, keys


def verify_redirect(probes, components, others, work):
    """Render with every image value at a sentinel registry: nothing may escape."""
    v = {"image": {"registry": SENTINEL, "repositoryPrefix": FIRST_PARTY_PREFIX,
                   "digests": {c: ZERO_DIGEST for c in components}}}
    for ref, keys in others.items():
        for k in keys:
            set_path(v, k, f"{SENTINEL}{slug(k)}@{ZERO_DIGEST}")
    path = os.path.join(work, "sentinel.json")
    with open(path, "w") as fh:
        json.dump(v, fh)
    escaped = []
    for p in probes:
        for ref in helm_images([p, path]):
            if not ref.startswith(SENTINEL) or not ref.endswith("@" + ZERO_DIGEST):
                escaped.append(ref)
    if escaped:
        die("these images are not redirected by the generated values (chart change?):\n  "
            + "\n  ".join(sorted(set(escaped))))
    log("chart: every rendered image is redirectable to a private registry by digest")


# ── the values template load.sh renders ────────────────────────────────────

REG_PH = "@@REGISTRY@@"


def digest_ph(name):
    return f"@@DIGEST:{name}@@"


def yaml_emit(d, indent=0):
    """Nested dict of strings -> block YAML (every scalar double-quoted)."""
    out = []
    for k in sorted(d):
        v = d[k]
        key = k if re.match(r"^[A-Za-z0-9_]+$", k) else json.dumps(k)
        if isinstance(v, dict):
            out.append(" " * indent + f"{key}:")
            out.extend(yaml_emit(v, indent + 2))
        else:
            out.append(" " * indent + f"{key}: {json.dumps(v)}")
    return out


def values_template(images, version):
    """The Helm values that point every chart image at the private registry by
    digest, with placeholders load.sh fills from what the registry reports."""
    v = {"image": {"registry": f"{REG_PH}/", "repositoryPrefix": FIRST_PARTY_PREFIX,
                   "tag": version, "digests": {}}}
    for im in images:
        for use in im["values"]:
            if use["form"] == "digest":
                set_path(v, use["key"], digest_ph(im["name"]))
            else:
                set_path(v, use["key"],
                         f"{REG_PH}/{im['target_repository']}:{im['target_tag']}@{digest_ph(im['name'])}")
    head = [
        "# Generated by deploy/airgap/build-bundle.py; rendered by load.sh.",
        "# Every image the chart renders, at the private registry, BY DIGEST (the",
        "# digest the registry reported after the push). Pass it with -f, AFTER your",
        "# own values, so nothing overrides an image back to a public registry.",
    ]
    return "\n".join(head + yaml_emit(v)) + "\n"


def verify_template(probes, template, work):
    """Render the chart with the template itself (sentinel registry, zero
    digests): every image must come out at the registry, pinned by digest."""
    text = template.replace(REG_PH, SENTINEL.rstrip("/"))
    text = re.sub(r"@@DIGEST:[^@]+@@", ZERO_DIGEST, text)
    if "@@" in text:
        die("values template has an unfilled placeholder")
    path = os.path.join(work, "template-check.yaml")
    with open(path, "w") as fh:
        fh.write(text)
    escaped = []
    for p in probes:
        for ref in helm_images([p, path]):
            if not ref.startswith(SENTINEL) or not ref.endswith("@" + ZERO_DIGEST):
                escaped.append(ref)
    if escaped:
        die("the bundle's values template leaves images outside the private registry:\n  "
            + "\n  ".join(sorted(set(escaped))))
    log("values template: every chart image is redirected to the private registry by digest")


# ── compose discovery ──────────────────────────────────────────────────────

def discover_compose(work):
    files = os.environ.get("AIRGAP_COMPOSE_FILES")
    files = files.split(",") if files else COMPOSE_FILES
    # A clean environment: the bundle must reflect the committed defaults, not
    # whatever NEXUS_*_IMAGE the builder's shell happens to export.
    env = {k: v for k, v in os.environ.items() if k in ("PATH", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "TMPDIR")}
    required = set()
    for f in files:
        required |= set(re.findall(r"\$\{([A-Z0-9_]+):\?", open(os.path.join(ROOT, f), encoding="utf-8").read()))
    for var in required:
        env[var] = "airgap-probe"
    empty_env = os.path.join(work, "empty.env")
    open(empty_env, "w").close()
    cmd = ["docker", "compose", "--env-file", empty_env, "--profile", "*"]
    for f in files:
        cmd += ["-f", os.path.join(ROOT, f)]
    cmd += ["config", "--format", "json"]
    cfg = json.loads(run(cmd, env=env, quiet=True).stdout)
    recipes, third, where = {}, {}, {}
    for svc, spec in sorted((cfg.get("services") or {}).items()):
        image, build = spec.get("image"), spec.get("build")
        if build:
            args = {k: ("" if v is None else str(v)) for k, v in (build.get("args") or {}).items()}
            if args.get("SERVICE"):
                name = args["SERVICE"]
            elif image and image.startswith(FIRST_PARTY_PREFIX):
                name = Ref(image).path.split("/", 1)[1]
            else:
                name = svc
            recipe = {"context": build.get("context", ROOT), "dockerfile": build.get("dockerfile", "Dockerfile"),
                      "args": args, "tag": (Ref(image).tag if image else None)}
            if name in recipes and {k: recipes[name][k] for k in ("dockerfile", "args")} != {k: recipe[k] for k in ("dockerfile", "args")}:
                die(f"compose builds image {name!r} two different ways ({where[name]} vs {svc})")
            recipes.setdefault(name, recipe)
            where.setdefault(name, svc)
        elif image:
            third.setdefault(image, []).append(svc)
    log(f"compose: {len(recipes)} first-party build recipes, {len(third)} third-party images")
    return recipes, third


def go_main_package(component):
    base = os.path.join(ROOT, "services", component)
    if not os.path.isdir(base):
        return None
    out = run(["go", "list", "-f", "{{if eq .Name \"main\"}}{{.Dir}}{{end}}", f"./services/{component}/..."], quiet=True).stdout
    mains = [os.path.relpath(p, ROOT) for p in out.split() if p]
    return mains[0] if len(mains) == 1 else None


def build_recipes(components, others, compose_recipes):
    """One recipe per first-party image: compose's when it builds it, else the
    shared Go service Dockerfile with the component's main package."""
    recipes = {}
    service_df = [r for r in compose_recipes.values() if r["dockerfile"].endswith("Dockerfile.service")]
    wanted = dict(components)
    for ref in others:
        if ref.startswith(FIRST_PARTY_PREFIX):
            wanted[Ref(ref).path.split("/", 1)[1]] = ref
    for name in compose_recipes:
        wanted.setdefault(name, None)
    for name in sorted(wanted):
        if name in compose_recipes:
            recipes[name] = dict(compose_recipes[name])
            continue
        main = go_main_package(name)
        if main and service_df:
            r = service_df[0]
            recipes[name] = {"context": r["context"], "dockerfile": r["dockerfile"],
                             "args": {"SERVICE": name, "SERVICE_PATH": main}, "tag": None}
            continue
        die(f"no build recipe for first-party image {name!r}: not built by compose and "
            f"services/{name} has no unique main package")
    return recipes


def dockerfile_bases(recipe):
    path = os.path.join(recipe["context"], recipe["dockerfile"])
    args, bases, stages = {}, [], set()
    for line in open(path, encoding="utf-8"):
        s = line.strip()
        m = re.match(r"^ARG\s+([A-Za-z0-9_]+)(?:=(.*))?$", s)
        if m and not bases:
            args[m.group(1)] = recipe["args"].get(m.group(1), (m.group(2) or "").strip('"'))
        m = re.match(r"^FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?", s, re.I)
        if m:
            ref = re.sub(r"\$\{?([A-Za-z0-9_]+)\}?", lambda x: args.get(x.group(1), ""), m.group(1))
            if ref not in stages and ref != "scratch" and ref not in bases:
                bases.append(ref)
            if m.group(2):
                stages.add(m.group(2))
    return bases


# ── registry resolution ────────────────────────────────────────────────────

class Resolver:
    def __init__(self, tool):
        self.tool = tool

    def raw(self, ref):
        if self.tool == "skopeo":
            out = run(["skopeo", "inspect", "--raw", f"docker://{ref}"], binary=True, quiet=True).stdout
        else:
            out = run(["docker", "buildx", "imagetools", "inspect", "--raw", ref], binary=True, quiet=True).stdout
        return out

    def resolve(self, ref_text, platforms):
        """-> (digest, media_type, {platform: child descriptor})"""
        ref = Ref(ref_text)
        lookup = ref.pull_ref(ref.digest) if ref.digest else (f"{ref.canonical}:{ref.tag}" if ref.tag else ref.canonical)
        raw = self.raw(lookup)
        digest = "sha256:" + hashlib.sha256(raw).hexdigest()
        if ref.digest and digest != ref.digest:
            die(f"{ref_text}: registry returned {digest}, not the pinned digest")
        doc = json.loads(raw)
        mt = doc.get("mediaType", "")
        children = {}
        if mt in INDEX_TYPES or "manifests" in doc:
            for want in platforms:
                match = [m for m in doc["manifests"] if m.get("platform") and platform_matches(want, m["platform"])]
                if not match:
                    avail = sorted({platform_str(m["platform"]) for m in doc["manifests"] if m.get("platform")})
                    die(f"{ref_text}: no {want} image (upstream has {', '.join(avail)})")
                children[want] = match[0]
        else:
            # A single-platform manifest: its platform is checked against the
            # image config when the saved layout is normalised.
            if len(platforms) != 1:
                die(f"{ref_text}: a single-platform image cannot provide {', '.join(platforms)}")
            children[platforms[0]] = {"mediaType": mt, "digest": digest, "size": len(raw),
                                      "platform": None}
        return digest, mt, children


# ── OCI layout normalisation ───────────────────────────────────────────────

def read_blob(layout, digest):
    algo, hexd = digest.split(":", 1)
    with open(os.path.join(layout, "blobs", algo, hexd), "rb") as fh:
        return fh.read()


def write_blob(layout, data):
    hexd = hashlib.sha256(data).hexdigest()
    d = os.path.join(layout, "blobs", "sha256")
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, hexd), "wb") as fh:
        fh.write(data)
    return "sha256:" + hexd


def reachable(layout, desc, seen):
    seen.add(desc["digest"])
    mt = desc.get("mediaType", "")
    if mt in INDEX_TYPES or mt in MANIFEST_TYPES:
        doc = json.loads(read_blob(layout, desc["digest"]))
        if "manifests" in doc:
            for m in doc["manifests"]:
                reachable(layout, m, seen)
        else:
            seen.add(doc["config"]["digest"])
            for layer in doc.get("layers", []):
                seen.add(layer["digest"])


def collect_image_manifests(layout):
    """Every image manifest (not attestation) in the layout, with its platform."""
    index = json.load(open(os.path.join(layout, "index.json")))
    found = []

    def walk(desc, platform):
        mt = desc.get("mediaType", "")
        if mt in INDEX_TYPES:
            doc = json.loads(read_blob(layout, desc["digest"]))
            for m in doc.get("manifests", []):
                walk(m, m.get("platform"))
        elif mt in MANIFEST_TYPES:
            ann = desc.get("annotations") or {}
            if "io.containerd.manifest.subject" in ann or ann.get("vnd.docker.reference.type") == "attestation-manifest":
                return
            if platform and platform.get("os") == "unknown":
                return
            if not platform:
                doc = json.loads(read_blob(layout, desc["digest"]))
                cfg = json.loads(read_blob(layout, doc["config"]["digest"]))
                platform = {"os": cfg.get("os"), "architecture": cfg.get("architecture")}
                if cfg.get("variant"):
                    platform["variant"] = cfg["variant"]
            found.append((dict(desc), platform))

    for m in index.get("manifests", []):
        walk(m, m.get("platform"))
    return found


def normalise(layout, platforms, load_name, ref_name, expected=None):
    """Rewrite `layout` so it holds exactly one image (one per-platform
    manifest, or an index over several) named load_name. expected maps
    platform -> upstream child digest and is enforced when given.
    Returns (top digest, media type, {platform: digest})."""
    manifests = collect_image_manifests(layout)
    chosen = {}
    for want in platforms:
        cands = [(d, p) for d, p in manifests if p and platform_matches(want, p)]
        if expected:
            cands = [(d, p) for d, p in cands if d["digest"] == expected[want]]
        uniq = {d["digest"]: (d, p) for d, p in cands}
        if len(uniq) != 1:
            die(f"{load_name}: expected exactly one {want} manifest in the saved image, found {len(uniq)}"
                + (f" matching {expected[want]}" if expected else ""))
        d, p = next(iter(uniq.values()))
        d = {"mediaType": d["mediaType"], "digest": d["digest"], "size": d["size"], "platform": p}
        chosen[want] = d
    ann = {"io.containerd.image.name": load_name, "org.opencontainers.image.ref.name": ref_name}
    if len(chosen) == 1:
        top = dict(next(iter(chosen.values())))
        top.pop("platform", None)
    else:
        idx = {"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
               "manifests": [chosen[p] for p in platforms]}
        data = json.dumps(idx, separators=(",", ":")).encode()
        top = {"mediaType": idx["mediaType"], "digest": write_blob(layout, data), "size": len(data)}
    top["annotations"] = ann
    # Drop everything the chosen image does not reference (other platforms'
    # attestations, stale entries).
    keep = set()
    reachable(layout, top, keep)
    blobdir = os.path.join(layout, "blobs", "sha256")
    for f in os.listdir(blobdir):
        if "sha256:" + f not in keep:
            os.remove(os.path.join(blobdir, f))
    with open(os.path.join(layout, "index.json"), "w") as fh:
        json.dump({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
                   "manifests": [top]}, fh, separators=(",", ":"))
    with open(os.path.join(layout, "oci-layout"), "w") as fh:
        json.dump({"imageLayoutVersion": "1.0.0"}, fh)
    # Legacy manifest.json, for `docker load` on a daemon without the
    # containerd image store. It can describe one platform only.
    legacy = []
    for want in platforms[:1]:
        doc = json.loads(read_blob(layout, chosen[want]["digest"]))
        legacy.append({"Config": "blobs/" + doc["config"]["digest"].replace(":", "/"),
                       "RepoTags": [load_name],
                       "Layers": ["blobs/" + l["digest"].replace(":", "/") for l in doc["layers"]]})
    with open(os.path.join(layout, "manifest.json"), "w") as fh:
        json.dump(legacy, fh, separators=(",", ":"))
    return top["digest"], top["mediaType"], {p: d["digest"] for p, d in chosen.items()}


def pack_layout(layout, dest, mtime):
    """Deterministic, uncompressed tar of an OCI layout (layers are already
    compressed; the bundle tarball is gzip over everything)."""
    with tarfile.open(dest, "w", format=tarfile.PAX_FORMAT) as tar:
        for dirpath, dirnames, filenames in os.walk(layout):
            dirnames.sort()
            for name in sorted(filenames):
                full = os.path.join(dirpath, name)
                info = tar.gettarinfo(full, arcname=os.path.relpath(full, layout))
                info.uid = info.gid = 0
                info.uname = info.gname = ""
                info.mtime = mtime
                info.mode = 0o644
                with open(full, "rb") as fh:
                    tar.addfile(info, fh)


def extract(tar_path, dest):
    with tarfile.open(tar_path) as tar:
        tar.extractall(dest, filter="data")


# ── image acquisition ──────────────────────────────────────────────────────

def docker_has(ref):
    return run(["docker", "image", "inspect", "--format", "{{.Id}}", ref],
               check=False, quiet=True).returncode == 0


def save_third_party(tool, ref_text, digest, platforms, layout):
    ref = Ref(ref_text)
    if tool == "skopeo":
        for i, want in enumerate(platforms):
            os_, arch, *variant = want.split("/")
            cmd = ["skopeo", "copy", "--preserve-digests", "--override-os", os_, "--override-arch", arch]
            if variant:
                cmd += ["--override-variant", variant[0]]
            cmd += [f"docker://{ref.pull_ref(digest)}", f"oci:{layout}:p{i}"]
            run(cmd)
        return
    pull = ref.pull_ref(digest)
    # Only an image this build pulled is removed afterwards: the builder's own
    # images (even the same digest under another name) are never touched.
    pulled = not docker_has(pull)
    raw = layout + ".docker-save.tar"
    try:
        for want in platforms:
            run(["docker", "pull", "--quiet", "--platform", want, pull])
        run(["docker", "save", "--platform", ",".join(platforms), "-o", raw, pull])
    finally:
        if pulled:
            run(["docker", "image", "rm", pull], check=False, quiet=True)
    extract(raw, layout)
    os.remove(raw)


def build_first_party(name, recipe, version, platforms, commit, load_name, layout, bases_pull):
    args = dict(recipe["args"])
    df_path = os.path.join(recipe["context"], recipe["dockerfile"])
    if re.search(r"^ARG\s+VERSION\b", open(df_path, encoding="utf-8").read(), re.M):
        args["VERSION"] = version
    cmd = ["docker", "buildx", "build", "--platform", ",".join(platforms),
           "--provenance=false", "--sbom=false", "-f", df_path, "-t", load_name, "--load",
           "--label", f"org.opencontainers.image.version={version}",
           "--label", f"org.opencontainers.image.revision={commit}",
           "--label", "org.opencontainers.image.title=" + name]
    if bases_pull:
        cmd.append("--pull")
    builder = os.environ.get("AIRGAP_BUILDER")
    if builder:
        cmd += ["--builder", builder]
    for k in sorted(args):
        cmd += ["--build-arg", f"{k}={args[k]}"]
    cmd.append(recipe["context"])
    run(cmd, capture=False)
    raw = layout + ".docker-save.tar"
    try:
        run(["docker", "save", "--platform", ",".join(platforms), "-o", raw, load_name])
    finally:
        # The tag is ours (nexus-airgap.local/...); the layers stay in the
        # build cache, so a rebuild is cheap.
        run(["docker", "image", "rm", load_name], check=False, quiet=True)
    extract(raw, layout)
    os.remove(raw)
    return args


# ── binaries, SDK, chart ───────────────────────────────────────────────────

# (name, main package, targets, -X symbol that carries the version)
BINARIES = [
    ("nexusctl", "./cli/cmd/nexusctl", ["linux/amd64", "linux/arm64", "darwin/arm64"],
     "github.com/khuriwalgroup/kgf-nexus/cli/internal/cmd.Version"),
    ("nexus-agent", "./agent/cmd/nexus-agent", ["linux/amd64", "linux/arm64"], "main.version"),
]


def build_binaries(stage, version):
    out = []
    for name, pkg, targets, symbol in BINARIES:
        for t in targets:
            goos, goarch = t.split("/")
            dest = os.path.join(stage, "bin", f"{goos}-{goarch}", name)
            env = dict(os.environ, CGO_ENABLED="0", GOOS=goos, GOARCH=goarch)
            run(["go", "build", "-trimpath", "-buildvcs=false",
                 "-ldflags", f"-s -w -X {symbol}={version}", "-o", dest, pkg], env=env)
            out.append({"name": name, "os": goos, "arch": goarch,
                        "file": os.path.relpath(dest, stage)})
    return out


def build_sdk(stage, work):
    """Build the wheel from a COPY of sdk/python in an isolated environment, so
    the source tree gets no build/ or *.egg-info and the builder's site-packages
    cannot leak into the wheel."""
    dest = os.path.join(stage, "sdk")
    os.makedirs(dest, exist_ok=True)
    src = os.path.join(work, "sdk-src")
    shutil.copytree(os.path.join(ROOT, "sdk", "python"), src,
                    ignore=shutil.ignore_patterns("build", "dist", "*.egg-info", "__pycache__",
                                                  ".venv", ".mypy_cache", ".pytest_cache"))
    if have("uv"):
        # uv build creates a fresh, isolated build environment per build.
        run(["uv", "build", "--wheel", "--out-dir", dest, src])
    else:
        venv = os.path.join(work, "sdk-build-venv")
        py = os.environ.get("SDK_PYTHON", "python3")
        run([py, "-m", "venv", venv])
        run([os.path.join(venv, "bin", "python"), "-m", "pip", "install", "--quiet", "build"])
        run([os.path.join(venv, "bin", "python"), "-m", "build", "--wheel", "--outdir", dest, src])
    wheels = [f for f in os.listdir(dest) if f.endswith(".whl")]
    if len(wheels) != 1:
        die(f"expected one SDK wheel, found {wheels}")
    for f in os.listdir(dest):
        if not f.endswith(".whl"):
            os.remove(os.path.join(dest, f))
    return {"file": f"sdk/{wheels[0]}"}


def package_chart(stage, version):
    dest = os.path.join(stage, "chart")
    os.makedirs(dest, exist_ok=True)
    run(["helm", "lint", CHART, "--values", os.path.join(CHART, "ci", "minimal-values.yaml")], quiet=True)
    run(["helm", "package", CHART, "--app-version", version, "--destination", dest])
    tgz = [f for f in os.listdir(dest) if f.endswith(".tgz")]
    meta = {}
    for line in open(os.path.join(CHART, "Chart.yaml"), encoding="utf-8"):
        m = re.match(r"^(name|version):\s*(\S+)", line)
        if m:
            meta[m.group(1)] = m.group(2).strip("'\"")
    return {"name": meta.get("name"), "version": meta.get("version"), "app_version": version,
            "file": f"chart/{tgz[0]}"}


# ── signing ────────────────────────────────────────────────────────────────

def signing_plan():
    if os.environ.get("AIRGAP_SIGN", "auto") == "0":
        return {"signed": False, "reason": "signing disabled (AIRGAP_SIGN=0)"}
    if not have("cosign"):
        return {"signed": False, "reason": "cosign was not installed on the build host; "
                "integrity rests on SHA256SUMS and the out-of-band .sha256 of the tarball"}
    key = os.environ.get("COSIGN_KEY")
    return {"signed": True, "method": "cosign sign-blob", "mode": "key" if key else "keyless",
            "payload": "SHA256SUMS", "bundle": "SHA256SUMS.cosign.bundle", "key": key}


def sign(stage, plan):
    cmd = ["cosign", "sign-blob", "--yes", "--bundle", os.path.join(stage, plan["bundle"])]
    if plan["mode"] == "key":
        cmd += ["--key", plan["key"]]
    # e.g. COSIGN_SIGN_ARGS="--tlog-upload=false" for a key that must not be
    # recorded in the public transparency log.
    cmd += shlex.split(os.environ.get("COSIGN_SIGN_ARGS", ""))
    cmd.append(os.path.join(stage, "SHA256SUMS"))
    run(cmd, capture=False)


# ── main ───────────────────────────────────────────────────────────────────

def git(*args):
    return run(["git", *args], quiet=True).stdout.strip()


def default_platform():
    arch = run(["docker", "version", "--format", "{{.Server.Arch}}"], quiet=True).stdout.strip()
    return f"linux/{arch or ('arm64' if pyplatform.machine() in ('arm64', 'aarch64') else 'amd64')}"


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--version", default=os.environ.get("VERSION"))
    ap.add_argument("--platforms", default=os.environ.get("PLATFORMS"),
                    help="comma-separated image platforms (default: the Docker host's, e.g. linux/arm64)")
    ap.add_argument("--out-dir", default=os.path.join(ROOT, "dist"))
    ap.add_argument("--image-tool", choices=["auto", "skopeo", "docker"], default=os.environ.get("AIRGAP_IMAGE_TOOL", "auto"))
    ap.add_argument("--plan", action="store_true",
                    help="discover and resolve every image, print the plan as JSON, build nothing")
    ap.add_argument("--keep-work", action="store_true", help="keep the staging directory")
    ap.add_argument("--no-pull", action="store_true", help="build first-party images without --pull (base images from the local cache)")
    ap.add_argument("--only", default=os.environ.get("AIRGAP_ONLY", ""),
                    help="DEBUG: comma-separated image names to include; the bundle is marked partial")
    a = ap.parse_args()

    for tool in ("docker", "helm", "go", "git"):
        if not have(tool):
            die(f"{tool} is required on the build host")
    commit = git("rev-parse", "HEAD")
    dirty = bool(git("status", "--porcelain", "--untracked-files=no"))
    version = a.version or git("rev-parse", "--short=12", "HEAD") + (
        "-dirty-" + dt.datetime.now(dt.timezone.utc).strftime("%Y%m%d%H%M%S") if dirty else "")
    if not TAG_RE.match(version) or version == "latest":
        die(f"version {version!r} is not a valid, immutable image tag")
    platforms = [p.strip() for p in (a.platforms or default_platform()).split(",") if p.strip()]
    for p in platforms:
        if not re.match(r"^linux/[a-z0-9]+(/v[0-9]+)?$", p):
            die(f"platform {p!r}: expected linux/<arch>[/<variant>]")
    tool = a.image_tool
    if tool == "auto":
        tool = "skopeo" if have("skopeo") else "docker"
    if tool == "skopeo" and not have("skopeo"):
        die("--image-tool skopeo but skopeo is not on PATH")
    if tool == "docker" and run(["docker", "buildx", "version"], check=False, quiet=True).returncode != 0:
        die("docker buildx is required to resolve digests without skopeo")
    if len(platforms) > 1 and tool == "docker":
        drv = run(["docker", "info", "--format", "{{json .DriverStatus}}"], quiet=True).stdout
        if "io.containerd.snapshotter" not in drv:
            die("several PLATFORMS need Docker's containerd image store (or skopeo) on the build host")
    source_epoch = int(git("log", "-1", "--format=%ct"))
    build_time = dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")
    log(f"version {version}, commit {commit[:12]}{' (DIRTY tree)' if dirty else ''}, "
        f"platforms {','.join(platforms)}, image tool {tool}")

    os.makedirs(a.out_dir, exist_ok=True)
    work = tempfile.mkdtemp(prefix=f".airgap-work-{version}-", dir=a.out_dir)
    bundle_dir = f"nexus-airgap-{version}" + ("-partial" if a.only else "")
    stage = os.path.join(work, bundle_dir)
    os.makedirs(os.path.join(stage, "images"))
    try:
        # 1. what the chart and compose reference
        probes, components, chart_others, value_keys = discover_chart(version, work)
        verify_redirect(probes, components, chart_others, work)
        compose_recipes, compose_third = discover_compose(work)
        recipes = build_recipes(components, chart_others, compose_recipes)

        if a.plan:
            plan_out = {"platforms": platforms, "chart_components": components,
                        "chart_other_images": chart_others, "compose_third_party": compose_third,
                        "first_party_recipes": recipes, "resolved": {}}
            res = Resolver(tool)
            for ref in sorted(set(chart_others) | set(compose_third)):
                if not ref.startswith(FIRST_PARTY_PREFIX):
                    plan_out["resolved"][ref] = res.resolve(ref, platforms)[0]
            for r in recipes.values():
                for b in dockerfile_bases(r):
                    plan_out["resolved"].setdefault(b, res.resolve(b, platforms)[0])
            print(json.dumps(plan_out, indent=2, sort_keys=True))
            return

        only = {x.strip() for x in a.only.split(",") if x.strip()}
        images = []
        resolver = Resolver(tool)
        base_digests = {}

        # 2. third-party: resolve every reference to a digest, then save
        third = {}
        for ref, keys in chart_others.items():
            if not ref.startswith(FIRST_PARTY_PREFIX):
                third.setdefault(ref, {"values": [], "compose": []})["values"] += keys
        for ref, svcs in compose_third.items():
            third.setdefault(ref, {"values": [], "compose": []})["compose"] += svcs
        # The same image under two spellings (postgres:16-alpine vs
        # docker.io/library/postgres:16-alpine) is one archive.
        merged = {}
        for ref, use in sorted(third.items()):
            r = Ref(ref)
            key = (r.canonical, r.tag, r.digest)
            m = merged.setdefault(key, {"refs": [], "values": [], "compose": []})
            m["refs"].append(ref)
            m["values"] += use["values"]
            m["compose"] += use["compose"]
        for (canonical, tag, pinned), use in sorted(merged.items()):
            ref_text = use["refs"][0]
            r = Ref(ref_text)
            digest, mt, children = resolver.resolve(ref_text, platforms)
            name = slug(r.path.replace("/", "-") + ("-" + r.tag if r.tag else ""))
            if only and name not in only:
                continue
            target_tag = r.tag or digest.split(":")[1][:12]
            load_name = f"{LOAD_HOST}/{r.path}:{target_tag}"
            layout = os.path.join(work, "layouts", name)
            os.makedirs(layout)
            log(f"image {r.with_digest(digest)}")
            save_third_party(tool, ref_text, digest, platforms, layout)
            expected = {p: c["digest"] for p, c in children.items()}
            top, top_mt, per_platform = normalise(layout, platforms, load_name, target_tag, expected)
            archive = f"images/{name}.tar"
            pack_layout(layout, os.path.join(stage, archive), source_epoch)
            shutil.rmtree(layout)
            images.append({
                "name": name, "kind": "third-party", "source_refs": sorted(use["refs"]),
                "source_digest": digest, "source_media_type": mt, "pinned_upstream": bool(pinned),
                "bundle_digest": top, "bundle_media_type": top_mt, "platforms": per_platform,
                "archive": archive, "load_name": load_name,
                "target_repository": r.path, "target_tag": target_tag,
                "values": [{"key": k, "form": "ref"} for k in sorted(set(use["values"]))],
                "compose_services": sorted(set(use["compose"])),
            })

        # 3. first-party: build from this tree for PLATFORMS, then save
        for name, recipe in sorted(recipes.items()):
            if only and name not in only:
                continue
            chart_ref = components.get(name)
            other_ref = next((r for r in chart_others if r.startswith(FIRST_PARTY_PREFIX + name + ":")), None)
            if chart_ref:
                target_tag, values = version, [{"key": f"image.digests.{name}", "form": "digest"}]
            elif other_ref:
                target_tag = Ref(other_ref).tag
                values = [{"key": k, "form": "ref"} for k in chart_others[other_ref]]
            else:
                target_tag, values = recipe.get("tag") or version, []
            repo = FIRST_PARTY_PREFIX + name
            load_name = f"{LOAD_HOST}/{repo}:{target_tag}"
            layout = os.path.join(work, "layouts", name)
            os.makedirs(layout)
            bases = {}
            for b in dockerfile_bases(recipe):
                if b not in base_digests:
                    base_digests[b] = resolver.resolve(b, platforms)[0]
                bases[b] = base_digests[b]
            log(f"build {repo}:{target_tag} ({recipe['dockerfile']})")
            args = build_first_party(name, recipe, version, platforms, commit, load_name, layout, not a.no_pull)
            top, top_mt, per_platform = normalise(layout, platforms, load_name, target_tag)
            archive = f"images/{slug('kgf-nexus-' + name)}.tar"
            pack_layout(layout, os.path.join(stage, archive), source_epoch)
            shutil.rmtree(layout)
            images.append({
                "name": name, "kind": "first-party",
                "source_refs": sorted({x for x in (chart_ref, other_ref) if x}),
                "source_digest": None, "bundle_digest": top, "bundle_media_type": top_mt,
                "platforms": per_platform, "archive": archive, "load_name": load_name,
                "target_repository": repo, "target_tag": target_tag, "values": values,
                "compose_services": [],
                "build": {"dockerfile": os.path.relpath(os.path.join(recipe["context"], recipe["dockerfile"]), ROOT),
                          "args": args, "base_images": bases, "pulled_bases": not a.no_pull},
            })

        # 4. everything else
        binaries = build_binaries(stage, version)
        sdk = build_sdk(stage, work)
        chart = package_chart(stage, version)
        shutil.copytree(os.path.join(ROOT, "schemas"), os.path.join(stage, "schemas"))
        os.makedirs(os.path.join(stage, "airgap"))
        for p in probes:
            shutil.copy(p, os.path.join(stage, "airgap", os.path.basename(p)))
        shutil.copy(os.path.join(ROOT, "deploy", "airgap", "load.sh"), os.path.join(stage, "load.sh"))
        os.chmod(os.path.join(stage, "load.sh"), 0o755)
        for f in ("docs/AIRGAP.md", "LICENSE", "NOTICE"):
            if os.path.exists(os.path.join(ROOT, f)):
                shutil.copy(os.path.join(ROOT, f), os.path.join(stage, os.path.basename(f)))

        # images.tsv: the manifest's image table in a form load.sh reads with
        # nothing but the shell (no jq/python on the air-gapped host).
        with open(os.path.join(stage, "images.tsv"), "w") as fh:
            fh.write("# name\tkind\tarchive\tload_name\tbundle_digest\ttarget_repository\ttarget_tag\tplatforms\tsource_refs\n")
            for im in images:
                fh.write("\t".join([im["name"], im["kind"], im["archive"], im["load_name"], im["bundle_digest"],
                                    im["target_repository"], im["target_tag"],
                                    ",".join(sorted(im["platforms"])),
                                    ",".join(im["source_refs"]) or "-"]) + "\n")

        template = values_template(images, version)
        if only:
            log("partial bundle (--only): the values template is NOT verified to cover the chart")
        else:
            verify_template(probes, template, work)
        with open(os.path.join(stage, "airgap", "values-airgap.template.yaml"), "w") as fh:
            fh.write(template)

        for im in images:
            im["archive_sha256"] = sha256_file(os.path.join(stage, im["archive"]))

        plan = signing_plan()
        files = {}
        for dirpath, _, filenames in os.walk(stage):
            for f in filenames:
                full = os.path.join(dirpath, f)
                files[os.path.relpath(full, stage)] = sha256_file(full)
        manifest = {
            "schema": "kgf-nexus.airgap-bundle", "v": 1,
            "version": version,
            "git": {"commit": commit, "dirty": dirty, "describe": git("describe", "--tags", "--always", "--dirty")},
            "build_time": build_time,
            "builder": {"image_tool": tool, "docker": run(["docker", "version", "--format", "{{.Server.Version}}"], quiet=True).stdout.strip(),
                        "helm": run(["helm", "version", "--short"], quiet=True).stdout.strip(),
                        "go": run(["go", "env", "GOVERSION"], quiet=True).stdout.strip()},
            "platforms": platforms,
            "partial": bool(only),
            "signed": plan["signed"],
            "signature": ({k: plan[k] for k in ("method", "mode", "payload", "bundle")} if plan["signed"]
                          else {"reason": plan["reason"]}),
            "chart": chart, "binaries": binaries, "sdk": sdk,
            "images": images,
            "files": dict(sorted(files.items())),
        }
        with open(os.path.join(stage, "manifest.json"), "w") as fh:
            json.dump(manifest, fh, indent=2)
            fh.write("\n")
        files["manifest.json"] = sha256_file(os.path.join(stage, "manifest.json"))
        with open(os.path.join(stage, "SHA256SUMS"), "w") as fh:
            for path in sorted(files):
                fh.write(f"{files[path]}  {path}\n")
        if plan["signed"]:
            sign(stage, plan)
        else:
            log(f"NOT signed: {plan['reason']}")

        # 5. the tarball
        out = os.path.join(a.out_dir, f"{bundle_dir}.tar.gz")
        tmp_out = out + ".partial"
        with open(tmp_out, "wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=source_epoch, compresslevel=6) as gz:
            with tarfile.open(fileobj=gz, mode="w", format=tarfile.PAX_FORMAT) as tar:
                for dirpath, dirnames, filenames in os.walk(stage):
                    dirnames.sort()
                    for name in sorted(filenames):
                        full = os.path.join(dirpath, name)
                        info = tar.gettarinfo(full, arcname=os.path.join(bundle_dir, os.path.relpath(full, stage)))
                        info.uid = info.gid = 0
                        info.uname = info.gname = ""
                        info.mtime = source_epoch
                        info.mode = 0o755 if os.access(full, os.X_OK) else 0o644
                        with open(full, "rb") as fh:
                            tar.addfile(info, fh)
        os.replace(tmp_out, out)
        with open(out + ".sha256", "w") as fh:
            fh.write(f"{sha256_file(out)}  {os.path.basename(out)}\n")
        size = os.path.getsize(out)
        log(f"wrote {out} ({size / 1e6:.1f} MB, {len(images)} images, signed={plan['signed']})")
        print(out)
    finally:
        if a.keep_work:
            log(f"staging kept at {work}")
        else:
            shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    main()
