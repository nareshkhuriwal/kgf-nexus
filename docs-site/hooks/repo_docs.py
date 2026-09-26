# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Render the repository's own design and operations documents in the site.

The runbooks, the architecture and the ADRs live next to the code they
describe (docs/, deploy/) and are reviewed there. Copying them into
docs-site/docs would create a second copy that drifts, so this hook adds them
to the build as generated pages instead: the file in the repository stays the
only source.

It also makes links honest in both directions:

* A relative link inside a repository document is resolved against that
  document's location in the REPOSITORY. If the target is also rendered in the
  site, the link is rewritten to the site page; otherwise it becomes a link to
  the file in the source repository. A target that does not exist is reported
  as a warning, which ``mkdocs build --strict`` turns into a failure.
* A site page links to repository files with the ``repo:`` scheme, for
  example ``[the example](repo:examples/hello_dag/dag.py)``. The path is
  checked against the working tree at build time, so a renamed file breaks
  the build instead of the reader's click.

Fenced code blocks and inline code are never rewritten.
"""

from __future__ import annotations

import logging
import posixpath
import re
from pathlib import Path

from mkdocs.structure.files import File, Files

log = logging.getLogger("mkdocs.plugins.nexus.repo_docs")

# Repository document -> site page. Order is irrelevant; paths are POSIX.
REPO_DOCS: dict[str, str] = {
    "docs/ARCHITECTURE.md": "design/architecture.md",
    "docs/MULTITENANCY.md": "design/multitenancy.md",
    "docs/RBAC-DESIGN.md": "design/rbac-design.md",
    "docs/RBAC-IMPLEMENTATION.md": "design/rbac-implementation.md",
    "docs/CLUSTER-SCALING.md": "design/cluster-scaling.md",
    "docs/COMPATIBILITY.md": "design/compatibility.md",
    "docs/THIRD-PARTY-LICENSES.md": "design/third-party-licenses.md",
    "docs/OPERATIONS.md": "operations/runbook.md",
    "docs/TLS.md": "operations/tls.md",
    "deploy/README.md": "operations/deploy.md",
    "deploy/AZURE-PROVISIONING.md": "operations/azure-provisioning.md",
    "deploy/terraform/README.md": "operations/terraform/index.md",
    "deploy/terraform/environments/aks/README.md": "operations/terraform/aks.md",
    "deploy/terraform/environments/eks/README.md": "operations/terraform/eks.md",
    "deploy/terraform/environments/gke/README.md": "operations/terraform/gke.md",
}
# Every ADR, discovered rather than listed so a new ADR cannot be forgotten.
ADR_DIR = "docs/adr"
ADR_SITE_DIR = "design/adr"

_FENCE = re.compile(r"^\s{0,3}(`{3,}|~{3,})")
# [text](target) and [text](target "title"); images too, via the optional '!'.
_INLINE_LINK = re.compile(r"(!?\[(?:[^\[\]]|\[[^\]]*\])*\]\()(<[^>]+>|[^()\s]+(?:\([^()\s]*\)[^()\s]*)*)((?:\s+\"[^\"]*\")?\))")
# [label]: target
_REF_DEF = re.compile(r"^(\s{0,3}\[[^\]]+\]:\s*)(\S+)(.*)$")
_CODE_SPAN = re.compile(r"(`+)(.+?)\1")


def _repo_root(config) -> Path:
    # docs-site/mkdocs.yml -> repository root.
    return Path(config["config_file_path"]).resolve().parent.parent


def _repo_url(config) -> str:
    url = (config.get("repo_url") or "").rstrip("/")
    if not url:
        raise ValueError("repo_docs hook: repo_url must be set in mkdocs.yml")
    return url


def _branch(config) -> str:
    return config.get("extra", {}).get("repo_branch", "main")


def _all_repo_docs(root: Path) -> dict[str, str]:
    docs = dict(REPO_DOCS)
    for adr in sorted((root / ADR_DIR).glob("*.md")):
        name = "index.md" if adr.name == "README.md" else adr.name
        docs[f"{ADR_DIR}/{adr.name}"] = f"{ADR_SITE_DIR}/{name}"
    return docs


def on_config(config, **_):
    root = _repo_root(config)
    mapping = _all_repo_docs(root)
    missing = [p for p in mapping if not (root / p).is_file()]
    if missing:
        raise ValueError(f"repo_docs hook: mapped documents do not exist: {missing}")
    config["_nexus_repo_docs"] = mapping
    _add_adrs_to_nav(config["nav"], mapping)
    config["_nexus_site_to_repo"] = {v: k for k, v in mapping.items()}
    # Rebuild on edits to the source documents under `mkdocs serve`.
    for p in mapping:
        config["watch"].append(str(root / p))
    return config


def _add_adrs_to_nav(nav, mapping: dict[str, str]) -> bool:
    """Expand the "design/adr/index.md" nav entry into the index plus every ADR."""
    adrs = sorted(v for k, v in mapping.items() if k.startswith(ADR_DIR + "/") and not v.endswith("index.md"))
    for i, item in enumerate(nav or []):
        if isinstance(item, dict):
            ((title, value),) = item.items()
            if value == f"{ADR_SITE_DIR}/index.md":
                nav[i] = {title: [value] + adrs}
                return True
            if isinstance(value, list) and _add_adrs_to_nav(value, mapping):
                return True
    return False


def on_files(files: Files, config, **_):
    root = _repo_root(config)
    for repo_path, site_path in config["_nexus_repo_docs"].items():
        if files.get_file_from_path(site_path) is not None:
            raise ValueError(f"repo_docs hook: {site_path} exists in docs-site/docs and would shadow {repo_path}")
        files.append(File.generated(config, site_path, abs_src_path=str(root / repo_path)))
    return files


def _split_target(target: str) -> tuple[str, str]:
    path, sep, frag = target.partition("#")
    return path, (sep + frag)


def _is_external(target: str) -> bool:
    return bool(re.match(r"^[a-zA-Z][a-zA-Z0-9+.-]*:", target)) and not target.startswith("repo:")


class _Rewriter:
    def __init__(self, config, page_src: str):
        self.root = _repo_root(config)
        self.url = _repo_url(config)
        self.branch = _branch(config)
        self.mapping: dict[str, str] = config["_nexus_repo_docs"]
        self.page_src = page_src
        # Where this page's relative links are resolved from.
        site_to_repo = config["_nexus_site_to_repo"]
        self.repo_origin = site_to_repo.get(page_src)
        self.problems: list[str] = []

    def _repo_link(self, repo_path: str, frag: str) -> str:
        full = self.root / repo_path
        if not full.exists():
            self.problems.append(f"links to '{repo_path}', which does not exist in the repository")
            return repo_path + frag
        kind = "tree" if full.is_dir() else "blob"
        return f"{self.url}/{kind}/{self.branch}/{repo_path.rstrip('/')}{frag}"

    def _site_link(self, site_path: str, frag: str) -> str:
        rel = posixpath.relpath(site_path, posixpath.dirname(self.page_src) or ".")
        return rel + frag

    def target(self, raw: str) -> str:
        bracketed = raw.startswith("<") and raw.endswith(">")
        t = raw[1:-1] if bracketed else raw
        out = self._rewrite(t)
        return f"<{out}>" if bracketed else out

    def _rewrite(self, t: str) -> str:
        if t.startswith("repo:"):
            path, frag = _split_target(t[len("repo:"):])
            path = posixpath.normpath(path.lstrip("/")) if path not in ("", "/") else ""
            if path in self.mapping:
                return self._site_link(self.mapping[path], frag)
            return self._repo_link(path, frag)
        if self.repo_origin is None or not t or t.startswith("#") or _is_external(t) or t.startswith("/"):
            return t
        path, frag = _split_target(t)
        resolved = posixpath.normpath(posixpath.join(posixpath.dirname(self.repo_origin), path))
        if resolved.startswith("../"):
            self.problems.append(f"links to '{t}', which is outside the repository")
            return t
        if resolved in self.mapping:
            return self._site_link(self.mapping[resolved], frag)
        # A directory whose README is rendered.
        readme = posixpath.join(resolved, "README.md")
        if readme in self.mapping:
            return self._site_link(self.mapping[readme], frag)
        return self._repo_link(resolved, frag)


def _rewrite_line(line: str, rw: _Rewriter) -> str:
    # Code spans are swapped for placeholders first, so a link whose TEXT is a
    # code span ([`api/openapi.yaml`](../api/openapi.yaml)) is still one link,
    # while a link-like string INSIDE a code span is never touched.
    spans: list[str] = []

    def stash(m: re.Match[str]) -> str:
        spans.append(m.group(0))
        return f"\x00{len(spans) - 1}\x00"

    masked = _CODE_SPAN.sub(stash, line)
    masked = _INLINE_LINK.sub(lambda m: m.group(1) + rw.target(m.group(2)) + m.group(3), masked)
    m = _REF_DEF.match(masked)
    if m:
        masked = m.group(1) + rw.target(m.group(2)) + m.group(3)
    return re.sub(r"\x00(\d+)\x00", lambda m: spans[int(m.group(1))], masked)


def on_page_markdown(markdown: str, page, config, files, **_):
    rw = _Rewriter(config, page.file.src_uri)
    lines, fence = [], None
    for line in markdown.split("\n"):
        m = _FENCE.match(line)
        if fence is None and m:
            fence = m.group(1)[0] * len(m.group(1))
            lines.append(line)
            continue
        if fence is not None:
            if line.strip().startswith(fence) and set(line.strip()) <= {fence[0]}:
                fence = None
            lines.append(line)
            continue
        lines.append(_rewrite_line(line, rw))
    for p in rw.problems:
        origin = rw.repo_origin or f"docs-site/docs/{page.file.src_uri}"
        log.warning("%s %s", origin, p)
    return "\n".join(lines)


def on_page_context(context, page, config, **_):
    # "Edit this page" must open the repository file, not a generated copy.
    origin = config["_nexus_site_to_repo"].get(page.file.src_uri)
    if origin:
        page.edit_url = f"{_repo_url(config)}/edit/{_branch(config)}/{origin}"
    return context

