# Dispatch benchmark, run 1 (interrupted, host heavily loaded)

`make bench-dispatch` with its defaults: pull and push, backlogs of 1,000,
10,000 and 100,000, and 1, 2, 4, 8 and 16 replicas. 64 workers. Each cell ran
3,000 claims, or backlog/2 if that is smaller, capped at 90 s. The code was
`82b9ec8`. The database was `nexus-w3l-bench-pg` (`postgres:16-alpine`,
`shared_buffers=512MB`, `max_connections=300`).

- **Start:** 2026-09-26T03:22:27Z. The 1-minute host load average was 10.6 at
  the start and rose to 29–49 during the run. Other agents' container
  workloads were sharing the laptop (39 running containers at the start).
- **End:** about 03:53Z, when Docker Desktop restarted and terminated the
  database connection while seeding the push/100000/16 cell:
  `seed nodes: FATAL: terminating connection due to administrator command (SQLSTATE 57P01)`.
- **Raw log:** it was in a temporary directory that a host reboot erased. The
  rows below were copied from the test's own table output before that.

**Do not use this run to size anything.** It is kept because it is a
measurement that was taken. [BENCHMARKS.md](../BENCHMARKS.md) explains how it
compares with runs on a quieter host.

| mode | backlog | replicas | dispatched | claims/s | p50 | p99 | max | empty | errors |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| pull | 1000 | 1 | 500 | 100 | 8.29ms | 30.34ms | 99.3ms | 0 | 0 |
| pull | 1000 | 2 | 501 | 188 | 8.05ms | 45.29ms | 106.3ms | 0 | 0 |
| pull | 1000 | 4 | 503 | 334 | 10.57ms | 27.86ms | 75.2ms | 0 | 0 |
| pull | 1000 | 8 | 507 | 565 | 11.48ms | 52.09ms | 57.3ms | 0 | 0 |
| pull | 1000 | 16 | 515 | 365 | 25.74ms | 242.76ms | 249.9ms | 0 | 0 |
| pull | 10000 | 1 | 3000 | 58 | 6.39ms | 183.57ms | 804ms | 0 | 0 |
| pull | 10000 | 2 | 3001 | 236 | 6.5ms | 43.82ms | 177.2ms | 0 | 0 |
| pull | 10000 | 4 | 3003 | 544 | 6.64ms | 17.88ms | 63.9ms | 0 | 0 |
| pull | 10000 | 8 | 3007 | 598 | 11.22ms | 43.97ms | 172.2ms | 0 | 0 |
| pull | 10000 | 16 | 3015 | 153 | 41.79ms | 1.03134s | 2.3589s | 0 | 0 |
| pull | 100000 | 1 | 3000 | 103 | 6.42ms | 81.36ms | 370.8ms | 0 | 0 |
| pull | 100000 | 2 | 3001 | 105 | 8.87ms | 174.66ms | 1.4713s | 0 | 0 |
| pull | 100000 | 4 | 3003 | 490 | 7.28ms | 24.32ms | 59.9ms | 0 | 0 |
| pull | 100000 | 8 | 3007 | 513 | 12.39ms | 51.07ms | 321ms | 0 | 0 |
| pull | 100000 | 16 | 3015 | 418 | 23.86ms | 240.5ms | 470.1ms | 0 | 0 |
| push | 1000 | 1 | 500 | 81 | 10.29ms | 53.97ms | 87.6ms | 0 | 0 |
| push | 1000 | 2 | 501 | 100 | 15.26ms | 103.73ms | 184.1ms | 0 | 0 |
| push | 1000 | 4 | 503 | 147 | 22.55ms | 65.82ms | 102.2ms | 26 | 0 |
| push | 1000 | 8 | 506 | 111 | 37.79ms | 137.92ms | 185.9ms | 173 | 0 |
| push | 1000 | 16 | 509 | 20 | 156.39ms | 1.57084s | 3.581s | 548 | 0 |
| push | 10000 | 1 | 1559 | 17 | 44.5ms | 263.85ms | 733.1ms | 0 | 0 |
| push | 10000 | 2 | 986 | 11 | 82.59ms | 1.53434s | 3.2427s | 7 | 0 |
| push | 10000 | 4 | 1795 | 20 | 76.35ms | 1.2036s | 10.1804s | 49 | 0 |
| push | 10000 | 8 | 2567 | 28 | 105.68ms | 1.59688s | 6.2589s | 209 | 0 |
| push | 10000 | 16 | 147 | 2 | 4.16052s | 15.79642s | 16.8618s | 72 | 0 |
| push | 100000 | 1 | 37 | 0 | 2.08838s | 5.816s | 6.7646s | 0 | 0 |
| push | 100000 | 2 | 30 | 0 | 4.37776s | 23.38855s | 23.6987s | 0 | 0 |
| push | 100000 | 4 | 49 | 0 | 4.54047s | 22.55683s | 42.0773s | 2 | 0 |
| push | 100000 | 8 | 82 | 1 | 7.49179s | 30.27932s | 37.6716s | 0 | 0 |
