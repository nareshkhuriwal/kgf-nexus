# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Unit parsing. Mundane code, but a wrong answer here silently mis-schedules.

"8Gi" parsed as 8e9 instead of 2**33 is a 7% capacity error on every task, which
shows up as unexplained OOM kills rather than as an obvious bug.
"""

from __future__ import annotations

import pytest

from nexus.units import (
    UnitParseError,
    format_bytes,
    format_cpu_millis,
    format_duration_ms,
    parse_bytes,
    parse_cpu_millis,
    parse_duration_ms,
)


@pytest.mark.parametrize(
    "text,expected",
    [
        ("8Gi", 8 * 1024**3),
        ("8Gi", 8589934592),
        ("512Mi", 536870912),
        ("1Ki", 1024),
        ("1024", 1024),
        ("2Ti", 2 * 1024**4),
        # Decimal suffixes are NOT binary -- this distinction is the whole reason
        # Kubernetes has both spellings.
        ("1G", 1_000_000_000),
        ("1M", 1_000_000),
        ("1k", 1000),
        ("0", 0),
        (" 4Gi ", 4 * 1024**3),
        ("8gi", 8 * 1024**3),
        ("1.5Gi", 1610612736),
    ],
)
def test_parse_bytes(text: str, expected: int) -> None:
    assert parse_bytes(text) == expected


def test_parse_bytes_int_passthrough() -> None:
    assert parse_bytes(4096) == 4096


@pytest.mark.parametrize("bad", ["", "Gi", "8Zi", "eight", "-1Gi", "8 Gi Gi", "1.5.2Gi"])
def test_parse_bytes_rejects_garbage(bad: str) -> None:
    with pytest.raises(UnitParseError):
        parse_bytes(bad)


def test_parse_bytes_rejects_bool() -> None:
    # bool is an int subclass; `memory=True` is never what anyone meant.
    with pytest.raises(UnitParseError):
        parse_bytes(True)  # type: ignore[arg-type]


@pytest.mark.parametrize(
    "text,expected",
    [
        ("2000m", 2000),
        ("2", 2000),
        ("0.5", 500),
        ("1", 1000),
        ("250m", 250),
        ("4000m", 4000),
        ("1.5", 1500),
        (2, 2000),
        (0.25, 250),
    ],
)
def test_parse_cpu_millis(text: object, expected: int) -> None:
    assert parse_cpu_millis(text) == expected  # type: ignore[arg-type]


@pytest.mark.parametrize("bad", ["2Gi", "2cores/sec", "-1", "", "m"])
def test_parse_cpu_rejects_garbage(bad: str) -> None:
    with pytest.raises(UnitParseError):
        parse_cpu_millis(bad)


def test_parse_cpu_rejects_sub_millicore() -> None:
    # 0.0001 cores is 0.1 millicores; rounding it would make the capacity ledger
    # drift, so the author is asked to be exact instead.
    with pytest.raises(UnitParseError, match="whole unit"):
        parse_cpu_millis("0.0001")


@pytest.mark.parametrize(
    "text,expected",
    [
        ("10s", 10_000),
        ("30m", 1_800_000),
        ("4h", 14_400_000),
        ("5m", 300_000),
        ("7d", 604_800_000),
        ("1w", 604_800_000),
        ("500ms", 500),
        ("1h30m", 5_400_000),
        ("2m10s", 130_000),
        ("1d12h", 129_600_000),
        # A bare number is milliseconds, matching the wire format, so a value
        # that round-trips through the spec does not change meaning.
        ("1000", 1000),
        (1000, 1000),
        ("0s", 0),
        ("1.5s", 1500),
        ("4H", 14_400_000),
    ],
)
def test_parse_duration_ms(text: object, expected: int) -> None:
    assert parse_duration_ms(text) == expected  # type: ignore[arg-type]


@pytest.mark.parametrize("bad", ["", "soon", "10x", "h", "1h junk", "-5s", "10 s 20"])
def test_parse_duration_rejects_garbage(bad: str) -> None:
    with pytest.raises(UnitParseError):
        parse_duration_ms(bad)


def test_duration_roundtrip_through_formatter() -> None:
    for text in ("10s", "30m", "4h", "1h30m", "7d"):
        assert parse_duration_ms(format_duration_ms(parse_duration_ms(text))) == parse_duration_ms(text)


def test_formatters() -> None:
    assert format_bytes(8 * 1024**3) == "8Gi"
    assert format_bytes(536870912) == "512Mi"
    assert format_bytes(1023) == "1023"
    assert format_cpu_millis(2000) == "2"
    assert format_cpu_millis(250) == "250m"
    assert format_duration_ms(0) == "0s"
    assert format_duration_ms(5_400_000) == "1h30m"
    assert format_duration_ms(1500) == "1s500ms"
