"""__WORKFLOW_NAME__ -- extract a CSV, validate it, transform it, load it idempotently.

    extract --> validate --> transform --> load

  * extract    reads the CSV at `source` (a path, file:// or http(s):// URL) and
               fingerprints it. Network failures are retried with backoff.
  * validate   checks every row against the schema below and splits valid rows
               from rejected ones. More than `max_reject_ratio` rejected fails
               the run: loading a mostly-broken file is worse than loading none.
  * transform  normalizes types and computes daily totals. Pure, so it is
               memoized: re-running over the same file reuses the result.
  * load       upserts into `target_db` inside ONE transaction that also records
               the batch under the platform's idempotency key. A retry after a
               crash either finds the batch recorded (and does nothing) or finds
               nothing written at all -- the effect happens once however many
               times the task runs (mechanism 2: conditional write).

Every task runs in the image built from ./Dockerfile via `nexus.entrypoint`.

    pytest                                   # the task functions, no platform needed
    nexusctl workflow compile dag.py --show  # the exact spec the server will see
    nexusctl workflow submit dag.py
    nexusctl run create __WORKFLOW_NAME__ --param source=https://example.com/orders.csv
"""

from __future__ import annotations

import csv
import datetime as dt
import hashlib
import io
import os
import sqlite3
import urllib.request
from typing import Any

from nexus import Cache, Resources, Retry, Runtime, TaskContext, Timeouts, task, workflow

# The image built from ./Dockerfile. Point this at your registry.
IMAGE = "__WORKFLOW_NAME__:0.1.0"

# Refuse sources bigger than this rather than exhausting the task's memory.
MAX_SOURCE_BYTES = 256 * 1024 * 1024

# The input contract: column -> parser. A parser raises ValueError on bad data.
REQUIRED_COLUMNS = ("order_id", "customer_id", "order_date", "amount", "currency")


def _task(name: str, **kwargs: Any):
    """@task with this project's conventions: task_ref and container runtime."""
    ref = f"__WORKFLOW_NAME__.{name}"
    return task(
        task_ref=ref,
        runtime=Runtime(
            executor="docker",
            image=IMAGE,
            command=["python", "-m", "nexus.entrypoint", "--module", "dag"],
            args=[ref],
        ),
        **kwargs,
    )


# ── extract ──────────────────────────────────────────────────────────────────


def _read_source(source: str) -> bytes:
    if source.startswith(("http://", "https://")):
        with urllib.request.urlopen(source, timeout=60) as resp:  # noqa: S310 - scheme checked above
            data = resp.read(MAX_SOURCE_BYTES + 1)
    else:
        path = source[len("file://"):] if source.startswith("file://") else source
        with open(path, "rb") as f:
            data = f.read(MAX_SOURCE_BYTES + 1)
    if len(data) > MAX_SOURCE_BYTES:
        raise ValueError(f"{source} is larger than {MAX_SOURCE_BYTES} bytes; split it or raise MAX_SOURCE_BYTES")
    return data


@_task(
    "extract",
    resources=Resources(cpu="500m", memory="1Gi"),
    timeouts=Timeouts(start_to_close="15m", heartbeat="2m"),
    # Transient network errors and timeouts are worth retrying; a 404 or a
    # malformed file is not, and retrying it only delays the page.
    retry=Retry(max_attempts=4, initial="15s", max="5m", retry_on=["transient", "timeout", "infrastructure"]),
)
def extract(ctx: TaskContext, source: str) -> dict:
    """Read the CSV and return its rows with a content fingerprint."""
    ctx.heartbeat(f"reading {source}")
    raw = _read_source(source)
    text = raw.decode("utf-8-sig")
    reader = csv.DictReader(io.StringIO(text))
    missing = [c for c in REQUIRED_COLUMNS if c not in (reader.fieldnames or [])]
    if missing:
        raise ValueError(f"{source} is missing required columns: {', '.join(missing)}")
    rows = [dict(r) for r in reader]
    ctx.log.info("extracted", source=source, rows=len(rows), bytes=len(raw))
    return {"source": source, "sha256": hashlib.sha256(raw).hexdigest(), "rows": rows}


# ── validate ─────────────────────────────────────────────────────────────────


