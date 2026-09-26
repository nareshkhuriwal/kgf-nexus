# Deploying Nexus to a Portainer-managed Docker host

The release pipeline builds every image, pushes it to ACR digest-pinned, then
deploys to a Docker daemon on a VM. Portainer manages the resulting containers
as it manages anything else on that daemon.

    build → push to ACR → render compose → copy to host → docker compose up -d

Portainer is not in the path of the deploy. It does not need to be: the stack is
a normal compose project on that daemon, so Portainer lists it, shows its logs
and can restart it, without the pipeline going through Portainer's API. One
fewer credential, one fewer thing to be out of date.

## Why the compose file is generated

`deploy/docker-compose.yml` builds all twelve first-party services from source
and names no image. A daemon on a VM has no source tree, so it can only pull.

`deploy/render_compose.py` turns one into the other: every `build:` is replaced
by the digest-pinned image this release pushed. It is generated per release and
never committed — a second compose file in the repo would hold the same
topology, and the first thing that happens to a second copy of a topology is
that it stops matching the first.

Substitution is **by digest**, not by tag. A tag can be moved afterwards, so a
stack deployed from a tag can pull something the pipeline never built — and a
`docker compose up` after a host reboot would silently pick it up. A digest
names one image forever.

One image serves several services: `scheduler-gpu` runs the scheduler image with
different environment, and all three agents run the agent image. `SERVICE_TO_IMAGE`
in the script is that mapping.

## What you have to create

### 1. An SSH service connection

Azure DevOps → Project settings → Service connections → **SSH**.

| Field | Value |
|---|---|
| Name | `kgf-nexus-docker-host` (or set `dockerHostSshConnection`) |
| Host name | the VM |
| Port | 22 |
| Private key | a key whose user can run `docker` |

That user must be in the `docker` group. The deploy runs `docker login`,
`docker compose pull` and `docker compose up -d` as that user.

### 2. An Azure Resource Manager connection

Used once per deploy to mint a short-lived ACR token — so the VM holds **no
stored registry credential**. It needs `AcrPull` on the registry.

Set `dockerHostAzureSubscription` to its name.

### 3. `.env` on the host

    /opt/kgf-nexus/.env        # or wherever dockerHostStackDir points

```sh
NEXUS_WORKER_API_KEY=nxs_worker_...
RESULT_STORE_ENDPOINT=http://minio:9000
RESULT_STORE_BUCKET=nexus-results
RESULT_STORE_ACCESS_KEY=...
RESULT_STORE_SECRET_KEY=...
```

These five have no defaults in the compose file. **The pipeline does not write
this file and never will** — they are the host's secrets, not the pipeline's,
and a pipeline that writes them puts them in a log the moment something goes
wrong. The deploy checks for the file and for each variable, and fails before
touching the stack if any is missing, rather than starting a partial stack.

Get `NEXUS_WORKER_API_KEY` from `make seed`. Only its SHA-256 is stored, so it
cannot be recovered later — record it when it is printed.

Everything else in the compose file has a default, including every published
port (`NEXUS_API_PORT`, `NEXUS_UI_PORT`, …). Override those in the same file if
the host already uses them.

> The stack's database is the Docker host's own PostgreSQL by default
> (`NEXUS_HOST_DATABASE_URL`; the rendered file keeps that default). Set it to
> the database the host should use, or render with
> `deploy/docker-compose.bundled-postgres.yml` to opt in to a Postgres
> container.

> The credentials in `deploy/docker-compose.yml` for Postgres and MinIO are
> marked **DEV ONLY** and are deliberately obvious. They are still there in the
> rendered file. A host reachable by anyone else needs them overridden.

### 4. Turn the stage on

In `.azure-pipelines/kgf-nexus-release.yml`:

```yaml
  - name: deployToDockerHost
    default: true        # was false
```

Optionally create an Azure DevOps **environment** named
`kgf-nexus-docker-host` and attach an approval check, so a release waits for a
human before touching the host.

## What the deploy does

1. **Render** — refuses a tag reference, and refuses a service whose image is
   absent from the digest map, so a bad map fails on the agent.
2. **Mint an ACR token** — `az acr login --expose-token`, masked, short-lived.
3. **Copy** the compose file to `dockerHostStackDir`. The `.env` beside it is
   never touched.
4. **Pull, then start.** Pulling is its own step: `up -d` would pull too, but a
   registry failure there leaves some services replaced and others not. Pulling
   everything first keeps a failed deploy on the *old* images rather than a
   mixture.
5. **Verify.** `up -d` returns when containers are *created*, not when they are
   serving, so the deploy waits up to 180s for every service to be running and
   healthy, and dumps the logs of any that are not. `migrate` and `minio-init`
   are one-shot and exit 0 by design, so they are excluded from that check — but
   `migrate`'s exit code is asserted separately, because a half-applied schema
   with every service happily running against it is exactly the failure the
   health check cannot see.

## Afterwards, in Portainer

The stack appears under its project name (`nexus` by default) with all fifteen
containers. Portainer can show logs, restart and stop them.

**Do not redeploy from Portainer's UI.** It would re-run the compose file
currently on disk, which is right, but any edit made in the UI is overwritten by
the next release — the pipeline owns that file. Change the topology in
`deploy/docker-compose.yml` and release.

## Rolling back

Every release leaves its digest map published as the `image-digests` artifact.
To go back, download the previous release's artifact and render against it:

```sh
python3 deploy/render_compose.py --digests digests.json --out dc.yml
scp dc.yml host:/opt/kgf-nexus/docker-compose.deploy.yml
ssh host 'cd /opt/kgf-nexus && docker compose -p nexus -f docker-compose.deploy.yml up -d'
```

The images are still in ACR and still pinned, so this returns the host to
exactly what that release ran.

Migrations do **not** roll back with it. `services/migrations/*.down.sql` exists
but is a break-glass operation an operator runs deliberately — see the header of
`deploy/migrate.sh`.

## TLS certificates

The deploy renders the base compose file only, so mTLS on this host is a one-time
host-side setup: leaves issued with `deploy/nexus-certs.sh` on a machine that is
**not** this host (the CA key never comes here), installed with `nexus-certs.sh
install --owner 10001:10001`, and renewed monthly without restarting anything.
The procedure is in [docs/OPERATIONS.md §9.5](../../docs/OPERATIONS.md).
