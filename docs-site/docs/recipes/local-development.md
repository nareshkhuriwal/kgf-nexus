# Local development and replay

Three loops, from fastest to most realistic.

## 1. Unit-test task functions

Handlers are plain functions:

```bash
pytest                          # the templates ship tests/ for every task
```

## 2. Run the workflow on your machine

```bash
nexusctl run --local dag.py --param source=data/orders.csv
nexusctl run --local compiled.yaml --param source=demo --backoff-scale 0 -o json
python -m nexus.local compiled.yaml --param source=demo     # the same engine
```

```python
from nexus import local
result = local.run(daily_etl, {"source": "s3://in"})
result.state, result.output("load"), print(result.summary())
```

The local runner decides what runs next **exactly as the DAG engine does**:
trigger rules, `SKIPPED`/`UPSTREAM_FAILED` propagation, `fail_fast`, map
expansion and fan-in with their limits, retries with backoff, timeouts, and
memoization within the run. Shared fixtures keep it in step with the Go engine
([ADR-0022](../design/adr/0022-local-runner-and-replay.md)). Shell tasks get
the agent's `NEXUS_*` environment. Python handlers run in a subprocess through
`python -m nexus.worker`, or in-process with `--isolation inprocess` when you
want a debugger.

**Container tasks.** `docker` and `kubernetes` tasks are refused unless you
pass `--host-executor`, which runs their command **on this host**, from the
directory you started in and with that directory on `PYTHONPATH`, standing in
for the image. For an `nexusctl init` project:

```bash
nexusctl run --local dag.py --host-executor --param source=data/orders.csv --param target_db=warehouse.db
```

It is not the platform: there are no leases, heartbeats, queues, priorities,
resource reservations or GPU placement, and the cache does not carry across
runs. Results, logs, `events.jsonl` and `run.json` are written to
`.nexus-local/runs/<id>/`.

## 3. Iterate against a stack

```bash
nexusctl workflow submit dag.py --watch            # resubmit on every save
nexusctl workflow submit dag.py --watch --dry-run  # validate on every save
```

A save that does not change the compiled spec, such as a comment, is reported
and not re-sent, so no version is minted.

## Replay a failed attempt

When an attempt fails on the cluster, run it again on your machine **with the
exact inputs it had**:

```bash
nexusctl run get <run-id>                       # the LAST ATTEMPT column
nexusctl replay <attempt-id>                    # downloads replay-<id>/assignment.json
nexusctl replay <attempt-id> --local            # and re-runs it here
nexusctl replay <attempt-id> --local --module mypkg.tasks --input big_input=/tmp/copy.bin
```

`replay` fetches `GET .../runs/{run}/attempts/{id}/assignment`, which holds the
TaskSpec and the resolved inputs. It never includes the lease token or a
secret value, and it needs `tasks.results.view`. With `--local`, the attempt
runs with its original inputs and its **original idempotency key**, so an
idempotent effect that already happened is recognized as such. Blob inputs
stay references; supply their bytes with `--input NAME=PATH`.

!!! warning "Replay performs real effects"
    A replayed handler runs your code for real. If it writes to production
    systems, point it at a sandbox, or rely on its idempotency.
