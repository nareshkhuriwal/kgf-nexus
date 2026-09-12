"""Canonical encoding. The server content-addresses this output (ADR-0004).

These tests exist because an unstable encoding would mint a spurious spec version
on every compile of an unchanged file, and every run would pin a version nobody
intended to create.
"""

from __future__ import annotations

import pytest

from nexus.canonical import CanonicalError, canonical_bytes, canonical_json, spec_hash


def test_key_order_does_not_affect_output() -> None:
    a = {"b": 1, "a": 2, "c": {"z": 1, "y": 2}}
    b = {"c": {"y": 2, "z": 1}, "a": 2, "b": 1}
    assert canonical_json(a) == canonical_json(b)
    assert spec_hash(a) == spec_hash(b)


def test_no_insignificant_whitespace() -> None:
    assert canonical_json({"a": 1, "b": [1, 2]}) == '{"a":1,"b":[1,2]}'


def test_list_order_is_significant() -> None:
    # Lists carry meaning (command argv, for instance); emitters sort the lists
    # whose order does not matter, not this module.
    assert canonical_json([1, 2]) != canonical_json([2, 1])


def test_integral_float_normalizes_to_int() -> None:
    # 2.0 and 2 must not hash differently, or a multiplier written as an int in
    # YAML and a float in Python would produce two spec versions.
    assert canonical_json({"m": 2.0}) == canonical_json({"m": 2})


def test_non_ascii_is_not_escaped() -> None:
    # One representation per string; \u escaping would give two.
    assert canonical_json({"n": "café"}) == '{"n":"café"}'
    assert canonical_bytes({"n": "café"}) == '{"n":"café"}'.encode("utf-8")


def test_rejects_nan_and_infinity() -> None:
    with pytest.raises(CanonicalError):
        canonical_json({"x": float("nan")})
    with pytest.raises(CanonicalError):
        canonical_json({"x": float("inf")})


def test_rejects_non_string_keys() -> None:
    with pytest.raises(CanonicalError, match="not a string"):
        canonical_json({1: "a"})


def test_rejects_unencodable_types() -> None:
    with pytest.raises(CanonicalError, match="not JSON-encodable"):
        canonical_json({"x": object()})


def test_hash_is_sha256_hex() -> None:
    h = spec_hash({"a": 1})
    assert len(h) == 64 and set(h) <= set("0123456789abcdef")


def test_hash_changes_when_content_changes() -> None:
    assert spec_hash({"a": 1}) != spec_hash({"a": 2})
