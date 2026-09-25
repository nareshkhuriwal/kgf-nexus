# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Typed shapes of the schedule and backfill API (services/api-gateway/rest/schedules.go).

TypedDicts rather than classes: the client returns the gateway's JSON as-is, so
a field added by a newer control plane is still there for a caller who wants it,
while a type checker knows the fields that exist today. Every field is optional
(``total=False``) because a response may legitimately omit one -- ``spec_version``
is absent on an unpinned schedule, ``schedule_name`` on some backfill responses.
"""

from __future__ import annotations

from typing import Any, Literal, TypedDict

__all__ = [
    "OverlapPolicy",
    "BackfillState",
    "Schedule",
    "SchedulePreview",
    "Backfill",
    "OVERLAP_POLICIES",
    "MAX_BACKFILL_ACTIVE",
    "MAX_BACKFILL_RUNS",
]

OverlapPolicy = Literal["skip", "allow", "queue_one"]
"""What a tick does while the schedule's previous run is still going."""

OVERLAP_POLICIES: tuple[str, ...] = ("skip", "allow", "queue_one")

BackfillState = Literal["RUNNING", "PAUSED", "COMPLETED", "CANCELLED", "FAILED"]

# The workflow-manager's ceilings (services/workflow-manager/schedules/backfill.go),
# checked client-side so an out-of-range request fails before a round trip.
MAX_BACKFILL_ACTIVE = 50
MAX_BACKFILL_RUNS = 100_000


class Schedule(TypedDict, total=False):
    id: str
    namespace: str
    name: str
    spec_name: str
    #: Present only when the schedule is pinned to one version.
    spec_version: int
    cron: str
    #: IANA zone the cron is evaluated in.
    timezone: str
    params: dict[str, Any]
    priority: int
    overlap_policy: OverlapPolicy
    catchup_limit: int
    enabled: bool
    #: None when the schedule is not armed (disabled, or being fired right now).
    next_fire_at: str | None
    last_fire_at: str | None
    created_at: str
    updated_at: str
    created_by: str
    #: Upcoming fire times in the schedule's zone (create and get only).
    next_fires: list[str]
    #: Set when the stored cron no longer compiles: the schedule cannot fire.
    cron_error: str


class SchedulePreview(TypedDict, total=False):
    cron: str
    timezone: str
    valid: bool
    next_fires: list[str]
    #: Why the expression is invalid, when ``valid`` is False.
    error: str


# Functional syntax because the wire field ``from`` is a Python keyword.
# ``from``/``to`` are the half-open interval [from, to) in RFC 3339; ``cursor_at``
# is the next logical tick to generate, None once the interval is exhausted.
Backfill = TypedDict(
    "Backfill",
    {
        "id": str,
        "schedule_id": str,
        "schedule_name": str,
        "namespace": str,
        "from": str,
        "to": str,
        "cursor_at": "str | None",
        "max_active": int,
        "max_runs": int,
        "runs_created": int,
        "state": BackfillState,
        "detail": "dict[str, Any]",
        "created_at": str,
        "updated_at": str,
        "ended_at": "str | None",
        "created_by": str,
    },
    total=False,
)
