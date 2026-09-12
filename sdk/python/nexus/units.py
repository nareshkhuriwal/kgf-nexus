"""Human-friendly unit parsing for resources and durations.

Authors write "8Gi" and "4h"; the wire format (proto/nexus/v1/common.proto) is
integer bytes and integer milliseconds. Parsing lives in the SDK -- i.e. in the
user's process -- rather than on the server, because the control plane must
receive a fully-resolved document it can validate without interpreting
author-facing sugar (ADR-0004).

Unit conventions follow Kubernetes deliberately: an orchestrator's users
already know that "Gi" is binary and "G" is decimal, and inventing a third
dialect would only produce quiet off-by-7% resource errors.
"""

from __future__ import annotations

import re
from typing import Final

__all__ = [
    "UnitParseError",
    "parse_cpu_millis",
    "parse_bytes",
    "parse_duration_ms",
    "format_cpu_millis",
    "format_bytes",
    "format_duration_ms",
]


class UnitParseError(ValueError):
    """Raised when a quantity string cannot be parsed.

    A distinct type so callers can catch author mistakes without swallowing
    unrelated ValueErrors from deeper code.
    """


# Binary (Ki) and decimal (K) suffixes, as in Kubernetes resource quantities.
_BYTE_SUFFIXES: Final[dict[str, int]] = {
    "": 1,
    "b": 1,
    "k": 1000,
    "kb": 1000,
    "m": 1000**2,
    "mb": 1000**2,
    "g": 1000**3,
    "gb": 1000**3,
    "t": 1000**4,
    "tb": 1000**4,
    "p": 1000**5,
    "pb": 1000**5,
    "ki": 1024,
    "kib": 1024,
    "mi": 1024**2,
    "mib": 1024**2,
    "gi": 1024**3,
    "gib": 1024**3,
    "ti": 1024**4,
    "tib": 1024**4,
    "pi": 1024**5,
    "pib": 1024**5,
}

_DURATION_UNITS_MS: Final[dict[str, float]] = {
    "ns": 1e-6,
    "us": 1e-3,
    "ms": 1.0,
    "s": 1000.0,
    "m": 60_000.0,
    "h": 3_600_000.0,
    "d": 86_400_000.0,
    "w": 604_800_000.0,
}

_NUM = r"[0-9]+(?:\.[0-9]+)?"
_QUANTITY_RE: Final[re.Pattern[str]] = re.compile(
    rf"^\s*(?P<num>{_NUM})\s*(?P<suffix>[A-Za-z]*)\s*$"
)
# A duration may be compound: "1h30m", "2m10s".
_DURATION_TERM_RE: Final[re.Pattern[str]] = re.compile(
    rf"(?P<num>{_NUM})\s*(?P<unit>ns|us|ms|s|m|h|d|w)", re.IGNORECASE
)


def parse_cpu_millis(value: str | int | float) -> int:
    """Parse a CPU quantity to millicores. "2000m" and "2" both give 2000.

    Fractional cores are accepted ("0.5" -> 500m) because that is how people
    actually think about small tasks, but the wire format is integer millicores
    so the scheduler never does float arithmetic on capacity. Float capacity
    arithmetic accumulates error, and a capacity ledger that drifts eventually
    either overcommits a node or leaks capacity forever.
    """
    if isinstance(value, bool):  # bool is an int subclass; almost never intended.
        raise UnitParseError(f"cpu must be a quantity, got bool {value!r}")
    if isinstance(value, (int, float)):
        millis = float(value) * 1000.0
        return _exact_int(millis, f"cpu {value!r}")

    m = _QUANTITY_RE.match(value)
    if not m:
        raise UnitParseError(
            f"cannot parse cpu {value!r}; expected e.g. '2', '2000m', '0.5'"
        )
    num = float(m.group("num"))
    suffix = m.group("suffix").lower()
    if suffix == "m":
        millis = num
    elif suffix in ("", "c", "core", "cores"):
        millis = num * 1000.0
    else:
        raise UnitParseError(
            f"unknown cpu suffix {m.group('suffix')!r} in {value!r}; "
            "use 'm' for millicores or no suffix for cores"
        )
    return _exact_int(millis, f"cpu {value!r}")


