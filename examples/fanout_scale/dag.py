# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""fanout_scale -- hundreds of tiny tasks, with an exact answer to check.

    numbers --> square (x count, created at run time) --> verify

  * numbers  emits [1, 2, ..., count] as a JSON array.
  * square   is a MAP template: one child per number, each returning n*n.
  * verify   sums every child's result and compares it with the closed form
             count(count+1)(2count+1)/6. A lost, duplicated or wrong result
             changes the sum, so the run FAILS instead of looking fine.

What it exercises: dispatch throughput under a burst of hundreds of queued
attempts (the windowed claim, ADR-0018), dynamic expansion near real sizes,
and a fan-in over hundreds of results. Tasks request little so many run at once.

    make submit-example EXAMPLE=fanout_scale PARAMS="count=500"
"""

from __future__ import annotations

from nexus import Resources, Runtime, TaskContext, Timeouts, task, workflow

_TINY = Resources(cpu="50m", memory="32Mi")
_READ = 'v() { tr -d \'"\\n \' < "$NEXUS_INPUT_DIR/$1"; }; '


@task(
    task_ref="scale.numbers",
    resources=_TINY,
    timeouts=Timeouts(start_to_close="5m"),
    inputs={"count": {"type": "json", "required": True}},
    outputs={"numbers": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + "awk -v n=\"$(v count)\" 'BEGIN { printf \"[\"; for (i = 1; i <= n; i++) printf \"%s%d\", (i > 1 ? \",\" : \"\"), i; printf \"]\" }' "
            '> "$NEXUS_OUTPUT_DIR/numbers"'
        ],
    ),
)
def numbers(ctx: TaskContext, count: int) -> list:
    """[1..count]."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="scale.square",
    resources=_TINY,
    timeouts=Timeouts(start_to_close="5m"),
    inputs={"n": {"type": "json", "required": True}},
    outputs={"square": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[_READ + 'n=$(v n); echo $((n * n)) > "$NEXUS_OUTPUT_DIR/square"'],
    ),
)
def square(ctx: TaskContext, n: int) -> int:
    """n*n."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="scale.verify",
    resources=_TINY,
    timeouts=Timeouts(start_to_close="5m"),
    inputs={"squares": {"type": "json", "required": True}, "count": {"type": "json", "required": True}},
    outputs={"result": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + "tr -d '[] \\n' < \"$NEXUS_INPUT_DIR/squares\" | tr ',' '\\n' | awk -v n=\"$(v count)\" '\n"
            '$0 == "null" { missing++; next }\n'
            "NF { sum += $0; got++ }\n"
            "END {\n"
            "  want = n * (n + 1) * (2 * n + 1) / 6;\n"
            '  printf "{\\"count\\":%d,\\"results\\":%d,\\"missing\\":%d,\\"sum\\":%d,\\"expected\\":%d,\\"ok\\":%s}\\n", n, got, missing, sum, want, (sum == want && got == n) ? "true" : "false";\n'
            '  if (sum != want || got != n) { printf "fan-in mismatch: %d results summing to %d, expected %d summing to %d\\n", got, sum, n, want > "/dev/stderr"; exit 1 }\n'
            "}' > \"$NEXUS_OUTPUT_DIR/result\" && cat \"$NEXUS_OUTPUT_DIR/result\""
        ],
    ),
)
def verify(ctx: TaskContext, squares: list, count: int) -> dict:
    """Fan-in: the sum must equal the closed form exactly."""
    raise NotImplementedError("runs as a shell workload")


@workflow(
    name="fanout-scale",
    description="Fan out to hundreds of tiny tasks and check the fan-in against an exact closed form.",
    params={"count": {"type": "int", "required": False, "default": 500}},
    run_timeout="1h",
)
def fanout_scale(count: int):
    nums = numbers(count=count)
    squares = square.map(nums["numbers"], item="n", max_fanout=2000)
    return verify(squares=squares, count=count)


if __name__ == "__main__":
    print(fanout_scale.compile().to_yaml())
