# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Usage units: what a task consumed that is not time (ADR-0032 §2).

A call to a hosted model is billed per token, not per reserved CPU-second, so
the platform's usage ledger (ADR-0021) also records *units* a task reports
itself. This module owns the key convention those units are recorded under.
The price-book workstream prices exactly these keys, so the convention is a
contract, not a style:

    tokens:<provider>/<model>:input      input (prompt) tokens
    tokens:<provider>/<model>:output     output (completion) tokens
    tokens:<provider>/<model>:cached     cached-input tokens, when reported
    requests:<provider>/<model>          provider calls made
    <kind>:<subject>[:<qualifier>]       anything else

Grammar: ``kind`` and ``qualifier`` are ``[a-z][a-z0-9_]{0,31}``; ``subject``
is ``[A-Za-z0-9][A-Za-z0-9._/@+-]{0,127}``; the whole key is at most 200
bytes. Values are non-negative integers (at most 10^15 per key per attempt);
a fractional quantity uses a smaller unit. At most 64 keys per attempt.

Because every key contains ``:`` and no custom resource name can, usage keys
share the ledger's ``custom`` object with reserved custom resources without
ever colliding.

The control plane re-validates everything (and refuses a bad report whole);
validating here as well means the author sees the mistake at the call that
made it, with a message naming the key, instead of as a lost report later.
"""

from __future__ import annotations

import re
from typing import Mapping

__all__ = [
    "InvalidUsage",
    "KEY_PATTERN",
    "SUBJECT_PATTERN",
    "MAX_KEY_BYTES",
    "MAX_KEYS",
    "MAX_VALUE",
    "validate_usage_key",
    "validate_usage",
    "validate_subject",
    "usage_subject",
    "token_units",
    "merge_units",
]

#: The full key grammar (ADR-0032 §2).
KEY_PATTERN = re.compile(
    r"^[a-z][a-z0-9_]{0,31}:[A-Za-z0-9][A-Za-z0-9._/@+-]{0,127}(:[a-z][a-z0-9_]{0,31})?$"
)
#: The subject grammar alone: what a rate-limit key is (``openai``,
#: ``openai/gpt-4o-mini``).
SUBJECT_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/@+-]{0,127}$")
MAX_KEY_BYTES = 200
MAX_KEYS = 64
MAX_VALUE = 10**15

_SUBJECT_CHAR = re.compile(r"[^A-Za-z0-9._/@+-]")


class InvalidUsage(ValueError):
    """A usage key or value outside the convention. Nothing was reported."""


def validate_usage_key(key: str) -> str:
    """Return ``key`` unchanged if it is a valid usage key; raise otherwise."""
    if not isinstance(key, str):
        raise InvalidUsage(f"usage key must be a string, got {type(key).__name__}")
    if len(key.encode("utf-8")) > MAX_KEY_BYTES:
        raise InvalidUsage(f"usage key {key[:40]!r}... is over {MAX_KEY_BYTES} bytes")
    if not KEY_PATTERN.match(key):
        raise InvalidUsage(
            f"usage key {key!r} does not match <kind>:<subject>[:<qualifier>] "
            "(kind/qualifier [a-z][a-z0-9_]*, subject [A-Za-z0-9][A-Za-z0-9._/@+-]*); "
            "use nexus.usage.token_units for token counts"
        )
    return key


def validate_usage(units: Mapping[str, int]) -> dict[str, int]:
    """Validate a usage report; return it as a plain dict.

    Every key must match the grammar and every value must be an ``int`` in
    ``[0, 10^15]`` (``bool`` is refused: ``True`` is an int to Python and a
    mistake to everyone else). Zero values are kept -- they are harmless and
    dropping them would surprise a caller comparing what it sent.
    """
    if not isinstance(units, Mapping):
        raise InvalidUsage(f"usage must be a mapping of key -> int, got {type(units).__name__}")
    if len(units) > MAX_KEYS:
        raise InvalidUsage(f"usage report has {len(units)} keys; at most {MAX_KEYS} per attempt")
    out: dict[str, int] = {}
    for key, value in units.items():
        validate_usage_key(key)
        if isinstance(value, bool) or not isinstance(value, int):
            raise InvalidUsage(
                f"usage value for {key!r} must be an int, got {type(value).__name__}; "
                "report a fractional quantity in a smaller unit"
            )
        if value < 0 or value > MAX_VALUE:
            raise InvalidUsage(f"usage value for {key!r} must be in [0, {MAX_VALUE}], got {value}")
        out[key] = value
    return out


def validate_subject(subject: str) -> str:
    """Return ``subject`` if it is a valid subject (a rate-limit key); raise otherwise."""
    if not isinstance(subject, str) or not SUBJECT_PATTERN.match(subject):
        raise InvalidUsage(
            f"{subject!r} is not a valid subject: expected [A-Za-z0-9][A-Za-z0-9._/@+-]{{0,127}}, "
            "e.g. 'openai' or 'openai/gpt-4o-mini'"
        )
    return subject


def usage_subject(provider: str, model: str | None = None) -> str:
    """The ``<provider>/<model>`` subject, made to fit the grammar.

    ``provider`` is lower-cased (the convention). The model id is kept verbatim
    except that any character outside the subject alphabet becomes ``_``: an
    OpenAI fine-tuned id such as ``ft:gpt-4o-mini:org::abc`` contains ``:``,
    which a key cannot, and becomes ``ft_gpt-4o-mini_org__abc``. The mapping is
    deterministic, so a price book can key on the mapped form.
    """
    p = provider.strip().lower()
    if not p:
        raise InvalidUsage("provider must be non-empty")
    subject = p if model is None else f"{p}/{model}"
    subject = _SUBJECT_CHAR.sub("_", subject)
    return validate_subject(subject)


def token_units(
    provider: str,
    model: str,
    *,
    input_tokens: int = 0,
    output_tokens: int = 0,
    cached_tokens: int = 0,
    requests: int = 0,
) -> dict[str, int]:
    """Usage units for one or more model calls, under the agreed key names.

        >>> token_units("openai", "gpt-4o-mini", input_tokens=12, output_tokens=3, requests=1)
        {'tokens:openai/gpt-4o-mini:input': 12, 'tokens:openai/gpt-4o-mini:output': 3, 'requests:openai/gpt-4o-mini': 1}

    Zero entries are omitted, so a report never carries noise keys (and a
    task that makes calls to many models stays well under the 64-key cap).
    """
    subject = usage_subject(provider, model)
    candidates = (
        (f"tokens:{subject}:input", input_tokens),
        (f"tokens:{subject}:output", output_tokens),
        (f"tokens:{subject}:cached", cached_tokens),
        (f"requests:{subject}", requests),
    )
    return validate_usage({k: v for k, v in candidates if v})


def merge_units(into: dict[str, int], add: Mapping[str, int]) -> dict[str, int]:
    """Add ``add`` into ``into`` per key (in place) and return ``into``."""
    for k, v in add.items():
        into[k] = into.get(k, 0) + v
    return into