def parse_bytes(value: str | int) -> int:
    """Parse a byte quantity. "8Gi" -> 8589934592, "512Mi" -> 536870912.

    Binary suffixes (Ki/Mi/Gi) and decimal ones (K/M/G) are both accepted and
    mean different things, exactly as in Kubernetes.
    """
    if isinstance(value, bool):
        raise UnitParseError(f"memory must be a quantity, got bool {value!r}")
    if isinstance(value, int):
        if value < 0:
            raise UnitParseError(f"byte quantity must be non-negative, got {value}")
        return value

    m = _QUANTITY_RE.match(value)
    if not m:
        raise UnitParseError(
            f"cannot parse byte quantity {value!r}; expected e.g. '8Gi', '512Mi', '1024'"
        )
    suffix = m.group("suffix").lower()
    if suffix not in _BYTE_SUFFIXES:
        raise UnitParseError(
            f"unknown byte suffix {m.group('suffix')!r} in {value!r}; "
            f"known: {', '.join(sorted(s for s in _BYTE_SUFFIXES if s))}"
        )
    total = float(m.group("num")) * _BYTE_SUFFIXES[suffix]
    return _exact_int(total, f"byte quantity {value!r}")


def parse_duration_ms(value: str | int | float) -> int:
    """Parse a duration to whole milliseconds. "30m" -> 1800000, "4h" -> 14400000.

    Compound forms ("1h30m") are supported. A bare number is interpreted as
    milliseconds, matching the wire format, so a value that round-trips through
    the spec and back does not silently change meaning.
    """
    if isinstance(value, bool):
        raise UnitParseError(f"duration must be a quantity, got bool {value!r}")
    if isinstance(value, (int, float)):
        return _exact_int(float(value), f"duration {value!r}")

    text = value.strip()
    if not text:
        raise UnitParseError("duration is empty")

    # A bare number means milliseconds.
    if re.fullmatch(_NUM, text):
        return _exact_int(float(text), f"duration {value!r}")

    total_ms = 0.0
    consumed = 0
    for m in _DURATION_TERM_RE.finditer(text):
        if m.start() != consumed:
            raise UnitParseError(
                f"cannot parse duration {value!r}: unexpected "
                f"{text[consumed:m.start()]!r} at offset {consumed}"
            )
        total_ms += float(m.group("num")) * _DURATION_UNITS_MS[m.group("unit").lower()]
        consumed = m.end()
    if consumed == 0 or consumed != len(text):
        raise UnitParseError(
            f"cannot parse duration {value!r}; expected e.g. '10s', '30m', '4h', '1h30m'"
        )
    return _exact_int(total_ms, f"duration {value!r}")


def _exact_int(v: float, what: str) -> int:
    """Convert to int, refusing a value that would lose precision.

    Rounding silently would turn "1.5m" cpu into either 1 or 2 millicores worth
    of drift per task; better to make the author write something exact.
    """
    if v < 0:
        raise UnitParseError(f"{what} must be non-negative")
    r = round(v)
    if abs(v - r) > 1e-9:
        raise UnitParseError(
            f"{what} does not resolve to a whole unit (got {v}); "
            "use a smaller unit to express it exactly"
        )
    return int(r)


def format_cpu_millis(millis: int) -> str:
    """Render millicores back to author form, for `compile --show` style output."""
    if millis % 1000 == 0:
        return str(millis // 1000)
    return f"{millis}m"


def format_bytes(n: int) -> str:
    """Render bytes using the largest binary suffix that stays exact."""
    for suffix, scale in (
        ("Pi", 1024**5),
        ("Ti", 1024**4),
        ("Gi", 1024**3),
        ("Mi", 1024**2),
        ("Ki", 1024),
    ):
        if n >= scale and n % scale == 0:
            return f"{n // scale}{suffix}"
    return str(n)


def format_duration_ms(ms: int) -> str:
    """Render milliseconds as a compact compound duration ("5400000" -> "1h30m")."""
    if ms == 0:
        return "0s"
    parts: list[str] = []
    for unit, scale in (("d", 86_400_000), ("h", 3_600_000), ("m", 60_000), ("s", 1000)):
        if ms >= scale:
            whole, ms = divmod(ms, scale)
            parts.append(f"{whole}{unit}")
    if ms:
        parts.append(f"{ms}ms")
    return "".join(parts)
