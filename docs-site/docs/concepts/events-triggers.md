# Events, triggers, sensors and lineage

Four features react to what happens in and around a run. All of them are
driven from the transactional outbox in PostgreSQL, so they need no message
bus and no leader election: every loop is safe on any number of replicas, and
every output is keyed so a redelivery does not repeat it
([ADR-0028](../design/adr/0028-event-triggers-notifications-sensors-lineage.md)).

## Notifications

A **channel** is a destination -- email, Slack (incoming webhook or bot),
Microsoft Teams, PagerDuty or a signed generic webhook -- and a **rule** says
which events reach which channels: `run.failed`, `run.succeeded`,
`run.timed_out`, `run.sla_exceeded` or `attempt.lost`, filtered by
workflow and label. Deliveries are at least once, retried with
backoff, and dead-lettered after their budget, from where they can be
redriven. Channel secrets (webhook URLs, keys) are sealed with the platform's
secret key and never returned.

```sh
nexusctl notify channel-create oncall --kind slack_webhook --secret-env SLACK_WEBHOOK_URL
nexusctl notify rule-create failures --event run.failed --channel oncall
nexusctl notify channel-test oncall
```

## Webhook triggers

A **trigger** is a signed inbound URL that starts a run of a workflow. The
sender signs `timestamp.body` with the trigger's secret (HMAC-SHA256), and a
JSONPath mapping turns the payload into run parameters and an idempotency key,
so a retried delivery starts one run. The secret is shown once and can be
rotated with a grace period for the old one.

```sh
nexusctl trigger create on-push --workflow build \
    --map ref='$.payload.ref' --idempotency-key-path '$.payload.after'
TRIGGER_SECRET=... nexusctl trigger fire on-push --secret-env TRIGGER_SECRET \
    --data '{"ref":"main","after":"abc123"}'
```

The hook itself (`POST /v1/hooks/{ns}/{trigger}`) needs no API key: the
signature is its credential ([REST reference](../reference/api/index.md)).

## Sensors

A **sensor** starts runs when something appears: an object under a prefix in
an S3-compatible store (`--uri s3://bucket/prefix/ --pattern '*.parquet'`),
or the success of an upstream workflow (`--upstream WORKFLOW`). The
workflow-manager polls due sensors on a lease, keeps a cursor, and records
each observation once, so a new object starts one run however many replicas
see it. A sensor watching the platform's own result store uses the same
credentials, including keyless cloud identities.

## Asset lineage

Tasks may declare the data **assets** they consume and produce
(`assets: {consumes: [...], produces: [...]}` in the spec, or on the SDK's
`@task`). When an attempt succeeds, the event-service records a
materialization and the edges between producers and consumers, and
`nexusctl asset graph` or the console's lineage page shows what feeds what.

Configuration, key rotation, tuning and rollback are in
[runbook §27](../operations/runbook.md#27-notifications-webhook-triggers-sensors-and-asset-lineage).
