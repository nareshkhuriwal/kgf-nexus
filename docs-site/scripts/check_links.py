#!/usr/bin/env python3
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Check every link in the built site.

``mkdocs build --strict`` already validates Markdown links and anchors between
pages. This checks the rendered HTML, which also covers what the Markdown
check cannot see: links produced by the theme, by mkdocstrings
cross-references, by the generated API and CLI reference, raw HTML in pages,
and assets.

For every ``href``/``src`` in every HTML file under the site directory:

* a relative or root-relative URL must resolve to a file in the site
  (a directory resolves through its ``index.html``);
* a ``#fragment`` must be an ``id`` (or ``name``) in the target page;
* external URLs are listed with ``--list-external`` and are fetched only with
  ``--external``, so the default run needs no network and is deterministic.

Standard library only. Exit 0 when every internal link resolves, 1 otherwise,
listing each broken link as ``page: url (reason)``.

Usage: check_links.py SITE_DIR [--external] [--list-external]
"""

from __future__ import annotations

import argparse
import sys
import urllib.error
import urllib.request
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import unquote, urldefrag, urlsplit

LINK_ATTRS = {("a", "href"), ("link", "href"), ("img", "src"), ("script", "src"), ("source", "src")}
# The theme's search worker and sitemap are fine to point at; these schemes are not links to check.
IGNORED_SCHEMES = {"mailto", "tel", "javascript", "data"}


class _Page(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.ids: set[str] = set()
        self.links: list[str] = []
        self.base: str | None = None

    def handle_starttag(self, tag, attrs):
        d = dict(attrs)
        for key in ("id", "name") if tag == "a" else ("id",):
            if d.get(key):
                self.ids.add(d[key])
        if tag == "base" and d.get("href"):
            self.base = d["href"]
        for t, attr in LINK_ATTRS:
            if tag == t and d.get(attr):
                # rel=preconnect/dns-prefetch etc. are hints, not navigable links.
                if tag == "link" and d.get("rel") in ("preconnect", "dns-prefetch"):
                    continue
                self.links.append(d[attr])


def _parse(path: Path) -> _Page:
    p = _Page()
    p.feed(path.read_text(encoding="utf-8", errors="replace"))
    return p


def _target_file(site: Path, page: Path, url: str) -> Path:
    path = unquote(urlsplit(url).path)
    if path.startswith("/"):
        target = site / path.lstrip("/")
    elif path == "":
        target = page
    else:
        target = (page.parent / path).resolve()
    if target.is_dir() or path.endswith("/"):
        target = target / "index.html"
    return target


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("site", type=Path)
    ap.add_argument("--external", action="store_true", help="also fetch external URLs")
    ap.add_argument("--list-external", action="store_true", help="print external URLs found")
    args = ap.parse_args(argv)
    site = args.site.resolve()
    if not (site / "index.html").is_file():
        print(f"check_links: {site} is not a built site (no index.html)", file=sys.stderr)
        return 2

    pages = {p.resolve(): _parse(p) for p in site.rglob("*.html")}
    broken: list[str] = []
    external: dict[str, str] = {}
    checked = 0
    for page, parsed in sorted(pages.items()):
        rel_page = page.relative_to(site)
        for url in parsed.links:
            scheme = urlsplit(url).scheme
            if scheme in IGNORED_SCHEMES:
                continue
            if scheme in ("http", "https") or url.startswith("//"):
                external.setdefault(url, str(rel_page))
                continue
            checked += 1
            target = _target_file(site, page, url)
            if not target.exists():
                broken.append(f"{rel_page}: {url} (no such file: {target.relative_to(site) if target.is_relative_to(site) else target})")
                continue
            _, frag = urldefrag(url)
            if frag and target.suffix == ".html":
                ids = pages.get(target.resolve())
                if ids is None:
                    ids = _parse(target)
                if unquote(frag) not in ids.ids:
                    broken.append(f"{rel_page}: {url} (no id '{unquote(frag)}' in {target.relative_to(site)})")

    if args.list_external:
        for url, where in sorted(external.items()):
            print(f"external: {url}  (first seen in {where})")
    if args.external:
        for url, where in sorted(external.items()):
            try:
                req = urllib.request.Request(url, method="HEAD", headers={"User-Agent": "nexus-docs-linkcheck"})
                with urllib.request.urlopen(req, timeout=15):
                    pass
            except urllib.error.HTTPError as e:
                if e.code not in (403, 405, 429):  # HEAD refused or rate limited is not "broken"
                    broken.append(f"{where}: {url} (HTTP {e.code})")
            except (urllib.error.URLError, TimeoutError) as e:
                broken.append(f"{where}: {url} ({e})")

    for b in broken:
        print(b)
    print(
        f"check_links: {len(pages)} pages, {checked} internal links checked, "
        f"{len(external)} external URLs {'fetched' if args.external else 'not fetched'}, {len(broken)} broken",
        file=sys.stderr,
    )
    return 1 if broken else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
