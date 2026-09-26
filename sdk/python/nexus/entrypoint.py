# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Run one @task handler under the executor FILE contract.

    python -m nexus.entrypoint --module dag etl.extract

The worker agent's executors (subprocess, docker, kubernetes) hand a workload
its inputs and collect its outputs through files, not through a TaskAssignment
document (agent/internal/executor/executor.go):

    $NEXUS_INPUT_DIR/<input>     one file per resolved input (JSON for json/int/...)
    $NEXUS_OUTPUT_DIR/<output>   one file per DECLARED output, written by the task
    $NEXUS_RUN_ID, $NEXUS_NODE_ID, $NEXUS_ATTEMPT_ID, $NEXUS_IDEMPOTENCY_KEY,
    $NEXUS_DEADLINE_UNIX_MS      identity and deadline
    $NEXUS_TASK_API(_TOKEN)      the attempt's workload API, when the agent
                                 serves one (ADR-0032): heartbeats, usage,
                                 permits, checkpoints and the declared error class

`nexus.worker` speaks the assignment-document protocol instead. This module is
the bridge that lets a plain decorated Python function run as a container
command: it decodes the input files into the handler's keyword arguments,
builds a TaskContext from the environment, calls the handler, and writes each
declared output file.

Exit status is the signal the executor reads: 0 means every declared output was
written; anything else fails the attempt, and the platform's retry policy (not
this process) decides whether to try again. The traceback goes to stderr, which
the agent ships as the attempt's log.

Declared ``max_bytes`` bounds are ENFORCED here, before an output is written: a
bound the author declared so that submit-time validation could prove a fan-in
fits is only sound if an oversized value is actually refused, and the worker
agent does not check it when it collects outputs.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import traceback
from typing import Any, Mapping, Sequence

from ._taskapi import from_environment
from .context import FileResultStore, InMemoryResultStore, TaskContext, derive_idempotency_key
from .task import TaskDefinition
from .types import ValueSpec
from .worker import HandlerNotFound, declare_error, import_modules, resolve

__all__ = ["main", "run_from_environment", "OutputTooLarge", "EXIT_HANDLER_FAILED", "EXIT_SETUP_FAILED"]

# The handler raised, or an output violated its declaration.
EXIT_HANDLER_FAILED = 1
# The process could not even start the handler: a missing module, an unknown
# task_ref, an unreadable input. Distinct so a log reader sees at once that the
# code never ran.
EXIT_SETUP_FAILED = 2


class OutputTooLarge(ValueError):
    """An output exceeded the ``max_bytes`` its declaration promised."""


def _decode_input(raw: bytes) -> Any:
    """Inputs are staged as the bytes the upstream produced: JSON for every
    typed value, arbitrary bytes for a file_ref. Undecodable content is handed
    over as text (or bytes) rather than guessed at."""
    try:
        return json.loads(raw)
    except ValueError:
        try:
            return raw.decode("utf-8")
        except UnicodeDecodeError:
            return raw


def read_inputs(definition: TaskDefinition[Any], input_dir: str | None) -> dict[str, Any]:
    """Map staged input files onto the handler's parameters.

    Files the handler does not accept are ignored, mirroring nexus.worker: adding
    an input to a spec must not break a handler that has not been redeployed.
    """
    kwargs: dict[str, Any] = {}
    if not input_dir or not os.path.isdir(input_dir):
        return kwargs
    accepted = set(definition.parameter_names)
    for name in sorted(os.listdir(input_dir)):
        path = os.path.join(input_dir, name)
        if not os.path.isfile(path) or (accepted and name not in accepted):
            continue
        with open(path, "rb") as f:
            kwargs[name] = _decode_input(f.read())
    return kwargs


def _output_specs(definition: TaskDefinition[Any]) -> dict[str, ValueSpec]:
    declared = definition._decl.outputs  # noqa: SLF001 - same package
    if declared:
        return {k: ValueSpec.coerce(v) for k, v in declared.items()}
    return {name: ValueSpec(type="json") for name in definition.declared_outputs}


def write_outputs(definition: TaskDefinition[Any], value: Any, output_dir: str) -> list[str]:
    """Write one file per declared output; returns the names written.

    A task with one declared output writes its whole return value there. A task
    with several must return a mapping holding each of them, because guessing
    which part of a return value is which output is how data ends up in the
    wrong place.
    """
    specs = _output_specs(definition)
    if len(specs) == 1:
        (name,) = specs
        values = {name: value}
    else:
        if not isinstance(value, Mapping):
            raise TypeError(
                f"{definition.task_ref} declares outputs {sorted(specs)}, so it must return a "
                f"mapping with those keys; it returned {type(value).__name__}"
            )
        values = {k: value[k] for k in specs if k in value}

    encoded: dict[str, bytes] = {}
    for name, spec in sorted(specs.items()):
        if name not in values or values[name] is None:
            if spec.required and not spec.has_default:
                raise ValueError(f"{definition.task_ref} did not produce required output {name!r}")
            continue
        raw = json.dumps(values[name], sort_keys=True, separators=(",", ":")).encode("utf-8")
        if spec.max_bytes is not None and len(raw) > spec.max_bytes:
            raise OutputTooLarge(
                f"output {name!r} of {definition.task_ref} is {len(raw)} bytes, over its declared "
                f"max_bytes={spec.max_bytes}; return a smaller value or pass a reference to it"
            )
        encoded[name] = raw

    # Validate everything before writing anything: a half-written output set is
    # worse than none, because the executor collects whatever files exist.
    os.makedirs(output_dir, exist_ok=True)
    for name, raw in encoded.items():
        tmp = os.path.join(output_dir, f".{name}.tmp")
        with open(tmp, "wb") as f:
            f.write(raw)
        os.replace(tmp, os.path.join(output_dir, name))
    return sorted(encoded)


