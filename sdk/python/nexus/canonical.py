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

__all__ = ["canonical_json", "canonical_bytes", "spec_hash", "CanonicalError"]


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


def spec_hash(value: Any) -> str:
    """The spec's content address: sha256 of canonical JSON, hex-encoded.

    Computed client-side so `nexusctl workflow compile --show` can print the
    hash the server will independently arrive at. A mismatch between the two is
    a canonicalization bug and should be loud.
    """
    return hashlib.sha256(canonical_bytes(value)).hexdigest()
