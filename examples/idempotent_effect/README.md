# idempotent_effect — why retrying this DAG cannot double-charge anyone

Nexus is **at-least-once** (ADR-0006). A task handler can run more than once for
one logical unit of work. This example shows the three mechanisms that make that
safe, in descending order of strength, and is honest about what the weakest one
does not give you.

## Run it

The demo needs no control plane. It simulates retries the way they actually
happen — a new `TaskContext` with a higher `attempt_number` but the **same**
idempotency key — and asserts on real invocation counts:

```bash
python3 -m venv .venv && ./.venv/bin/pip install -e sdk/python

./.venv/bin/python examples/idempotent_effect/demo.py
```

Then the compile and submit path:

```bash
nexusctl workflow compile examples/idempotent_effect/dag.py --show --python ./.venv/bin/python
nexusctl workflow submit examples/idempotent_effect/dag.py --dry-run --python ./.venv/bin/python

nexusctl run create idempotent-effect \
  --param order_id=order-42 --param amount_cents=4999 \
  --idempotency-key charge-order-42
```

## Files

| File | What it is |
|---|---|
| `dag.py` | Four tasks, each demonstrating one mechanism. The docstrings are the content. |
| `fake_systems.py` | Stand-ins that mimic each real system's dedup behaviour and count invocations. |
| `demo.py` | Runs the scenarios and asserts exactly-one-logical-effect. |
| `compiled.yaml` | The compiled WorkflowSpec. |

## The key, and why retrying is safe at all

```
idempotency_key = sha256(run_id ‖ node_id ‖ map_index ‖ task_version)
```

**`attempt_number` is deliberately excluded.** That exclusion is the entire
mechanism: attempt 1 and attempt 5 of a node receive the *identical* key, so
anything deduplicating on that key treats them as one effect. Including the
attempt number would make every retry a fresh effect and turn deduplication into
a no-op.

The platform derives the key. Users never invent one — hand-rolled keys are where
this goes wrong in practice.

## The three mechanisms

### 1. Downstream-native dedup — strongest, no window

`charge_customer` passes `ctx.idempotency_key` to the payment provider's own
idempotency mechanism. The provider stores the key *atomically with* the charge; a
second request with the same key returns the original charge.

There is **no window**. Even if the process dies one instruction after the
provider commits, the retry gets the same charge back. The effect guard in that
handler is only an optimisation that avoids a round trip — delete it and the task
is still correct. Delete the `idempotency_key=` argument and it is not.

Reach for this first whenever the downstream system offers it. Most modern
payment, messaging, and provisioning APIs do.

### 2. Conditional write — no window

`record_ledger` issues `INSERT ... ON CONFLICT (idempotency_key) DO NOTHING`. The
uniqueness constraint is on the stable key, so a retry's insert conflicts and
writes nothing.

Also **no window**, because the check and the write are one atomic statement in
the target store. Available whenever you control the schema.

What would be *wrong*: `SELECT` then `INSERT`. Two attempts can both pass the
SELECT before either INSERTs. The atomicity is the whole point.

### 3. Platform effect journal — weakest; window narrowed, not closed

`send_receipt` uses `ctx.effect_recorded()` / `ctx.record_effect()`, because the
SMTP gateway has neither a dedup key nor a conditional write. The sequence is:

```
1. effect_recorded("receipt")?  -> if yes, return; send nothing
2. mailer.send(...)                                  <-- the effect
3. ctx.record_effect("receipt", ...)                 <-- the journal write
```

**The window is between steps 2 and 3.** If the process dies there, the mail was
sent but not journaled, and the retry sends a second one. `demo.py` reproduces
exactly this and asserts that two emails go out — the window is real and this
example does not pretend otherwise.

What the journal buys is the window's *size*: it shrinks from the whole task to a
single store write. That is a large improvement, and it is not a guarantee.

Note the ordering: effect first, journal second. Journaling first would mean a
crash in between skips the effect **forever** — a receipt nobody gets and nobody
notices. A duplicate receipt is an annoyance; a silently-never-sent one is a
support ticket in three weeks. Given the choice, prefer the visible failure.

This task declares `idempotency="best_effort"`, so a duplicate surfaces as a
`PossibleDuplicateExecution` event rather than being assumed impossible.

## When a task cannot be made safe: `idempotency="none"`

`archive_invoice` appends to an archive with no dedup of any kind. None of the
three mechanisms applies, so it declares `idempotency="none"`, which changes
platform behaviour: an indeterminate (`LOST`) outcome is **not** auto-retried. The
attempt is marked `FAILED` with `reason=indeterminate_outcome` and a human
decides (§6.5).

That costs operator attention, which is a real cost. It is the right trade: the
alternative is the platform silently duplicating financial records because
retrying was more convenient than admitting it did not know. Such tasks are also
flagged in the UI, so the cost is visible rather than discovered later.

The correct long-term fix is not a cleverer retry policy — it is to give the
archive a dedup key so the task can be promoted to `required`. Declaring `none` is
an accurate description of today, not a design target.

## Why this ordering is documented instead of abstracted

Mechanisms 1 and 2 have no duplicate window; mechanism 3 has one. A single
`@idempotent` decorator hiding all three would leave users believing they had the
first while relying on the third — a correctness bug they could not see. Being
clear about which guarantee you actually have is worth more than a comfortable
abstraction.
