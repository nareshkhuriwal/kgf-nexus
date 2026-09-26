# Copyright 2026 Khuriwal Group. SPDX-License-Identifier: Apache-2.0
# Hand-written test input for `nexusctl import prefect` (original to this repository). Futures, .map with unmapped and wait_for, retries, caching,
# a subflow, a runtime branch, and serve(cron=...).
from datetime import timedelta

from prefect import flow, task, unmapped
from prefect.tasks import exponential_backoff, task_input_hash


@task(retries=3, retry_delay_seconds=[1, 10, 100], timeout_seconds=300)
def list_shards(source: str) -> list[str]:
    return [f"{source}/{i}" for i in range(4)]


@task(retries=2, retry_delay_seconds=exponential_backoff(backoff_factor=5),
      cache_key_fn=task_input_hash, cache_expiration=timedelta(hours=1), tags=["io"])
def process(shard: str, mode: str) -> int:
    return len(shard)


@task
def warm_up() -> None:
    pass


@task
def combine(counts: list[int]) -> int:
    return sum(counts)


@task(cache_policy=None)
def notify(total: int, channel: str = "#data") -> None:
    print(total, channel)


@flow(name="Nightly ETL", timeout_seconds=3600, retries=1)
def nightly_etl(source: str = "s3://bucket/in", mode: str = "full", limit: int = 10):
    shards = list_shards.submit(source)
    ready = warm_up.submit()
    counts = process.map(shards, unmapped(mode), wait_for=[ready])
    total = combine(counts)
    squares = process.map(["x", "y"], mode)
    if total.result() > limit:
        notify(total)
    report(total)


@flow
def report(total: int):
    notify(total, channel="#reports")


if __name__ == "__main__":
    nightly_etl.serve(name="nightly", cron="0 3 * * *", parameters={"mode": "incremental"})
