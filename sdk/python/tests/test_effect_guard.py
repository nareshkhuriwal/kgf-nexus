# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""The effect guard and idempotency key derivation.

This is the correctness core of ADR-0006. The tests assert the SKIP PATH: that a
second attempt of the same logical work sees the first attempt's journal and does
not repeat the side effect. The mechanism is that the idempotency key excludes
attempt_number (CONTRACT.md invariant 1), so both attempts address the same
journal entry.
"""

from __future__ import annotations

import pytest

from nexus.context import (
    EffectNotRecorded,
    FileResultStore,
    InMemoryResultStore,
    TaskContext,
    derive_idempotency_key,
)


def _ctx(store, attempt: int = 1, *, run_id: str = "run-1", node_id: str = "charge", version: int = 1) -> TaskContext:
    """A context for one attempt. Only attempt_number varies between attempts."""
    return TaskContext(
        run_id=run_id,
        node_id=node_id,
        attempt_id=f"attempt-{attempt}",
        attempt_number=attempt,
        task_ref="billing.charge",
        task_version=version,
        idempotency_key=derive_idempotency_key(run_id, node_id, None, version),
        store=store,
    )


# ── key derivation ────────────────────────────────────────────────────────────


def test_key_excludes_attempt_number() -> None:
    """The single most important property in the system.

    If retries got different keys, every downstream dedup would be a no-op and
    at-least-once would become at-least-once-with-duplicate-effects.
    """
    store = InMemoryResultStore()
    assert _ctx(store, 1).idempotency_key == _ctx(store, 7).idempotency_key


def test_key_varies_with_each_included_component() -> None:
    base = derive_idempotency_key("run-1", "node-1", None, 1)
    assert derive_idempotency_key("run-2", "node-1", None, 1) != base
    assert derive_idempotency_key("run-1", "node-2", None, 1) != base
    assert derive_idempotency_key("run-1", "node-1", 0, 1) != base
    # Bumping task_version is an INTENTIONAL new logical effect, so it must change
    # the key -- otherwise a version bump would be deduplicated away.
    assert derive_idempotency_key("run-1", "node-1", None, 2) != base


def test_absent_map_index_is_the_dash_sentinel() -> None:
    # Per §6.2 the absent map_index is "-", not the empty string, so a non-mapped
    # node and map_index="" cannot collide.
    assert derive_idempotency_key("r", "n", None, 1) == derive_idempotency_key("r", "n", "-", 1)
    assert derive_idempotency_key("r", "n", None, 1) == derive_idempotency_key("r", "n", "", 1)
    assert derive_idempotency_key("r", "n", 0, 1) != derive_idempotency_key("r", "n", None, 1)


def test_key_is_sha256_hex() -> None:
    k = derive_idempotency_key("r", "n", None, 1)
    assert len(k) == 64 and set(k) <= set("0123456789abcdef")


def test_key_derivation_requires_identity() -> None:
    with pytest.raises(ValueError):
        derive_idempotency_key("", "n", None, 1)
    with pytest.raises(ValueError):
        derive_idempotency_key("r", "", None, 1)


# ── the skip path ─────────────────────────────────────────────────────────────


def test_retry_skips_the_recorded_effect() -> None:
    """The whole point: attempt 2 reads attempt 1's journal and does not re-charge."""
    store = InMemoryResultStore()
    charges: list[str] = []

    def charge_customer(ctx: TaskContext, order_id: str) -> dict:
        if ctx.effect_recorded("charge"):
            return ctx.previous_result("charge")
        charges.append(ctx.idempotency_key)
        receipt = {"receipt_id": f"rc-{len(charges)}", "order_id": order_id}
        ctx.record_effect("charge", receipt)
        return receipt

    first = charge_customer(_ctx(store, attempt=1), "order-9")
    second = charge_customer(_ctx(store, attempt=2), "order-9")
    third = charge_customer(_ctx(store, attempt=3), "order-9")

    assert len(charges) == 1, "the side effect ran more than once across retries"
    assert first == second == third, "a retry returned a different receipt"


