# Vendored Prefect examples (test data)

These files are **unmodified, byte-for-byte copies** of examples from the
Prefect repository. They are test inputs for `nexusctl import prefect` only:
they are not compiled into, linked into or shipped with any Nexus binary, and
nothing in Nexus imports or executes them (the importer parses them with `ast`).

- Source: https://github.com/PrefectHQ/prefect
- Tag: `3.8.6` (commit `33de4078a0f1d5efb6e4170ccb8fcc482e073155`)
- Downloaded: 2026-09-25 from `https://raw.githubusercontent.com/PrefectHQ/prefect/3.8.6/examples/<file>`
- License: Apache License 2.0 (checked against the repository's `LICENSE` at that tag;
  the repository has no NOTICE file). Copyright Prefect Technologies, Inc.

| File (examples/) | sha256 |
|---|---|
| hello_world.py | c4a3ef9713e77f56a6be40ec66e6770a7adf57ba877eb9d6799ad88e3d5694db |
| per_worker_task_concurrency.py | bd35d2db6452822d9c7da9267c976d512ee66dff8abc518bdc1babe2e103cf1f |
| run_api_sourced_etl.py | c58ac0d2703cdc1202c07a6095ee25394c91c44eea6b9d07aaff9fbfec13e0ee |
| simple_web_scraper.py | e737e62a2bb839a8c7f216aed0c6b3ef839467af8a855e85a05ee4608d6716ec |

Prefect's upstream examples use few orchestration features (no `.map`,
`wait_for` or `serve(cron=...)`); those are covered by the hand-written cases in
`../../cases/prefect/`, which are original to this repository.
