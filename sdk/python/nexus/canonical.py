# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Canonical JSON encoding and content addressing.

The server content-addresses a submitted spec (`sha256` of canonical JSON,
ADR-0004) and treats a byte-identical resubmission as a no-op. That makes
canonicalization a correctness requirement, not a formatting preference: if this
module emitted dict keys in insertion order, re-running the same unchanged
workflow file on a different Python process would produce a different hash and
therefore a spurious new spec version, and every run would be pinned to a
version nobody intended to create.

Rules, all of them deliberate:
  * object keys sorted lexicographically by UTF-8 code point
  * no insignificant whitespace
  * UTF-8 output, no \\u escaping of non-ASCII (one representation per string)
  * floats are rejected unless they are exactly representable, and are emitted
    with `repr`, because "2.0" vs "2" vs "2.00" are three hashes for one value
  * empty containers and proto3 default values are omitted by the emitters, so
    "field absent" and "field set to its default" cannot hash differently
"""

from __future__ import annotations

import hashlib
import json
from typing import Any

__all__ = ["canonical_json", "canonical_bytes", "spec_hash", "normalize_enums", "CanonicalError"]


class CanonicalError(ValueError):
    """A value that cannot be canonically encoded."""


def _check(value: Any, path: str = "$") -> Any:
    """Reject anything whose JSON encoding is not uniquely determined."""
    if value is None or isinstance(value, (str, bool)):
        return value
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        if value != value or value in (float("inf"), float("-inf")):
            raise CanonicalError(
                f"{path}: NaN/Infinity has no JSON representation"
            )
        # An integral float would encode as "2.0" here but "2" if the same value
        # arrived as an int, so normalize to int and keep one hash per value.
        if value.is_integer():
            return int(value)
        return value
    if isinstance(value, dict):
        out: dict[str, Any] = {}
        for k, v in value.items():
            if not isinstance(k, str):
                raise CanonicalError(f"{path}: object key {k!r} is not a string")
            out[k] = _check(v, f"{path}.{k}")
        return {k: out[k] for k in sorted(out)}
    if isinstance(value, (list, tuple)):
        # List ORDER is significant and preserved; emitters are responsible for
        # sorting lists whose order carries no meaning (tasks, edges).
        return [_check(v, f"{path}[{i}]") for i, v in enumerate(value)]
    raise CanonicalError(f"{path}: {type(value).__name__} is not JSON-encodable")


def canonical_json(value: Any) -> str:
    """Encode to canonical JSON text: sorted keys, no whitespace, UTF-8 literals."""
    return json.dumps(
        _check(value),
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    )


def canonical_bytes(value: Any) -> bytes:
    """Canonical JSON as UTF-8 bytes -- exactly what gets hashed and submitted."""
    return canonical_json(value).encode("utf-8")


def _aliases(prefix: str, *short_forms: str) -> dict[str, str]:
    out: dict[str, str] = {}
    for s in short_forms:
        out[s] = s
        out[prefix + s.upper()] = s
    return out


# The gateway's alias tables (internal/specenum), mirrored: BOTH spellings map
# to the short form. A change there must land here too; the golden file
# tests/parity/spec_hash_golden.json fails otherwise.
_IDEMPOTENCY = _aliases("IDEMPOTENCY_MODE_", "required", "best_effort", "none")
_TRIGGER_RULE = _aliases(
    "TRIGGER_RULE_", "all_success", "all_done", "one_success", "one_failed", "none_failed", "all_skipped"
)
_BACKOFF = _aliases("BACKOFF_STRATEGY_", "fixed", "linear", "exponential")
_ERROR_CLASS = {
    **_aliases(
        "ERROR_CLASS_", "transient", "timeout", "worker_lost", "validation", "permission_denied",
        "application", "oom", "not_found", "infrastructure", "rate_limited",
    ),
    # Architecture 4.3's spelling of ERROR_CLASS_VALIDATION (F20).
    "validation_error": "validation",
}


def normalize_enums(spec: Any) -> Any:
    """A copy of ``spec`` with every enum value in its SHORT spelling.

    The gateway stores and content-addresses a spec only after rewriting
    ``IDEMPOTENCY_MODE_REQUIRED`` to ``required``, ``BACKOFF_STRATEGY_LINEAR`` to
    ``linear`` and so on (services/api-gateway/spec/normalize.go). The SDK emits
    the proto spelling, which the gateway accepts; hashing it as emitted gave a
    different address from the server for every spec with an enum in it
    (CONTRACT-FINDINGS F124). The input is not modified: the compiled spec keeps
    the proto spelling the local runner and replay read.
    """
    if not isinstance(spec, dict):
        return spec
    out = dict(spec)
    tasks = out.get("tasks")
    if not isinstance(tasks, list):
        return out
    new_tasks: list[Any] = []
    for raw in tasks:
        if not isinstance(raw, dict):
            new_tasks.append(raw)
            continue
        t = dict(raw)
        _norm_field(t, "idempotency", _IDEMPOTENCY)
        _norm_field(t, "trigger_rule", _TRIGGER_RULE)
        retry = t.get("retry")
        if isinstance(retry, dict):
            r = dict(retry)
            _norm_field(r, "strategy", _BACKOFF)
            _norm_list(r, "retry_on", _ERROR_CLASS)
            _norm_list(r, "no_retry_on", _ERROR_CLASS)
            t["retry"] = r
        new_tasks.append(t)
    out["tasks"] = new_tasks
    return out


def _norm_field(m: dict[str, Any], key: str, aliases: dict[str, str]) -> None:
    v = m.get(key)
    if isinstance(v, str) and v in aliases:
        m[key] = aliases[v]


def _norm_list(m: dict[str, Any], key: str, aliases: dict[str, str]) -> None:
    v = m.get(key)
    if isinstance(v, list):
        m[key] = [aliases.get(x, x) if isinstance(x, str) else x for x in v]


def spec_hash(value: Any) -> str:
    """The spec's content address: the hash the SERVER stores it under.

    sha256 of the canonical JSON of the spec after the gateway's enum
    normalization (:func:`normalize_enums`), hex-encoded. Computed client-side
    so ``nexusctl workflow compile --show`` and ``CompiledWorkflow.hash`` show
    the address the server will independently arrive at; a mismatch is a
    canonicalization bug and should be loud. The bytes SUBMITTED are
    :func:`canonical_bytes` of the spec as compiled; the gateway normalizes
    them the same way before hashing.
    """
    return hashlib.sha256(canonical_bytes(normalize_enums(value))).hexdigest()