def test_a_different_run_is_a_different_effect() -> None:
    """Dedup must not bleed across runs -- each run is new logical work."""
    store = InMemoryResultStore()
    calls: list[str] = []

    def handler(ctx: TaskContext) -> str:
        if ctx.effect_recorded("write"):
            return ctx.previous_result("write")
        calls.append(ctx.run_id)
        ctx.record_effect("write", ctx.run_id)
        return ctx.run_id

    handler(_ctx(store, run_id="run-A"))
    handler(_ctx(store, run_id="run-A", attempt=2))
    handler(_ctx(store, run_id="run-B"))
    assert calls == ["run-A", "run-B"]


def test_version_bump_is_a_new_logical_effect() -> None:
    store = InMemoryResultStore()
    calls: list[int] = []

    def handler(ctx: TaskContext) -> int:
        if ctx.effect_recorded("e"):
            return ctx.previous_result("e")
        calls.append(ctx.task_version)
        ctx.record_effect("e", ctx.task_version)
        return ctx.task_version

    handler(_ctx(store, version=1))
    handler(_ctx(store, version=1, attempt=2))
    handler(_ctx(store, version=2))
    assert calls == [1, 2]


def test_independent_named_effects_in_one_task() -> None:
    """A task doing two effects can guard them separately.

    This matters for the partial-failure case: if the second effect fails, a retry
    should skip the first and redo only the second.
    """
    store = InMemoryResultStore()
    done: list[str] = []

    def two_effects(ctx: TaskContext, fail_on_second: bool) -> None:
        if not ctx.effect_recorded("a"):
            done.append("a")
            ctx.record_effect("a", 1)
        if not ctx.effect_recorded("b"):
            if fail_on_second:
                raise RuntimeError("downstream b is down")
            done.append("b")
            ctx.record_effect("b", 2)

    with pytest.raises(RuntimeError):
        two_effects(_ctx(store, attempt=1), True)
    two_effects(_ctx(store, attempt=2), False)
    # "a" is not repeated even though attempt 1 failed after doing it.
    assert done == ["a", "b"]


def test_record_effect_with_no_value() -> None:
    store = InMemoryResultStore()
    ctx = _ctx(store)
    ctx.record_effect("fired")
    assert ctx.effect_recorded("fired") is True
    assert _ctx(store, attempt=2).previous_result("fired") is None


def test_previous_result_for_an_unrecorded_effect_raises() -> None:
    ctx = _ctx(InMemoryResultStore())
    with pytest.raises(EffectNotRecorded, match="never recorded"):
        ctx.previous_result("nope")


def test_journal_survives_a_process_boundary(tmp_path) -> None:
    """Cross-process durability: a retry is usually a NEW process on a new worker.

    An in-memory-only journal would pass every other test here and be useless in
    production, so this uses the file store and separate context objects.
    """
    charges: list[str] = []

    def handler(ctx: TaskContext) -> str:
        if ctx.effect_recorded("charge"):
            return ctx.previous_result("charge")
        charges.append(ctx.attempt_id)
        ctx.record_effect("charge", "rc-1")
        return "rc-1"

    assert handler(_ctx(FileResultStore(str(tmp_path)), attempt=1)) == "rc-1"
    # A brand-new store object over the same directory, as a new worker process
    # would construct.
    assert handler(_ctx(FileResultStore(str(tmp_path)), attempt=2)) == "rc-1"
    assert len(charges) == 1


