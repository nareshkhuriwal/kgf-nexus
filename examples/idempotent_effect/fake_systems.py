# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Stand-ins for the downstream systems in dag.py.

Each one mimics the DEDUP BEHAVIOUR of the real thing, because that behaviour is
what the example is actually about. `payments` deduplicates on a key like Stripe
does; `ledger_db` has a unique constraint like Postgres does; `mailer` has no dedup
at all, like SMTP; `warehouse` appends blindly.

Every system counts its real invocations, so `demo.py` can assert that retrying
produced exactly one logical effect rather than merely claiming it did.
"""

from __future__ import annotations

from typing import Any


class _Payments:
    """A payment provider with native idempotency-key dedup (Stripe-style).

    The key point: the key is stored ATOMICALLY WITH the charge, inside the system
    performing the effect. That is what makes mechanism 1 window-free, and it is
    not something a client can emulate.
    """

    def __init__(self) -> None:
        self._by_key: dict[str, dict[str, Any]] = {}
        self.real_charges = 0

    def charge(self, *, order_id: str, amount_cents: int, idempotency_key: str) -> dict[str, Any]:
        if idempotency_key in self._by_key:
            # The original charge, returned verbatim. No second charge is created.
            return dict(self._by_key[idempotency_key])
        self.real_charges += 1
        receipt = {
            "charge_id": f"ch_{self.real_charges:04d}",
            "order_id": order_id,
            "amount_cents": amount_cents,
            "customer_email": f"{order_id}@example.test",
            "idempotency_key": idempotency_key,
        }
        self._by_key[idempotency_key] = receipt
        return dict(receipt)


class _LedgerDB:
    """A table with a UNIQUE index on idempotency_key.

    `insert_if_absent` models INSERT ... ON CONFLICT DO NOTHING RETURNING id: the
    check and the write are one atomic operation. A SELECT-then-INSERT would let
    two attempts both pass the check, which is exactly the bug this shape avoids.
    """

    def __init__(self) -> None:
        self._rows: dict[str, dict[str, Any]] = {}
        self.real_inserts = 0

    def insert_if_absent(self, *, idempotency_key: str, order_id: str, amount_cents: int) -> dict[str, Any]:
        if idempotency_key in self._rows:
            return {"id": self._rows[idempotency_key]["id"], "inserted": False}
        self.real_inserts += 1
        row = {
            "id": f"led_{self.real_inserts:04d}",
            "idempotency_key": idempotency_key,
            "order_id": order_id,
            "amount_cents": amount_cents,
        }
        self._rows[idempotency_key] = row
        return {"id": row["id"], "inserted": True}


class _Mailer:
    """An SMTP gateway with NO dedup. Every call sends another email.

    This is why send_receipt has to fall back to the effect journal, and why that
    mechanism is the weakest of the three: the safety lives entirely on the client
    side, so a crash between the send and the journal write duplicates the effect.
    """

    def __init__(self) -> None:
        self.sent: list[dict[str, Any]] = []

    def send(self, *, to: str, subject: str, body: str) -> dict[str, Any]:
        self.sent.append({"to": to, "subject": subject, "body": body})
        return {"message_id": f"msg_{len(self.sent):04d}", "to": to}


class _Warehouse:
    """An append-only archive with no dedup and no way to ask what it holds.

    Nothing on the client side can make this safe, which is the honest reason
    archive_invoice declares idempotency="none".
    """

    def __init__(self) -> None:
        self.entries: list[dict[str, Any]] = []

    def append_invoice(self, receipt: dict[str, Any]) -> dict[str, Any]:
        self.entries.append(dict(receipt))
        return {"entry_id": f"arc_{len(self.entries):04d}"}


payments = _Payments()
ledger_db = _LedgerDB()
mailer = _Mailer()
warehouse = _Warehouse()


def reset() -> None:
    """Clear every system's recorded state, in place.

    Mutates the existing objects rather than rebinding the module globals: dag.py
    does `from fake_systems import payments`, which binds by VALUE, so a rebind
    here would leave dag.py holding the old instances and the demo's counters would
    silently measure nothing.
    """
    payments.__init__()  # type: ignore[misc]
    ledger_db.__init__()  # type: ignore[misc]
    mailer.__init__()  # type: ignore[misc]
    warehouse.__init__()  # type: ignore[misc]