def context_from_environment(definition: TaskDefinition[Any], env: Mapping[str, str]) -> TaskContext:
    run_id = env.get("NEXUS_RUN_ID", "")
    node_id = env.get("NEXUS_NODE_ID", "")
    key = env.get("NEXUS_IDEMPOTENCY_KEY") or derive_idempotency_key(run_id, node_id, None, definition.version)
    work_dir = env.get("NEXUS_RESULT_DIR") or ""
    return TaskContext(
        run_id=run_id,
        node_id=node_id,
        attempt_id=env.get("NEXUS_ATTEMPT_ID", ""),
        attempt_number=int(env.get("NEXUS_ATTEMPT_NUMBER") or 1),
        task_ref=definition.task_ref,
        task_version=definition.version,
        idempotency_key=key,
        start_to_close_deadline_unix_ms=int(env.get("NEXUS_DEADLINE_UNIX_MS") or 0),
        worker_id=env.get("NEXUS_WORKER_ID", ""),
        store=FileResultStore(work_dir) if work_dir else InMemoryResultStore(),
        # The agent's per-attempt workload API (ADR-0032), when it exported one:
        # heartbeats, usage, permits and checkpoints go through it.
        _task_api=from_environment(env),
    )


def run_from_environment(
    task_ref: str, modules: Sequence[str], env: Mapping[str, str] | None = None
) -> int:
    """Execute one handler against the environment's file contract; return an exit code."""
    env = os.environ if env is None else env
    try:
        import_modules(modules)
        definition = resolve(task_ref)
    except HandlerNotFound as e:
        print(f"nexus.entrypoint: {e}", file=sys.stderr)
        return EXIT_SETUP_FAILED
    except Exception:
        print(f"nexus.entrypoint: failed to import {', '.join(modules)}:", file=sys.stderr)
        traceback.print_exc()
        return EXIT_SETUP_FAILED

    output_dir = env.get("NEXUS_OUTPUT_DIR")
    if not output_dir:
        print(
            "nexus.entrypoint: NEXUS_OUTPUT_DIR is not set. This command runs a task the way a "
            "Nexus worker does; to call a handler directly, import it and call it in a test.",
            file=sys.stderr,
        )
        return EXIT_SETUP_FAILED
    try:
        kwargs = read_inputs(definition, env.get("NEXUS_INPUT_DIR"))
    except OSError as e:
        print(f"nexus.entrypoint: cannot read inputs: {e}", file=sys.stderr)
        return EXIT_SETUP_FAILED

    ctx = context_from_environment(definition, env)
    ctx.log.info("task starting", inputs=sorted(kwargs))
    try:
        value = definition.fn(ctx, **kwargs)
        written = write_outputs(definition, value, output_dir)
    except Exception as e:
        ctx.log.error("task failed", error_type=type(e).__name__, error=str(e))
        traceback.print_exc()
        # Declare the class (rate_limited, validation, ...) before exiting
        # non-zero; the agent applies it to the failure it reports.
        declare_error(ctx, e)
        ctx._finish()  # noqa: SLF001 - same package
        return EXIT_HANDLER_FAILED
    ctx._finish()  # noqa: SLF001 - same package
    ctx.log.info("task succeeded", outputs=written)
    return 0


def main(argv: Sequence[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        prog="python -m nexus.entrypoint",
        description="Run one Nexus @task handler under the executor file contract.",
    )
    p.add_argument("task_ref", help="task_ref of the handler to run, e.g. etl.extract")
    p.add_argument(
        "--module",
        action="append",
        default=[],
        dest="modules",
        help="Module (or .py path) whose @task handlers to register. Repeatable. "
        "Defaults to $NEXUS_TASK_MODULES (comma separated).",
    )
    args = p.parse_args(argv)
    modules = list(args.modules) or [
        m for m in (os.environ.get("NEXUS_TASK_MODULES") or "").split(",") if m
    ]
    if not modules:
        p.error("no module given: pass --module or set NEXUS_TASK_MODULES")
    # The working directory is where the image's code lives; make it importable
    # the way `python -m` would for a module run from there.
    if os.getcwd() not in sys.path:
        sys.path.insert(0, os.getcwd())
    return run_from_environment(args.task_ref, modules)


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