def test_unreadable_journal_fails_toward_re_execution(tmp_path, capsys) -> None:
    """A corrupt journal must not silently skip the work.

    Treating an unreadable journal as "recorded" would mean the effect never
    happens and nobody notices; treating it as "not recorded" risks a duplicate,
    which is the platform's stated guarantee (at-least-once) and is detectable.
    """
    ctx = _ctx(FileResultStore(str(tmp_path)))
    ctx.record_effect("charge", "rc-1")

    journal = tmp_path / ("effects_" + ctx.idempotency_key + ".json")
    assert journal.exists(), "the journal should be a real file at this key"
    journal.write_text("{ this is not json")

    fresh = _ctx(FileResultStore(str(tmp_path)), attempt=2)
    assert fresh.effect_recorded("charge") is False
    assert "unreadable" in capsys.readouterr().err


# ── progress signalling ───────────────────────────────────────────────────────


def test_heartbeat_and_progress_reach_the_agent_callback() -> None:
    seen: list[tuple[str | None, float | None]] = []
    ctx = _ctx(InMemoryResultStore())
    ctx._heartbeat_fn = lambda msg, frac: seen.append((msg, frac))

    ctx.heartbeat("connecting")
    ctx.progress(0.5, "halfway")
    ctx.progress(1.0)
    assert seen == [("connecting", None), ("halfway", 0.5), (None, 1.0)]


def test_progress_fraction_is_bounded() -> None:
    ctx = _ctx(InMemoryResultStore())
    with pytest.raises(ValueError):
        ctx.progress(1.5)
    with pytest.raises(ValueError):
        ctx.progress(-0.1)


def test_remaining_ms_uses_the_control_plane_deadline() -> None:
    import time

    ctx = _ctx(InMemoryResultStore())
    assert ctx.remaining_ms() is None  # no deadline supplied
    ctx.start_to_close_deadline_unix_ms = int(time.time() * 1000) + 60_000
    remaining = ctx.remaining_ms()
    assert remaining is not None and 55_000 < remaining <= 60_000
    ctx.start_to_close_deadline_unix_ms = int(time.time() * 1000) - 1000
    assert ctx.remaining_ms() == 0


def test_context_repr_does_not_leak_the_full_key() -> None:
    # The key is what a downstream system dedupes on; a full key in a log line is
    # a replay primitive for anyone who can read logs.
    ctx = _ctx(InMemoryResultStore())
    assert ctx.idempotency_key not in repr(ctx)
    assert ctx.idempotency_key[:12] in repr(ctx)


# ── structured logging ────────────────────────────────────────────────────────


def test_log_lines_carry_correlation_fields() -> None:
    import io
    import json

    from nexus.context import StructuredLogger

    buf = io.StringIO()
    log = StructuredLogger({"run_id": "r1", "attempt_id": "a1"}, buf)
    log.info("started", node_id="n1")
    record = json.loads(buf.getvalue())
    assert record["run_id"] == "r1" and record["attempt_id"] == "a1"
    assert record["level"] == "INFO" and record["msg"] == "started"
    assert record["node_id"] == "n1"


def test_log_survives_an_unserializable_field() -> None:
    import io
    import json

    from nexus.context import StructuredLogger

    buf = io.StringIO()
    StructuredLogger({}, buf).info("weird", obj=object())
    # A log call must never be the thing that fails a task.
    assert json.loads(buf.getvalue())["msg"] == "weird"


# ── result store ──────────────────────────────────────────────────────────────


def test_small_results_are_inlined_and_large_ones_are_blobs() -> None:
    store = InMemoryResultStore()
    small = store.put({"rows": 42})
    assert "inline" in small, "a tiny result should not cost a second round-trip"
    assert store.get(small) == {"rows": 42}

    big = store.put({"payload": "x" * 100_000})
    assert "blob" in big and big["blob"]["sha256"]
    assert store.get(big)["payload"].startswith("xxx")


def test_file_store_is_content_addressed(tmp_path) -> None:
    store = FileResultStore(str(tmp_path))
    a = store.put({"payload": "y" * 100_000})
    b = store.put({"payload": "y" * 100_000})
    # Content addressing gives deduplication for free (ADR-0008).
    assert a["blob"]["key"] == b["blob"]["key"]
    assert store.get(a) == store.get(b)
