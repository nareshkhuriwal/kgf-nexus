"""idempotent_effect -- why retrying this DAG cannot double-charge anyone.

THE PROBLEM THIS EXAMPLE EXISTS FOR
-----------------------------------
Nexus is at-least-once (ADR-0006), and that is not a limitation of the
implementation. Consider a worker that charges a customer, commits the charge, and
dies before its report is acknowledged. The control plane sees only silence. It
cannot distinguish "died AFTER the charge" from "died BEFORE the charge". It can
retry (risking a double charge) or not (risking a stalled pipeline). There is no
third option -- this is the Two Generals problem, a property of networks rather
than of engineering quality. Anything marketed as exactly-once is at-least-once
plus deduplication at the effect boundary, which is what this file demonstrates.

So the platform does two things: it retries, and it hands every attempt of the
same logical work THE SAME idempotency key.

    idempotency_key = sha256(run_id ‖ node_id ‖ map_index ‖ task_version)

Note what is NOT in there: attempt_number. That exclusion is the entire mechanism.
Attempt 1 and attempt 5 of one node get the identical key, so whatever
deduplicates on that key sees them as one effect. Including the attempt number
would make every retry a fresh effect and turn dedup into a no-op.

Users never invent these keys. Hand-rolled keys are where this goes wrong in
practice, so the platform derives them and the SDK only hands them over.

THE THREE MECHANISMS, IN DESCENDING ORDER OF STRENGTH
----------------------------------------------------
Each task below demonstrates one. The ordering is not a style preference -- it is
the difference in what you actually get, and the SDK states it plainly rather than
hiding it behind a comfortable abstraction:

  1. charge_customer   DOWNSTREAM-NATIVE DEDUP (strongest). No window.
  2. record_ledger     CONDITIONAL WRITE. No window.
  3. send_receipt      PLATFORM EFFECT JOURNAL (weakest). Window narrowed, not
                       closed.
  4. archive_invoice   idempotency="none" -- what to do when a task CANNOT be
                       made safe.

    nexusctl workflow compile examples/idempotent_effect/dag.py --show
"""

from __future__ import annotations

from nexus import Resources, Retry, TaskContext, Timeouts, task, workflow

# Stand-ins for real systems. Each mimics the dedup behaviour of the real thing so
# the pattern, not the SDK, is what this file is showing.
from fake_systems import ledger_db, mailer, payments, warehouse


# ── MECHANISM 1: downstream-native dedup (STRONGEST) ─────────────────────────
@task(
    task_ref="billing.charge_customer",
    resources=Resources(cpu="500m", memory="512Mi"),
    timeouts=Timeouts(start_to_close="5m", heartbeat="1m"),
    retry=Retry(max_attempts=5, initial="10s", retry_on=["transient", "timeout", "worker_lost"]),
    idempotency="required",
)
def charge_customer(ctx: TaskContext, order_id: str, amount_cents: int) -> dict:
    """Charge a card. Safe to retry because STRIPE deduplicates, not because we do.

    WHY RETRYING THIS IS SAFE
    -------------------------
    `ctx.idempotency_key` is handed to the payment provider's own idempotency
    mechanism. The provider records the key with the charge; a second request
    carrying the same key returns the ORIGINAL charge instead of creating a new
    one. The deduplication happens inside the system performing the effect, which
    is the only place it can be atomic with the effect itself.

    There is NO WINDOW here. Even if this process dies one instruction after the
    provider commits, the retry's request carries the same key and the provider
    returns the first charge. The effect-guard below is a cheap optimisation that
    avoids a network round trip -- it is NOT what makes this safe. Delete the guard
    and this task is still correct; delete the `idempotency_key=` argument and it
    is not.

    This is why mechanism 1 is the strongest and should be reached for first
    whenever the downstream system offers it. Most modern payment, messaging, and
    provisioning APIs do.
    """
    if ctx.effect_recorded("charge"):
        # Purely an optimisation: skip a round trip we know the answer to.
        ctx.log.info("charge already recorded; skipping the API call")
        return ctx.previous_result("charge")

    receipt = payments.charge(
        order_id=order_id,
        amount_cents=amount_cents,
        # THE LOAD-BEARING LINE. Same key on every attempt, so the provider
        # collapses them into one charge.
        idempotency_key=ctx.idempotency_key,
    )
    ctx.record_effect("charge", receipt)
    return receipt