def _check_row(row: dict) -> str | None:
    """Return why a row is invalid, or None."""
    for col in REQUIRED_COLUMNS:
        if not (row.get(col) or "").strip():
            return f"{col} is empty"
    try:
        dt.date.fromisoformat(row["order_date"].strip())
    except ValueError:
        return f"order_date {row['order_date']!r} is not YYYY-MM-DD"
    try:
        amount = float(row["amount"])
    except ValueError:
        return f"amount {row['amount']!r} is not a number"
    if amount < 0:
        return f"amount {amount} is negative"
    if len(row["currency"].strip()) != 3:
        return f"currency {row['currency']!r} is not a 3-letter code"
    return None


@_task(
    "validate",
    resources=Resources(cpu="500m", memory="1Gi"),
    timeouts=Timeouts(start_to_close="15m"),
    retry=Retry(max_attempts=1),  # deterministic: a retry would reject the same rows
)
def validate(ctx: TaskContext, extracted: dict, max_reject_ratio: float = 0.05) -> dict:
    """Split rows into valid and rejected; fail when too many are rejected."""
    valid, rejected, seen = [], [], set()
    for line, row in enumerate(extracted["rows"], start=2):  # line 1 is the header
        reason = _check_row(row)
        if reason is None and row["order_id"] in seen:
            reason = f"duplicate order_id {row['order_id']}"
        if reason:
            rejected.append({"line": line, "order_id": row.get("order_id"), "reason": reason})
            continue
        seen.add(row["order_id"])
        valid.append(row)
    total = len(extracted["rows"])
    ratio = (len(rejected) / total) if total else 0.0
    ctx.log.info("validated", valid=len(valid), rejected=len(rejected), ratio=round(ratio, 4))
    if total == 0:
        raise ValueError(f"{extracted['source']} has no data rows")
    if ratio > max_reject_ratio:
        sample = "; ".join(f"line {r['line']}: {r['reason']}" for r in rejected[:5])
        raise ValueError(
            f"{len(rejected)} of {total} rows rejected ({ratio:.1%}), over max_reject_ratio "
            f"{max_reject_ratio:.1%}. First rejections: {sample}"
        )
    return {"source": extracted["source"], "sha256": extracted["sha256"], "valid": valid, "rejected": rejected}


# ── transform ────────────────────────────────────────────────────────────────


@_task(
    "transform",
    resources=Resources(cpu="1", memory="2Gi"),
    timeouts=Timeouts(start_to_close="30m"),
    retry=Retry(max_attempts=2),
    # Pure function of its input, so memoizing is safe: the same validated file
    # is never transformed twice within the TTL.
    cache=Cache(enabled=True, ttl="7d"),
)
def transform(ctx: TaskContext, validated: dict) -> dict:
    """Normalize rows (integer cents, upper-case currency) and total per day."""
    rows = []
    daily: dict[tuple[str, str], int] = {}
    for r in validated["valid"]:
        cents = int(round(float(r["amount"]) * 100))
        currency = r["currency"].strip().upper()
        day = dt.date.fromisoformat(r["order_date"].strip()).isoformat()
        rows.append({
            "order_id": r["order_id"].strip(),
            "customer_id": r["customer_id"].strip(),
            "order_date": day,
            "amount_cents": cents,
            "currency": currency,
        })
        daily[(day, currency)] = daily.get((day, currency), 0) + cents
    totals = [
        {"order_date": d, "currency": c, "amount_cents": v} for (d, c), v in sorted(daily.items())
    ]
    return {"source_sha256": validated["sha256"], "rows": rows, "daily_totals": totals}


# ── load ─────────────────────────────────────────────────────────────────────

_SCHEMA = """
CREATE TABLE IF NOT EXISTS orders (
    order_id     TEXT PRIMARY KEY,
    customer_id  TEXT NOT NULL,
    order_date   TEXT NOT NULL,
    amount_cents INTEGER NOT NULL,
    currency     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS daily_totals (
    order_date   TEXT NOT NULL,
    currency     TEXT NOT NULL,
    amount_cents INTEGER NOT NULL,
    PRIMARY KEY (order_date, currency)
);
CREATE TABLE IF NOT EXISTS load_batches (
    idempotency_key TEXT PRIMARY KEY,
    source_sha256   TEXT NOT NULL,
    rows_loaded     INTEGER NOT NULL,
    loaded_at       TEXT NOT NULL
);
"""


