# ETL

```bash
nexusctl init etl orders-etl
```

```text
extract --> validate --> transform --> load
```

The template ([source](repo:cli/internal/templates/files/etl/dag.py)) is
the [quickstart](../quickstart.md) project. It reads a CSV (path, `file://`
or `http(s)://`), rejects bad rows with a reason each, normalizes and
aggregates the data, and upserts into SQLite. Swap `_connect` for PostgreSQL;
the `ON CONFLICT` SQL works unchanged.

## The policies, and why

| Task | Policy | Why |
| --- | --- | --- |
| `extract` | 4 attempts on transient, timeout and infrastructure errors | network reads fail transiently |
| `validate` | 1 attempt; fails the run above `max_reject_ratio` | deterministic: a retry would reject the same rows; loading a mostly broken file is worse than loading nothing |
| `transform` | memoized 7 days | pure: a re-run over the same file reuses the result |
| `load` | 5 attempts; **conditional write** keyed on `ctx.idempotency_key` | at-least-once delivery: a retry must be harmless |

## The load step is the one that matters

Nexus runs tasks [at least once](../concepts/retries-idempotency.md). A worker
can die after the database commits and before the platform hears about it.
`load` handles that in **one transaction**:

1. Look up `ctx.idempotency_key` in `load_batches`. If it is there, return:
   the effect already happened.
2. Upsert the rows by `order_id`, and recompute daily totals from the table.
3. Insert the key into `load_batches`.

The key is the same on every retry of the same logical load, because it
excludes the attempt number. So a retry either finds the batch or finds
nothing written. Upserting by business key also means a *new* run over an
overlapping file cannot double-count.

## Run it nightly

```bash
nexusctl schedule create orders-etl-nightly --workflow orders-etl \
    --cron "30 2 * * *" --timezone Europe/London \
    --param source=https://example.com/orders.csv
```

See [schedules and backfills](schedules-backfills.md) for overlap, catch-up
and backfilling history.

## Bigger examples

[`examples/csv_sales_pipeline`](repo:examples/csv_sales_pipeline/) is a
49-node DAG: 16 regions × (generate → validate → aggregate) → report, with
320k rows by default and MB-sized intermediate files passed through the result
store. It fails the run unless every row is either aggregated or rejected.
