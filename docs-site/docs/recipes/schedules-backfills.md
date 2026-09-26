# Schedules and backfills

Schedules are separate API objects that point at a registered workflow by
name. They are not part of the spec, so changing a schedule never mints a new
spec version.

## Preview first

Preview never creates anything:

```console
$ nexusctl schedule preview "30 2 * * *" --timezone Europe/London --count 3
30 2 * * * (Europe/London) fires next at:
  2026-09-27T02:30:00+01:00
  2026-09-28T02:30:00+01:00
  2026-09-29T02:30:00+01:00
```

## Create

```bash
nexusctl schedule create orders-nightly --workflow orders-etl \
    --cron "30 2 * * *" --timezone Europe/London \
    --param source=https://example.com/orders.csv \
    --overlap skip --catchup 1
```

| Flag | Meaning |
| --- | --- |
| `--cron` | five-field cron, evaluated **in `--timezone`** (IANA; default UTC) |
| `--overlap skip\|allow\|queue_one` | what happens when a tick is due while the previous run is still going: skip the tick (default), start anyway, or queue at most one |
| `--catchup N` | after downtime, fire up to N missed ticks (default 0: only the tick that just came due) |
| `--spec-version N` | pin a version (default: the latest at each fire) |
| `--param K=V` | parameters for every run |
| `--disabled` | create it disabled |

`schedule update` changes only the flags you pass. `enable`/`disable` pause a
schedule without losing it, and `get <name> --count N` shows the next N fire
times. The SDK has the same operations (`NexusClient.create_schedule`, …),
and the console has a Schedules page.

## Time zones and daylight saving

A schedule means **local wall-clock time** in its zone: `0 2 * * *` in
Europe/London is 02:00 local all year, and it does not drift an hour at each
transition. The two ambiguous cases are decided explicitly
(`internal/cron`):

- **Spring gap** (the wall time does not exist): the tick fires **once**, at
  the first instant that exists afterwards. A 01:30 nightly job runs an hour
  late that day, rather than not at all.
- **Autumn fold** (the wall time happens twice): the tick fires **once**, on
  the **first** occurrence.

Use UTC when you need fixed intervals across transitions.

## Backfill

```bash
nexusctl backfill start orders-nightly --from 2026-09-01 --to 2026-09-08 --max-active 2
nexusctl backfill get <id>            # cursor and runs created
nexusctl backfill pause|resume|cancel <id>
```

- A backfill creates one run for every tick the cron **would have fired** in
  the half-open interval `[from, to)`, oldest first, in the schedule's zone.
- Each tick is claimed against the schedule, so a tick that already produced
  a run, scheduled or backfilled, is not produced again.
- `--max-active` bounds runs in flight (default 1, which is serial; at most
  50). `--max-runs` is a hard cap (default 1000).
- A backfill is durable. Its cursor survives control-plane restarts, and it
  is addressed by its own id.

Runs created by a schedule or a backfill are marked with their
`trigger_kind`, and the database records the tick as the run's `logical_date`.
The logical date is **not** passed to tasks today: it is not in the run
document the API returns, and not in a task's environment. A task that
processes "the data for tick T" has to derive T itself, for example from a
parameter.

!!! note "Migrating from Airflow"
    Airflow's `catchup=True` and `execution_date` have no direct equivalent.
    `nexusctl import airflow` turns `catchup` into a suggested `backfill start`
    command, and templated dates (`{{ ds }}`) are reported as not translated. See
    the [Airflow guide](../migrate/airflow.md).