# ── MECHANISM 2: conditional write ───────────────────────────────────────────
@task(
    task_ref="billing.record_ledger",
    resources=Resources(cpu="500m", memory="512Mi"),
    timeouts=Timeouts(start_to_close="5m"),
    retry=Retry(max_attempts=5, initial="5s"),
    idempotency="required",
)
def record_ledger(ctx: TaskContext, charge: dict) -> dict:
    """Write a ledger row. Safe to retry because the DATABASE enforces uniqueness.

    WHY RETRYING THIS IS SAFE
    -------------------------
    The insert is conditional:

        INSERT INTO ledger (idempotency_key, order_id, amount_cents)
        VALUES (%s, %s, %s)
        ON CONFLICT (idempotency_key) DO NOTHING
        RETURNING id

    The uniqueness constraint is on `ctx.idempotency_key`, which is stable across
    attempts. Attempt 2's insert conflicts and writes nothing; RETURNING yields no
    row, so we read the existing one instead.

    There is NO WINDOW here either, because the check and the write are ONE atomic
    statement in the target store. This is the strongest mechanism available
    whenever you control the schema, and it needs no cooperation from the
    downstream system beyond a unique index.

    What would be WRONG: SELECT-then-INSERT. Two attempts can both pass the SELECT
    before either INSERTs, and you get two rows. The atomicity is the whole point;
    a read followed by a write is not a conditional write.
    """
    row = ledger_db.insert_if_absent(
        idempotency_key=ctx.idempotency_key,
        order_id=charge["order_id"],
        amount_cents=charge["amount_cents"],
    )
    if row["inserted"]:
        ctx.log.info("ledger row written", ledger_id=row["id"])
    else:
        # Not an error, and not even unusual: this is a retry landing on work an
        # earlier attempt finished.
        ctx.log.info("ledger row already present; earlier attempt wrote it", ledger_id=row["id"])
    return {"ledger_id": row["id"]}


