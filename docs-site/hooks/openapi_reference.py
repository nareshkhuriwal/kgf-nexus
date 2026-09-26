# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Render api/openapi.yaml as REST API reference pages.

The OpenAPI document is the contract: a Go test fails when it and the
gateway's registered routes disagree. These pages are generated from it at
build time, one page per tag plus a schema page, and the raw document is
published beside them for tools (``reference/api/openapi.yaml``).

Rendering is done here rather than with a JavaScript viewer so the site works
offline, needs no CDN, and is covered by the same strict build and link check
as every other page.
"""

from __future__ import annotations

import json
import re
from pathlib import Path

import yaml
from mkdocs.structure.files import File, Files

METHODS = ("get", "put", "post", "delete", "patch", "head", "options")
API_DIR = "reference/api"


def _repo_root(config) -> Path:
    return Path(config["config_file_path"]).resolve().parent.parent


def _slug(s: str) -> str:
    return re.sub(r"[^a-z0-9]+", "-", s.lower()).strip("-")


def _cell(s: object) -> str:
    return str(s or "").replace("\n", " ").replace("|", "\\|").strip()


def _ref_name(ref: str) -> str:
    return ref.rsplit("/", 1)[-1]


def _refs(node, acc: set[str]) -> set[str]:
    if isinstance(node, dict):
        r = node.get("$ref")
        if isinstance(r, str) and r.startswith("#/components/schemas/"):
            acc.add(_ref_name(r))
        for v in node.values():
            _refs(v, acc)
    elif isinstance(node, list):
        for v in node:
            _refs(v, acc)
    return acc


def _type(schema: dict | None) -> str:
    if not schema:
        return ""
    if "$ref" in schema:
        n = _ref_name(schema["$ref"])
        return f"[{n}](schemas.md#{_slug(n)})"
    t = schema.get("type", "")
    if isinstance(t, list):
        t = " \\| ".join(t)
    if t == "array" and isinstance(schema.get("items"), dict):
        return f"array of {_type(schema['items'])}"
    if "enum" in schema:
        return f"{t} (one of {', '.join('`' + str(e) + '`' for e in schema['enum'])})"
    fmt = schema.get("format")
    return f"{t} ({fmt})" if fmt else str(t)


def _block(value, lang: str = "yaml") -> str:
    if lang == "json":
        body = json.dumps(value, indent=2, ensure_ascii=False)
    else:
        body = yaml.safe_dump(value, sort_keys=False, allow_unicode=True, width=100).rstrip()
    return f"```{lang}\n{body}\n```"


def _examples(media: dict) -> list[str]:
    out = []
    if "example" in media:
        out += ["Example:", "", _block(media["example"], "json"), ""]
    for name, ex in (media.get("examples") or {}).items():
        title = ex.get("summary") or name
        if "value" in ex:
            out += [f"Example — {title}:", "", _block(ex["value"], "json"), ""]
    return out


def _media(content: dict) -> list[str]:
    out = []
    for ctype, media in (content or {}).items():
        schema = media.get("schema")
        out.append(f"`{ctype}`" + (f" — {_type(schema)}" if schema else ""))
        out.append("")
        if schema and "$ref" not in schema:
            out += ["<details><summary>Schema</summary>", "", _block(schema), "", "</details>", ""]
        out += _examples(media)
    return out


def _resolve(doc: dict, obj: dict) -> dict:
    ref = obj.get("$ref") if isinstance(obj, dict) else None
    if ref and ref.startswith("#/"):
        node = doc
        for part in ref[2:].split("/"):
            node = node[part.replace("~1", "/").replace("~0", "~")]
        return node
    return obj


def _operation(doc: dict, method: str, path: str, item: dict, op: dict) -> list[str]:
    anchor = op.get("operationId") or _slug(f"{method} {path}")
    out = [f"## {op.get('summary') or method.upper() + ' ' + path} {{#{anchor}}}", ""]
    out += [f"```http\n{method.upper()} {path}\n```", ""]
    if op.get("deprecated"):
        out += ['!!! warning "Deprecated"', ""]
    if op.get("description"):
        out += [op["description"].strip(), ""]

    facts = []
    security = op.get("security", doc.get("security"))
    if security == []:
        facts.append("**Authentication:** none")
    elif security:
        names = sorted({k for req in security for k in req})
        facts.append("**Authentication:** " + ", ".join(f"`{n}`" for n in names))
    perms = op.get("x-nexus-permissions")
    if perms is not None:
        facts.append("**Permissions:** " + (", ".join(f"`{p}`" for p in perms) if perms else "any authenticated principal"))
    if op.get("x-nexus-action"):
        facts.append(f"**Action class:** `{op['x-nexus-action']}`")
    cond = op.get("x-nexus-conditional")
    if cond:
        facts.append(f"**Registered only when:** {cond if isinstance(cond, str) else json.dumps(cond)}")
    if facts:
        out += ["  \n".join(facts), ""]

    params = [_resolve(doc, p) for p in item.get("parameters", []) + op.get("parameters", [])]
    if params:
        out += ["**Parameters**", "", "| Name | In | Type | Required | Description |", "| --- | --- | --- | --- | --- |"]
        for p in params:
            out.append(
                f"| `{p.get('name')}` | {p.get('in')} | {_type(p.get('schema'))} | "
                f"{'yes' if p.get('required') else 'no'} | {_cell(p.get('description'))} |"
            )
        out.append("")

    body = op.get("requestBody")
    if body:
        body = _resolve(doc, body)
        out += ["**Request body**" + (" (required)" if body.get("required") else ""), ""]
        if body.get("description"):
            out += [body["description"].strip(), ""]
        out += _media(body.get("content", {}))

    out += ["**Responses**", "", "| Status | Description | Body |", "| --- | --- | --- |"]
    details = []
    for status, resp in (op.get("responses") or {}).items():
        resp = _resolve(doc, resp)
        content = resp.get("content") or {}
        kinds = ", ".join(
            f"`{c}`" + (f" {_type(m.get('schema'))}" if m.get("schema") else "") for c, m in content.items()
        )
        out.append(f"| {status} | {_cell(resp.get('description'))} | {kinds} |")
        for media in content.values():
            ex = _examples(media)
            if ex:
                details += [f"**Response `{status}`**", ""] + ex
    out.append("")
    out += details
    return out


def _tag_pages(doc: dict) -> dict[str, list[tuple[str, str, dict, dict]]]:
    by_tag: dict[str, list] = {}
    for path, item in (doc.get("paths") or {}).items():
        for method in METHODS:
            op = item.get(method)
            if op is None:
                continue
            tag = (op.get("tags") or ["Other"])[0]
            by_tag.setdefault(tag, []).append((method, path, item, op))
    return by_tag


def generate(doc: dict) -> dict[str, str]:
    pages: dict[str, str] = {}
    info = doc.get("info", {})
    tags_meta = {t["name"]: t for t in doc.get("tags", [])}
    by_tag = _tag_pages(doc)
    order = [t["name"] for t in doc.get("tags", []) if t["name"] in by_tag] + sorted(
        t for t in by_tag if t not in tags_meta
    )

    index = [
        f"# {info.get('title', 'REST API')}",
        "",
        f"OpenAPI {doc.get('openapi')} · API version `{info.get('version', '')}`",
        "",
        (info.get("description") or "").strip(),
        "",
        "The machine-readable document is [openapi.yaml](openapi.yaml) (also served by every gateway at "
        "`GET /v1/openapi.yaml`). It is checked against the gateway's registered routes by "
        "[a Go test](repo:services/api-gateway/rest), so a route cannot be added or removed without "
        "updating it.",
        "",
    ]
    schemes = (doc.get("components") or {}).get("securitySchemes") or {}
    if schemes:
        index += ["## Authentication", "", "| Scheme | Type | Description |", "| --- | --- | --- |"]
        for name, s in schemes.items():
            kind = s.get("type", "") + (f" ({s['scheme']})" if s.get("scheme") else "")
            index.append(f"| `{name}` | {kind} | {_cell(s.get('description'))} |")
        index.append("")
    index += ["## Operations", "", "| Method | Path | Summary |", "| --- | --- | --- |"]
    for tag in order:
        page = f"{_slug(tag)}.md"
        for method, path, _item, op in by_tag[tag]:
            anchor = op.get("operationId") or _slug(f"{method} {path}")
            index.append(f"| {method.upper()} | [`{path}`]({page}#{anchor}) | {_cell(op.get('summary'))} |")
    index.append("")
    pages["index.md"] = "\n".join(index)

    for tag in order:
        meta = tags_meta.get(tag, {})
        lines = [f"# {tag}", ""]
        if meta.get("description"):
            lines += [meta["description"].strip(), ""]
        for method, path, item, op in by_tag[tag]:
            lines += _operation(doc, method, path, item, op)
        pages[f"{_slug(tag)}.md"] = "\n".join(lines)

    schemas = (doc.get("components") or {}).get("schemas") or {}
    lines = ["# Schemas", "", "Every named schema in the document, as written there.", ""]
    for name in sorted(schemas):
        s = schemas[name]
        lines += [f"## {name} {{#{_slug(name)}}}", ""]
        if isinstance(s, dict) and s.get("description"):
            lines += [s["description"].strip(), ""]
        lines += [_block(s), ""]
    pages["schemas.md"] = "\n".join(lines)
    return pages


def _expand_nav(nav, target: str, entries: list) -> bool:
    """Replace the nav item whose value is `target` with a section of entries."""
    for i, item in enumerate(nav or []):
        if isinstance(item, dict):
            ((title, value),) = item.items()
            if value == target:
                nav[i] = {title: entries}
                return True
            if isinstance(value, list) and _expand_nav(value, target, entries):
                return True
    return False


def on_config(config, **_):
    # One nav entry per tag page, in the document's tag order, so a new tag
    # appears in the navigation without editing mkdocs.yml.
    doc = yaml.safe_load((_repo_root(config) / "api" / "openapi.yaml").read_text(encoding="utf-8"))
    by_tag = _tag_pages(doc)
    names = [t["name"] for t in doc.get("tags", []) if t["name"] in by_tag]
    names += sorted(t for t in by_tag if t not in names)
    entries = [f"{API_DIR}/index.md"] + [{n: f"{API_DIR}/{_slug(n)}.md"} for n in names]
    entries.append({"Schemas": f"{API_DIR}/schemas.md"})
    if not _expand_nav(config["nav"], f"{API_DIR}/index.md", entries):
        raise ValueError(f"openapi_reference hook: no nav entry for {API_DIR}/index.md")
    return config


def on_files(files: Files, config, **_):
    src = _repo_root(config) / "api" / "openapi.yaml"
    doc = yaml.safe_load(src.read_text(encoding="utf-8"))
    for name, content in generate(doc).items():
        path = f"{API_DIR}/{name}"
        if files.get_file_from_path(path) is not None:
            raise ValueError(f"openapi_reference hook: {path} is generated; remove the hand-written copy")
        files.append(File.generated(config, path, content=content))
    files.append(File.generated(config, f"{API_DIR}/openapi.yaml", abs_src_path=str(src)))
    return files
