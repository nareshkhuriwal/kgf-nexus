# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Render schemas/workflow-spec-v1.json as the workflow spec reference page.

The JSON Schema is the grammar the gateway enforces (it embeds a copy and a
test keeps the two identical), so the reference is generated from it at
build time rather than written by hand.
"""

from __future__ import annotations

import json
import re
from pathlib import Path

from mkdocs.structure.files import File, Files

SITE_PATH = "reference/workflow-spec.md"


def _repo_root(config) -> Path:
    return Path(config["config_file_path"]).resolve().parent.parent


def _anchor(name: str) -> str:
    return re.sub(r"[^a-z0-9]+", "-", name.lower()).strip("-")


def _cell(s: object) -> str:
    return " ".join(str(s or "").split()).replace("|", "\\|")


def _type(p: dict) -> str:
    if "$ref" in p:
        n = p["$ref"].rsplit("/", 1)[-1]
        return f"[{n}](#{_anchor(n)})"
    if "const" in p:
        return f"exactly `{p['const']}`"
    if "enum" in p:
        short = [e for e in p["enum"] if not (isinstance(e, str) and e.isupper() and "_" in e)]
        return "one of " + ", ".join(f"`{e}`" for e in (short or p["enum"]))
    t = p.get("type", "")
    if isinstance(t, list):
        t = " or ".join(t)
    if t == "array" and isinstance(p.get("items"), dict):
        return f"array of {_type(p['items'])}"
    if t == "object" and isinstance(p.get("additionalProperties"), dict):
        return f"map of {_type(p['additionalProperties'])}"
    extra = []
    for k in ("minimum", "maximum", "pattern", "maxLength"):
        if k in p:
            extra.append(f"{k} `{p[k]}`")
    return f"`{t}`" + (f" ({', '.join(extra)})" if extra else "") if t else ""


def _props(obj: dict) -> list[str]:
    props = obj.get("properties") or {}
    if not props:
        return []
    req = set(obj.get("required") or [])
    out = ["| Field | Type | Required | Description |", "| --- | --- | --- | --- |"]
    for name, p in props.items():
        out.append(f"| `{name}` | {_type(p)} | {'yes' if name in req else ''} | {_cell(p.get('description'))} |")
    return out + [""]


def generate(schema: dict) -> str:
    lines = [
        "# Workflow spec reference",
        "",
        "Generated from [`schemas/workflow-spec-v1.json`](repo:schemas/workflow-spec-v1.json), "
        "the JSON Schema the gateway enforces and serves at `GET /v1/schemas/workflow-spec-v1.json`. "
        "Beyond the schema, the gateway also validates the graph semantically (acyclic, typed edges, "
        "bounded maps); see [workflows](../concepts/workflows.md) and [DAG semantics](../concepts/dag-semantics.md).",
        "",
        "Enumerations accept both a short spelling (`required`) and the protobuf spelling "
        "(`IDEMPOTENCY_MODE_REQUIRED`); only the short one is listed, and the stored document "
        "is normalized to it.",
        "",
        f"`$id`: `{schema.get('$id', '')}`",
        "",
        "## WorkflowSpec",
        "",
        _cell(schema.get("description")),
        "",
    ]
    lines += _props(schema)
    for name, d in (schema.get("$defs") or {}).items():
        if name.startswith("_") or not isinstance(d, dict):
            continue
        lines += [f"## {name} {{#{_anchor(name)}}}", ""]
        if d.get("description"):
            lines += [_cell(d["description"]), ""]
        table = _props(d)
        if table:
            lines += table
        else:
            lines += [f"Type: {_type(d) or '`object`'}", ""]
    return "\n".join(lines)


def on_files(files: Files, config, **_):
    if files.get_file_from_path(SITE_PATH) is not None:
        raise ValueError(f"schema_reference hook: {SITE_PATH} is generated; remove the hand-written copy")
    schema = json.loads((_repo_root(config) / "schemas" / "workflow-spec-v1.json").read_text(encoding="utf-8"))
    files.append(File.generated(config, SITE_PATH, content=generate(schema)))
    return files
