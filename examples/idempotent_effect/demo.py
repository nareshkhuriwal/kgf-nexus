# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Prove the effect guard works, locally, with no control plane running.

Retries here are simulated the way they actually happen: a NEW TaskContext with a
higher attempt_number but THE SAME idempotency key, because the key excludes
attempt_number (CONTRACT.md invariant 1). That is the whole mechanism, and this
script is the smallest thing that demonstrates it.

    python3 examples/idempotent_effect/demo.py
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

import fake_systems
from dag import archive_invoice, charge_customer, record_ledger, send_receipt
from fake_systems import ledger_db, mailer, payments, warehouse

from nexus import InMemoryResultStore, TaskContext, derive_idempotency_key

RUN_ID = "run-demo-0001"


def context_for(node_id: str, attempt: int, store) -> TaskContext:
    """Build the context the worker would build for one attempt.

    Only attempt_number changes between attempts. The key is derived from
    (run_id, node_id, map_index, task_version) and is therefore IDENTICAL for
    every attempt of the same node -- which is what every guard below relies on.
    """
    return TaskContext(
        run_id=RUN_ID,
        node_id=node_id,
        attempt_id=f"{node_id}-attempt-{attempt}",
        attempt_number=attempt,
        task_ref=f"billing.{node_id}",
        task_version=1,
        idempotency_key=derive_idempotency_key(RUN_ID, node_id, None, 1),
        store=store,
    )


def rule(title: str) -> None:
    print(f"\n{'=' * 74}\n{title}\n{'=' * 74}")


def main() -> int:
    failures: list[str] = []

    def check(label: str, actual, expected) -> None:
        ok = actual == expected
        print(f"  [{'PASS' if ok else 'FAIL'}] {label}: {actual} (expected {expected})")
        if not ok:
            failures.append(label)

    # ── keys ────────────────────────────────────────────────────────────────
    rule("The idempotency key excludes attempt_number")
    store = InMemoryResultStore()
    k1 = context_for("charge_customer", 1, store).idempotency_key
    k5 = context_for("charge_customer", 5, store).idempotency_key
    print(f"  attempt 1 key: {k1}")
    print(f"  attempt 5 key: {k5}")
    check("attempts 1 and 5 share one key", k1 == k5, True)
    other = context_for("record_ledger", 1, store).idempotency_key
    check("a different node gets a different key", other != k1, True)

    # ── mechanism 1 ─────────────────────────────────────────────────────────
    rule("MECHANISM 1 -- downstream-native dedup (strongest, no window)")
    fake_systems.reset()
    store = InMemoryResultStore()
    receipts = [
        charge_customer(context_for("charge_customer", n, store), "order-42", 4999)
        for n in (1, 2, 3)
    ]
    print("  three attempts ran the handler; the provider deduplicated on the key")
    check("real charges at the provider", payments.real_charges, 1)
    check("every attempt returned the same charge", len({r["charge_id"] for r in receipts}), 1)

    rule("MECHANISM 1 still holds with NO journal at all")
    # The strong claim: delete the client-side guard entirely and mechanism 1 is
    # still correct, because the dedup lives inside the provider. This is what
    # separates it from mechanism 3, where the guard IS the safety.
    fake_systems.reset()
    for n in (1, 2, 3):
        ctx = context_for("charge_customer", n, InMemoryResultStore())  # fresh store each time
        payments.charge(order_id="order-42", amount_cents=4999, idempotency_key=ctx.idempotency_key)
    check("real charges with the journal unavailable", payments.real_charges, 1)

    # ── mechanism 2 ─────────────────────────────────────────────────────────
    rule("MECHANISM 2 -- conditional write (no window)")
    fake_systems.reset()
    store = InMemoryResultStore()
    charge = charge_customer(context_for("charge_customer", 1, store), "order-42", 4999)
    rows = [record_ledger(context_for("record_ledger", n, store), charge) for n in (1, 2, 3)]
    print("  three attempts issued the insert; the unique index absorbed two of them")
    check("real ledger inserts", ledger_db.real_inserts, 1)
    check("every attempt saw the same row", len({r["ledger_id"] for r in rows}), 1)

    # ── mechanism 3 ─────────────────────────────────────────────────────────
    rule("MECHANISM 3 -- platform effect journal (weakest; window narrowed)")
    fake_systems.reset()
    store = InMemoryResultStore()
    charge = charge_customer(context_for("charge_customer", 1, store), "order-42", 4999)
    ledger = record_ledger(context_for("record_ledger", 1, store), charge)
    for n in (1, 2, 3):
        send_receipt(context_for("send_receipt", n, store), charge=charge, ledger=ledger)
    print("  the journal let attempts 2 and 3 skip a mailer with no dedup of its own")
    check("emails actually sent", len(mailer.sent), 1)

    rule("MECHANISM 3 -- the window that REMAINS (this is the honest part)")
    fake_systems.reset()
    store = InMemoryResultStore()
    charge = charge_customer(context_for("charge_customer", 1, store), "order-42", 4999)
    ledger = record_ledger(context_for("record_ledger", 1, store), charge)

    # Attempt 1 crashes in the window: the mail is sent, the journal write never
    # happens. This is the real failure mode, reproduced exactly.
    ctx1 = context_for("send_receipt", 1, store)
    mailer.send(to="x@example.test", subject="Receipt", body="...")
    print("  attempt 1: mail sent, then the process died BEFORE record_effect")

    ctx2 = context_for("send_receipt", 2, store)
    send_receipt(ctx2, charge=charge, ledger=ledger)
    print("  attempt 2: no journal entry to find, so it sent again")
    check("emails sent (the duplicate the journal cannot prevent)", len(mailer.sent), 2)
    print("\n  This is why mechanism 3 is documented as WEAKEST. The window shrank")
    print("  from the whole task to one store write -- it did not disappear. A task")
    print("  relying on it declares best_effort, so a duplicate shows up as a")
    print("  PossibleDuplicateExecution event rather than being assumed impossible.")

    # ── idempotency=none ────────────────────────────────────────────────────
    rule('idempotency="none" -- a task that cannot be made safe')
    fake_systems.reset()
    store = InMemoryResultStore()
    receipt = {"charge_id": "ch_0001", "order_id": "order-42"}
    for n in (1, 2):
        archive_invoice(context_for("archive_invoice", n, store), receipt)
    print("  the archive has no dedup, so two attempts really did append twice")
    check("archive entries after 2 attempts", len(warehouse.entries), 2)
    print("\n  Nothing client-side can fix this, which is why the task declares")
    print('  idempotency="none". The platform then REFUSES to auto-retry an')
    print("  indeterminate outcome: the attempt is marked FAILED with")
    print("  reason=indeterminate_outcome and a human decides (§6.5). Costly, and")
    print("  better than silently duplicating financial records.")

    rule("Summary")
    if failures:
        print(f"  {len(failures)} check(s) FAILED: {', '.join(failures)}")
        return 1
    print("  All checks passed.")
    print("  Mechanisms 1 and 2 have no duplicate window. Mechanism 3 narrows one.")
    print('  A task that can offer none of the three says so with idempotency="none".')
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