# ── MECHANISM 3: platform effect journal (WEAKEST) ───────────────────────────
@task(
    task_ref="billing.send_receipt",
    resources=Resources(cpu="250m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="5m"),
    retry=Retry(max_attempts=4, initial="10s"),
    # best_effort, not required: the guarantee this handler can actually offer is
    # weaker than the other two, and the spec should say so rather than overclaim.
    # The platform still auto-retries an indeterminate outcome, but emits a
    # PossibleDuplicateExecution warning event so a duplicate is VISIBLE in the
    # run history instead of being silently assumed impossible.
    idempotency="best_effort",
)
def send_receipt(ctx: TaskContext, charge: dict, ledger: dict) -> dict:
    """Email a receipt. The journal narrows the duplicate window; it does not close it.

    WHY RETRYING THIS IS *MOSTLY* SAFE -- AND WHERE IT IS NOT
    --------------------------------------------------------
    This SMTP gateway has no idempotency key and no conditional write. All that is
    left is the platform's effect journal: record, durably, that the effect landed,
    and check that record before doing it again.

    The sequence is:

        1. effect_recorded("receipt")?  -> if yes, return and send nothing
        2. mailer.send(...)                                  <-- the effect
        3. ctx.record_effect("receipt", ...)                 <-- the journal write

    THE WINDOW IS BETWEEN 2 AND 3. If the process dies there, the mail is sent but
    unjournaled, and the retry sends a second one. That window is REAL and cannot
    be removed without cooperation from the mailer.

    What the journal buys is its SIZE. Without it, any crash anywhere in a
    multi-second task means a possible duplicate. With it, the exposure is the
    microseconds between the send returning and one store write completing. That is
    a large improvement and it is not a guarantee, and those are different claims.

    Note the ORDER, which is not arbitrary: the effect first, the journal second.
    Journaling first would mean a crash in between skips the effect FOREVER -- a
    receipt nobody ever gets and nobody notices. A duplicate receipt is a mild
    annoyance; a silently-never-sent one is a support ticket in three weeks. Given
    a choice between the two failure modes, prefer the visible one.

    This is also why the SDK documents the ordering instead of offering one
    `@idempotent` decorator: mechanisms 1 and 2 have no window and mechanism 3
    does, and a user is entitled to know which one they are relying on.
    """
    if ctx.effect_recorded("receipt"):
        ctx.log.info("receipt already sent by an earlier attempt; not resending")
        return ctx.previous_result("receipt")

    result = mailer.send(
        to=charge["customer_email"],
        subject=f"Receipt for order {charge['order_id']}",
        body=f"Charged {charge['amount_cents']} cents. Ledger {ledger['ledger_id']}.",
    )
    # Immediately after the effect, never before it.
    ctx.record_effect("receipt", result)
    return result


# ── WHEN A TASK CANNOT BE MADE SAFE: idempotency="none" ──────────────────────
@task(
    task_ref="billing.archive_invoice",
    resources=Resources(cpu="500m", memory="1Gi"),
    timeouts=Timeouts(start_to_close="30m"),
    # Still retried on a KNOWN failure: FAILED means we know it ran and did not
    # succeed, so re-running is well-defined.
    retry=Retry(max_attempts=3, retry_on=["transient", "timeout"]),
    # But NOT on an indeterminate one. An attempt that goes LOST is marked FAILED
    # with reason=indeterminate_outcome and waits for a human (§6.5), because the
    # platform cannot know whether the append already happened.
    idempotency="none",
)
def archive_invoice(ctx: TaskContext, receipt: dict) -> dict:
    """Append to an append-only archive that has no dedup of any kind.

    WHY THIS ONE IS DIFFERENT
    -------------------------
    The archive appends blindly: no idempotency key, no unique constraint, no way
    to ask "did I already write this". None of the three mechanisms applies. An
    unlucky retry genuinely produces a duplicate archive entry.

    The honest declaration is `idempotency="none"`, and it changes platform
    behaviour in a specific way: an indeterminate (LOST) outcome is NOT
    auto-retried. The attempt is marked FAILED with
    reason=indeterminate_outcome and a human decides.

    That costs an operator's attention, which is a real cost. It is the right
    trade: the alternative is the platform silently duplicating financial records
    because retrying was more convenient than admitting it did not know. A task
    that declares `none` is also flagged in the UI, so the cost is visible rather
    than discovered later.

    The correct long-term fix is not a cleverer retry policy -- it is to give the
    archive a dedup key so this task can be promoted to `required`. Declaring
    `none` is an accurate description of today, not a design target.
    """
    entry = warehouse.append_invoice(receipt)
    ctx.log.warning(
        "appended to a non-deduplicating archive; an indeterminate outcome here "
        "needs a human, by design",
        entry_id=entry["entry_id"],
    )
    return entry


@workflow(
    name="idempotent-effect",
    description="The three idempotency mechanisms, in descending order of strength.",
    params={
        "order_id": {"type": "string", "required": True},
        "amount_cents": {"type": "int", "required": True},
    },
    run_timeout="2h",
    # Money moves here. Stop on the first terminal failure rather than pressing on
    # into downstream effects whose inputs may be wrong.
    fail_fast=True,
)
def idempotent_effect(order_id: str, amount_cents: int):
    """charge -> ledger -> receipt -> archive, each guarded a different way."""
    charge = charge_customer(order_id, amount_cents)
    ledger = record_ledger(charge)
    receipt = send_receipt(charge=charge, ledger=ledger)
    return archive_invoice(receipt)


if __name__ == "__main__":
    compiled = idempotent_effect.compile()
    print(compiled.to_json(indent=2))
    print(f"\nspec hash: {compiled.hash}")
