# Quickstart

In about ten minutes, starting from a checkout, you will:

- start a local Nexus stack;
- generate an ETL project and test it;
- submit the project and run it on the stack;
- watch the run in the console and read its logs;
- run the same workflow on your laptop without a cluster.

Every command and output on this page comes from a real run: a stack started
on a clean database, on an Apple-silicon laptop with Docker Desktop, on
2026-09-26. IDs, keys and timings will differ for you.

## Before you start

- Docker with the Compose v2 plugin (2.24 or later). Docker Desktop is fine.
- Go 1.25 or later, to build `nexusctl`.
- Python 3.11 or later, for the SDK and the project's tests.
- About 4 GB of free memory, and ports 3000, 8090, 9091, 55433, 9010/9011,
  8025, 4222 and 19100–19122 free (see [other ports](#other-ports)).

```bash
git clone https://github.com/nareshkhuriwal/kgf-nexus.git && cd kgf-nexus
go build -o ./nexusctl ./cli/cmd/nexusctl
export PATH="$PWD:$PATH"
```

## 1. Start the stack

```console
$ nexusctl up
Starting docker-compose.yml (project nexus) without its 3 agent(s) until a worker key exists...
Waiting up to 5m0s for 15 services to become healthy...
Seeding: registering tenant "Local Quick Start" with administrator admin@nexus.localhost through http://127.0.0.1:8090 ...
Seeding: confirming the administrator's address from the stack's Mailpit (http://127.0.0.1:8025) ...
Nexus is up and seeded (compose project nexus).
  Console      http://localhost:3000
  API          http://127.0.0.1:8090
  Tenant ID    fa8ba03f-f6d8-4276-ba1b-56ebc9be3e12
  Sign in as   admin@nexus.localhost
  Password     XfDUNAUGoFTBh0NCWOe0s9rn
  Admin key    nxtok_…                     (expires 2026-12-25)
These are shown ONCE. The password is not stored anywhere; save it now.
The admin key is also saved in ~/.nexus/config.yaml as context "nexus"; it is now the current context, so nexusctl commands use it by default.
Issuing the tenant's NATS agent identity (nats-init --tenant fa8ba03f-…) ...
Starting the agents with the new worker key ...
3 worker(s) registered in tenant fa8ba03f-f6d8-4276-ba1b-56ebc9be3e12. The stack is ready.
```

The first run builds the images, which took about 4 minutes here. Later runs
take seconds. `up` registers a tenant through the product's own API, verifies
the administrator's address from the bundled Mailpit, mints an admin API key
and starts three agents. **Save the password now**: it is printed once and
stored nowhere. `nexusctl status` shows the health of each service.

## 2. Create a project

```console
$ nexusctl init etl orders-etl
Created etl project "orders-etl" in orders-etl (namespace fa8ba03f-f6d8-4276-ba1b-56ebc9be3e12)

  .dockerignore
  .gitignore
  Dockerfile
  README.md
  conftest.py
  dag.py
  data/orders.csv
  requirements.txt
  tests/test_tasks.py
```

The namespace is your tenant's, taken from the context `up` wrote.

```bash
cd orders-etl
python3 -m venv .venv && . .venv/bin/activate
pip install -e ../sdk/python -r requirements.txt pytest    # the SDK from your checkout
pytest -q
```

```console
........                                                                 [100%]
8 passed in 0.09s
```

!!! warning "Install the SDK from the checkout"
    `nexus-sdk` on PyPI is an unrelated project. `pip install nexus-sdk` from
    a public index installs the wrong package (CONTRACT-FINDINGS F144).

## 3. See exactly what the server will get

```console
$ nexusctl workflow compile dag.py --show
Workflow:     orders-etl
Namespace:    fa8ba03f-f6d8-4276-ba1b-56ebc9be3e12
Spec hash:    98a4becdf3fcf7f2b3ecf073bc0f850e3db17bb4fdf06217874b1d717095cf79
Source:       dag.py
Tasks:        4
Edges:        3

ID          TASK_REF               VER   QUEUE     CPU    MEMORY   GPU   IDEMPOTENCY   RETRIES
extract     orders-etl.extract     1     default   500m   1Gi      -     required      4
load        orders-etl.load        1     default   500m   1Gi      -     required      5
transform   orders-etl.transform   1     default   1      2Gi      -     required      2
validate    orders-etl.validate    1     default   500m   1Gi      -     required      1

FROM        TO          OUTPUT -> INPUT
extract     validate    result -> extracted
transform   load        result -> transformed
validate    transform   result -> validated

--- canonical WorkflowSpec (what the server parses) ---
…
```

Compiling runs **your** Python on **your** machine and contacts nothing. The
control plane only ever receives this document.

## 4. Build the task image and submit

Every task runs in the project's image through the docker executor:

```bash
docker build --build-context nexus-sdk=../sdk/python -t orders-etl:0.1.0 .
```

```console
$ nexusctl workflow submit dag.py
Submitted orders-etl
  local spec hash:  98a4becdf3fcf7f2b3ecf073bc0f850e3db17bb4fdf06217874b1d717095cf79
  server spec hash: 98a4becdf3fcf7f2b3ecf073bc0f850e3db17bb4fdf06217874b1d717095cf79
  version:          1

Start a run with:
  nexusctl run create orders-etl
```

The agents run tasks as sibling containers on your Docker daemon, so the
locally built image is found without pushing it anywhere.

## 5. Run it

The project ships a 31-row sample with one bad row, baked into the image at
`/app/data/orders.csv`:

```console
$ nexusctl run create orders-etl --param source=/app/data/orders.csv \
      --param target_db=/tmp/warehouse.db --wait
Created run 06839e0e-114b-4242-846f-c1962eebdbf6 (running)
  RUNNING
  SUCCEEDED
```

```console
$ nexusctl run get 06839e0e-114b-4242-846f-c1962eebdbf6
Run:        06839e0e-114b-4242-846f-c1962eebdbf6
Workflow:   orders-etl
State:      succeeded
Version:    1
…
Params:     {"source":"/app/data/orders.csv","target_db":"/tmp/warehouse.db"}

NODE        STATE       ATTEMPTS   LAST ATTEMPT                           WORKER         DURATION   ERROR
extract     succeeded   1/4        84d55125-87b1-4481-9ece-b321cac37840   f5dde18a223e   1s         -
load        succeeded   1/5        1497467a-01dc-4673-a8f6-50f2e655f3b2   f5dde18a223e   808ms      -
transform   succeeded   1/2        3e59fa10-fd2b-47af-ad6b-e1e4b453a01f   f5dde18a223e   840ms      -
validate    succeeded   1/1        c04279cd-2e9e-4b44-83b4-af9eed8a5c11   f5dde18a223e   1s         -
```

## 6. Watch it in the console

Open <http://localhost:3000>, sign in as `admin@nexus.localhost` with the
password from step 1, and open **Runs**. The run page shows the DAG, each
node's attempts, the timeline and the logs, and it updates live while a run is
in progress. Start another run with step 5 and watch it move.

## 7. Read the logs and events

```console
$ nexusctl logs c04279cd-2e9e-4b44-83b4-af9eed8a5c11
{"ts": "2026-09-26T06:54:02Z", "level": "INFO", "msg": "task starting", "node_id": "validate", …}
{"ts": "2026-09-26T06:54:02Z", "level": "INFO", "msg": "validated", …, "valid": 30, "rejected": 1, "ratio": 0.0323}
{"ts": "2026-09-26T06:54:02Z", "level": "INFO", "msg": "task succeeded", …, "outputs": ["result"]}
```

`validate` rejected the one bad row (an impossible date) and stayed under the
5% limit, so the run succeeded. `nexusctl logs <id> --follow` tails a running
attempt.

```console
$ nexusctl events 06839e0e-114b-4242-846f-c1962eebdbf6
SEQ   TIME           TYPE                NODE        DETAIL
1     06:53:59.273   RunStateChanged     -           {"from":"PENDING","to":"RUNNING","trigger":"start"}
3     06:53:59.334   AttemptDispatched   extract     {"attempt_number":1,"best_fit_score":0,…
4     06:53:59.357   AttemptStarted      extract     {"started_at":"2026-09-26T06:53:59.357817043Z"}
5     06:54:01.240   AttemptSucceeded    extract     {"attempt_number":1,…,"state":"SUCCEEDED"}
…
15    06:54:04.634   RunStateChanged     -           {"from":"RUNNING","to":"SUCCEEDED","trigger":"all_nodes_terminal"}
```

## 8. Run it without a cluster

```console
$ nexusctl run --local dag.py --host-executor --param source=data/orders.csv --param target_db=warehouse.db
[local] run 01229c71-a154-4136-816f-e2e3392d5bd3 of orders-etl in …/orders-etl/.nexus-local/runs/01229c71-…
NODE       STATE      ATTEMPTS  TIME   DETAIL
extract    SUCCEEDED  1         0.11s
validate   SUCCEEDED  1         0.11s
transform  SUCCEEDED  1         0.10s
load       SUCCEEDED  1         0.12s

run 01229c71-a154-4136-816f-e2e3392d5bd3 (orders-etl): SUCCEEDED in 0.45s
```

The local runner makes the same scheduling decisions as the platform's DAG
engine. `--host-executor` runs the docker tasks' commands on your machine
instead of in the image. See [local development](recipes/local-development.md).

## Clean up

```bash
nexusctl down             # stop; data is kept, `up` resumes with the same tenant
nexusctl down --volumes   # also DELETE the database, results and identities
```

## Other ports

If a default port is taken, move it:
`nexusctl up --api-port 18090 --ui-port 13000 …`. Every published port has
a flag, and `nexusctl help up` lists them. The service metrics ports
(127.0.0.1:191xx) have no flag. To run beside another stack, pass an override
file with `-f` that resets them. The CLI README
[shows how](repo:cli/README.md). This page was verified exactly that way: a
stack under its own compose project name, on ports 55851–55858.

## Next

- [Concepts](concepts/index.md): what a spec, task, attempt and lease are.
- [Recipes](recipes/index.md): ML training, batch inference, file fan-out, schedules.
- [Migrating](migrate/index.md) from Airflow, Prefect or Argo with `nexusctl import`.