def _connect(target_db: str) -> sqlite3.Connection:
    """SQLite keeps the template self-contained. For Postgres, swap this for a
    psycopg connection: the SQL below uses ON CONFLICT, which both support."""
    if os.path.dirname(target_db):
        os.makedirs(os.path.dirname(target_db), exist_ok=True)
    conn = sqlite3.connect(target_db, timeout=30, isolation_level=None)
    conn.executescript(_SCHEMA)
    return conn


@_task(
    "load",
    resources=Resources(cpu="500m", memory="1Gi"),
    timeouts=Timeouts(start_to_close="30m", heartbeat="5m"),
    retry=Retry(max_attempts=5, initial="10s", retry_on=["transient", "timeout", "infrastructure"]),
    # "required": this task is safe to re-run automatically after an
    # indeterminate outcome, BECAUSE of the conditional write below.
    idempotency="required",
)
def load(ctx: TaskContext, transformed: dict, target_db: str) -> dict:
    """Upsert rows and totals, recording the batch in the same transaction."""
    conn = _connect(target_db)
    try:
        conn.execute("BEGIN IMMEDIATE")
        prior = conn.execute(
            "SELECT rows_loaded, loaded_at FROM load_batches WHERE idempotency_key = ?",
            (ctx.idempotency_key,),
        ).fetchone()
        if prior is not None:
            conn.execute("ROLLBACK")
            ctx.log.info("batch already loaded; nothing to do", rows=prior[0])
            return {"rows_loaded": prior[0], "loaded_at": prior[1], "already_loaded": True}

        conn.executemany(
            """INSERT INTO orders (order_id, customer_id, order_date, amount_cents, currency)
               VALUES (:order_id, :customer_id, :order_date, :amount_cents, :currency)
               ON CONFLICT (order_id) DO UPDATE SET
                 customer_id = excluded.customer_id, order_date = excluded.order_date,
                 amount_cents = excluded.amount_cents, currency = excluded.currency""",
            transformed["rows"],
        )
        # Totals are recomputed from the orders table rather than added to, so
        # loading an overlapping file twice cannot double-count.
        days = sorted({(r["order_date"], r["currency"]) for r in transformed["rows"]})
        for day, currency in days:
            conn.execute(
                """INSERT INTO daily_totals (order_date, currency, amount_cents)
                   SELECT order_date, currency, SUM(amount_cents) FROM orders
                    WHERE order_date = ? AND currency = ? GROUP BY order_date, currency
                   ON CONFLICT (order_date, currency) DO UPDATE SET amount_cents = excluded.amount_cents""",
                (day, currency),
            )
        loaded_at = dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds")
        conn.execute(
            "INSERT INTO load_batches (idempotency_key, source_sha256, rows_loaded, loaded_at) VALUES (?, ?, ?, ?)",
            (ctx.idempotency_key, transformed["source_sha256"], len(transformed["rows"]), loaded_at),
        )
        conn.execute("COMMIT")
    except BaseException:
        if conn.in_transaction:
            conn.execute("ROLLBACK")
        raise
    finally:
        conn.close()
    ctx.log.info("loaded", rows=len(transformed["rows"]), target=target_db)
    return {"rows_loaded": len(transformed["rows"]), "loaded_at": loaded_at, "already_loaded": False}


# ── workflow ─────────────────────────────────────────────────────────────────


@workflow(
    name="__WORKFLOW_NAME__",
    namespace="__NAMESPACE__",
    description="Extract a CSV, validate it, transform it and load it idempotently.",
    params={
        "source": {"type": "string", "required": True},
        "target_db": {"type": "string", "required": False, "default": "/data/warehouse.db"},
        "max_reject_ratio": {"type": "float", "required": False, "default": 0.05},
    },
    run_timeout="2h",
    # One load at a time: two concurrent runs writing the same tables is safe
    # but wasteful, and serial loads keep daily totals easy to reason about.
    max_active_runs=1,
)
def pipeline(source: str, target_db: str, max_reject_ratio: float):
    extracted = extract(source=source)
    validated = validate(extracted=extracted, max_reject_ratio=max_reject_ratio)
    transformed = transform(validated=validated)
    return load(transformed=transformed, target_db=target_db)


if __name__ == "__main__":
    compiled = pipeline.compile()
    print(compiled.to_json(indent=2))
    print(f"\nspec hash: {compiled.hash}")
