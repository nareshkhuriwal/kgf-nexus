"""Worker-side handler entrypoint: run one registered handler by task_ref.

This is what the worker agent invokes inside the task container. The flow is:

    agent  --(TaskAssignment as JSON)-->  python -m nexus.worker  --> outcome JSON

The agent has already polled, leased and resolved inputs; this process does one
thing, once, and exits. It is a one-shot process on purpose:

  * the handler cannot leak state into the next task,
  * a handler that wedges is killed by killing the process,
  * and an OOM kills the task, not the agent, which keeps the agent's own
    lease-renewal loop alive to report the failure honestly.

The control plane is nowhere in this path. It never imports user code (ADR-0004);
only this process, inside the container, does.
"""

from __future__ import annotations

import argparse
import importlib
import json
import os
import sys
import time
import traceback
from typing import Any, Mapping, Sequence

from .context import FileResultStore, InMemoryResultStore, TaskContext, derive_idempotency_key
from .task import TaskDefinition, registry

__all__ = ["execute_assignment", "run_handler", "main", "HandlerNotFound"]


class HandlerNotFound(LookupError):
    """No handler registered for a task_ref after importing the given modules.

    Reported as ERROR_CLASS_NOT_FOUND, which is terminal: retrying cannot make a
    missing handler appear, and burning three attempts on it wastes capacity and
    delays the real error reaching a human.
    """


def import_modules(modules: Sequence[str]) -> None:
    """Import user modules so their @task decorators register handlers.

    Import happens HERE, in the task container, which is the only place a user
    dependency needs to exist. The Airflow failure mode being avoided is the
    scheduler needing every user dependency installed (ADR-0004).
    """
    for name in modules:
        if name.endswith(".py") and os.path.exists(name):
            # A file path, for the subprocess executor running straight off a
            # mounted workspace rather than an installed package.
            import importlib.util

            spec = importlib.util.spec_from_file_location(
                os.path.splitext(os.path.basename(name))[0], name
            )
            if spec is None or spec.loader is None:
                raise HandlerNotFound(f"cannot load module from path {name!r}")
            module = importlib.util.module_from_spec(spec)
            sys.modules[spec.name] = module
            spec.loader.exec_module(module)
        else:
            importlib.import_module(name)


def resolve(task_ref: str) -> TaskDefinition:
    definition = registry.get(task_ref)
    if definition is None:
        raise HandlerNotFound(
            f"no handler registered for task_ref {task_ref!r}. Registered: "
            f"{', '.join(registry.refs()) or '(none)'}. Did --module import the "
            "module containing the @task?"
        )
    return definition


def build_context(assignment: Mapping[str, Any], *, store: Any | None = None) -> TaskContext:
    """Build a TaskContext from a TaskAssignment (proto/nexus/v1/task.proto).

    The idempotency key comes FROM THE ASSIGNMENT -- the control plane derived it
    and it is authoritative. We recompute it only to verify, because a key that
    silently differs between control plane and worker would break dedup in the one
    situation it exists for, and a loud mismatch is far cheaper than a duplicate
    charge.
    """
    run_id = str(assignment.get("run_id") or "")
    node_id = str(assignment.get("node_id") or "")
    spec = assignment.get("spec") or {}
    task_version = int(spec.get("version") or assignment.get("task_version") or 1)
    map_index = assignment.get("map_index")

    key = str(assignment.get("idempotency_key") or "")
    expected = derive_idempotency_key(run_id, node_id, map_index, task_version)
    if not key:
        key = expected
    elif key != expected:
        # Not fatal -- the control plane's key is authoritative, and a version skew
        # between agent and control plane is a legitimate reason for a difference
        # (architecture §9 requires N-1 agent compatibility). But it must be
        # visible.
        print(
            json.dumps(
                {
                    "level": "WARN",
                    "msg": "idempotency key from control plane differs from locally derived key",
                    "run_id": run_id,
                    "node_id": node_id,
                    "using": "control_plane_key",
                }
            ),
            file=sys.stderr,
            flush=True,
        )

    if store is None:
        root = os.environ.get("NEXUS_RESULT_DIR")
        store = FileResultStore(root) if root else InMemoryResultStore()

    return TaskContext(
        run_id=run_id,
        node_id=node_id,
        attempt_id=str(assignment.get("attempt_id") or ""),
        attempt_number=int(assignment.get("attempt_number") or 1),
        task_ref=str(spec.get("task_ref") or assignment.get("task_ref") or ""),
        task_version=task_version,
        idempotency_key=key,
        idempotency_mode=_short_idempotency(spec.get("idempotency")),
        map_index=map_index if map_index is None else int(map_index),
        namespace=str(assignment.get("namespace") or "default"),
        worker_id=str(assignment.get("worker_id") or os.environ.get("NEXUS_WORKER_ID") or ""),
        lease_token=str(assignment.get("lease_token") or ""),
        start_to_close_deadline_unix_ms=int(assignment.get("start_to_close_deadline_unix_ms") or 0),
        store=store,
        inputs=dict(assignment.get("inputs") or {}),
    )


def _short_idempotency(value: Any) -> str:
    """Map the wire enum name back to the author-facing word."""
    table = {
        "IDEMPOTENCY_MODE_REQUIRED": "required",
        "IDEMPOTENCY_MODE_BEST_EFFORT": "best_effort",
        "IDEMPOTENCY_MODE_NONE": "none",
        1: "required",
        2: "best_effort",
        3: "none",
    }
    return table.get(value, "required") if value is not None else "required"


def _resolve_inputs(ctx: TaskContext, definition: TaskDefinition, assignment: Mapping[str, Any]) -> dict[str, Any]:
    """Materialize handler kwargs from resolved ResultRefs and literals.

    Blob inputs are fetched from the result store by the handler's process, not
    passed through the control plane (invariant 6).
    """
    kwargs: dict[str, Any] = {}
    accepted = set(definition.parameter_names)
    for name, ref in (assignment.get("inputs") or {}).items():
        if accepted and name not in accepted:
            # An input the handler does not accept is dropped rather than passed,
            # so adding an input to a spec does not break a not-yet-redeployed
            # handler with a TypeError.
            ctx.log.warning("dropping input not accepted by handler", input=name)
            continue
        kwargs[name] = _materialize(ctx, ref)
    for name, raw in (assignment.get("input_literals") or {}).items():
        if accepted and name not in accepted:
            continue
        kwargs.setdefault(name, json.loads(raw) if isinstance(raw, str) else raw)
    return kwargs


def _materialize(ctx: TaskContext, ref: Any) -> Any:
    if isinstance(ref, Mapping) and ("inline" in ref or "blob" in ref):
        return ctx.store.get(ref)
    return ref


def run_handler(
    definition: TaskDefinition, ctx: TaskContext, kwargs: Mapping[str, Any]
) -> Any:
    """Invoke the handler with its context. No retry logic here -- that is the platform's.

    Retrying inside the worker would hide failures from the control plane's retry
    policy, its backoff, and its `retry_on` classification, and would make
    `attempt_number` a lie.
    """
    return definition.fn(ctx, **kwargs)


def execute_assignment(
    assignment: Mapping[str, Any], *, modules: Sequence[str] = (), store: Any | None = None
) -> dict[str, Any]:
    """Execute one assignment and return a ReportResult-shaped outcome dict.

    Never raises for a handler failure: a handler exception is a RESULT (FAILED
    with a classified error), and the agent needs that result to report it. Only
    a failure to even build the context escapes.
    """
    started_ms = int(time.time() * 1000)
    if modules:
        try:
            import_modules(modules)
        except Exception as e:
            return _failure_outcome(
                "ERROR_CLASS_NOT_FOUND",
                type(e).__name__,
                f"failed to import handler module(s) {', '.join(modules)}: {e}",
                started_ms,
                retryable=False,
            )

    try:
        definition = resolve(str((assignment.get("spec") or {}).get("task_ref") or assignment.get("task_ref") or ""))
    except HandlerNotFound as e:
        return _failure_outcome(
            "ERROR_CLASS_NOT_FOUND", "HandlerNotFound", str(e), started_ms, retryable=False
        )

    ctx = build_context(assignment, store=store)
    try:
        kwargs = _resolve_inputs(ctx, definition, assignment)
    except Exception as e:
        # A bad input is the author's or the upstream's problem, and re-running
        # will produce the identical bad input: terminal, not retryable.
        return _failure_outcome(
            "ERROR_CLASS_VALIDATION",
            type(e).__name__,
            f"cannot resolve inputs: {e}",
            started_ms,
            retryable=False,
        )

    ctx.log.info("task starting", idempotency_key_prefix=ctx.idempotency_key[:12])
    try:
        value = run_handler(definition, ctx, kwargs)
    except MemoryError:
        # Retryable on a larger worker; the platform, not us, decides whether to.
        return _failure_outcome(
            "ERROR_CLASS_OOM", "MemoryError", "handler exhausted memory", started_ms
        )
    except KeyboardInterrupt:
        # SIGINT/SIGTERM during cancellation. Reported as cancelled rather than
        # failed so a deliberate cancel does not look like a code defect.
        return {
            "outcome": "ATTEMPT_OUTCOME_FAILED",
            "error": {
                "class": "ERROR_CLASS_TRANSIENT",
                "type": "Cancelled",
                "message": "handler interrupted",
                "retryable": False,
            },
            "metrics": {"wall_ms": int(time.time() * 1000) - started_ms},
        }
    except Exception as e:
        tb = traceback.format_exc()
        ctx.log.error("task failed", error_type=type(e).__name__, error=str(e))
        detail_ref = ""
        try:
            # The full traceback goes to the result store, not into the metadata
            # store: unbounded text in Postgres is how a metadata store dies.
            ref = ctx.store.put({"traceback": tb}, key=f"errors/{ctx.attempt_id or ctx.idempotency_key}")
            blob = ref.get("blob") if isinstance(ref, Mapping) else None
            detail_ref = blob["key"] if blob else ""
        except Exception:  # pragma: no cover - never let diagnostics mask the error
            pass
        return {
            "outcome": "ATTEMPT_OUTCOME_FAILED",
            "error": {
                "class": _classify(e),
                "type": type(e).__name__,
                "message": str(e)[:2000],
                "retryable": _classify(e)
                in ("ERROR_CLASS_TRANSIENT", "ERROR_CLASS_TIMEOUT", "ERROR_CLASS_INFRASTRUCTURE", "ERROR_CLASS_OOM"),
                "detail_ref": detail_ref,
            },
            "metrics": {"wall_ms": int(time.time() * 1000) - started_ms},
        }

    output_ref = ctx.store.put(value) if value is not None else None
    ctx.log.info("task succeeded")
    outcome: dict[str, Any] = {
        "outcome": "ATTEMPT_OUTCOME_SUCCEEDED",
        "metrics": {"wall_ms": int(time.time() * 1000) - started_ms, "exit_code": 0},
    }
    if output_ref is not None:
        outcome["output_ref"] = output_ref
    return outcome


def _classify(e: BaseException) -> str:
    """Best-effort error classification. Advisory: the control plane decides.

    The worker sees the real exception and so is the only place that can classify
    at all, but `class` drives platform retry policy, so a wrong guess costs
    capacity. Conservative: unrecognized exceptions are APPLICATION (terminal
    unless the author explicitly lists them in retry_on).
    """
    if isinstance(e, TimeoutError):
        return "ERROR_CLASS_TIMEOUT"
    if isinstance(e, (ConnectionError, BrokenPipeError)):
        return "ERROR_CLASS_TRANSIENT"
    if isinstance(e, PermissionError):
        return "ERROR_CLASS_PERMISSION_DENIED"
    if isinstance(e, FileNotFoundError):
        return "ERROR_CLASS_NOT_FOUND"
    if isinstance(e, (ValueError, TypeError, KeyError)):
        return "ERROR_CLASS_VALIDATION"
    if isinstance(e, OSError):
        return "ERROR_CLASS_INFRASTRUCTURE"
    return "ERROR_CLASS_APPLICATION"


def _failure_outcome(
    error_class: str, error_type: str, message: str, started_ms: int, *, retryable: bool = True
) -> dict[str, Any]:
    return {
        "outcome": "ATTEMPT_OUTCOME_FAILED",
        "error": {
            "class": error_class,
            "type": error_type,
            "message": message[:2000],
            "retryable": retryable,
        },
        "metrics": {"wall_ms": int(time.time() * 1000) - started_ms},
    }


def main(argv: Sequence[str] | None = None) -> int:
    """CLI entrypoint. `python -m nexus.worker --assignment a.json --module pkg.tasks`.

    Exit code 0 means "the outcome was reported on stdout", not "the task
    succeeded" -- the agent reads the outcome document to learn that. A non-zero
    exit means this process could not produce an outcome at all, which the agent
    treats as an infrastructure failure of the executor.
    """
    p = argparse.ArgumentParser(
        prog="python -m nexus.worker",
        description="Execute one registered Nexus task handler by task_ref.",
    )
    p.add_argument(
        "--assignment",
        default="-",
        help="Path to a TaskAssignment JSON document, or '-' for stdin.",
    )
    p.add_argument(
        "--module",
        action="append",
        default=[],
        dest="modules",
        help="Module (or .py path) to import so its @task handlers register. Repeatable.",
    )
    p.add_argument("--task-ref", default=None, help="Override the assignment's task_ref.")
    p.add_argument("--output", default="-", help="Where to write the outcome JSON ('-' = stdout).")
    p.add_argument(
        "--result-dir",
        default=os.environ.get("NEXUS_RESULT_DIR"),
        help="Local result-store directory (also NEXUS_RESULT_DIR).",
    )
    args = p.parse_args(argv)

    modules = list(args.modules) or [
        m for m in (os.environ.get("NEXUS_TASK_MODULES") or "").split(",") if m
    ]

    try:
        raw = sys.stdin.read() if args.assignment == "-" else open(args.assignment).read()
        assignment = json.loads(raw)
        if not isinstance(assignment, dict):
            raise ValueError("assignment must be a JSON object")
    except (OSError, ValueError) as e:
        print(f"nexus.worker: cannot read assignment: {e}", file=sys.stderr)
        return 2

    if args.task_ref:
        assignment.setdefault("spec", {})["task_ref"] = args.task_ref

    store = FileResultStore(args.result_dir) if args.result_dir else None
    outcome = execute_assignment(assignment, modules=modules, store=store)

    text = json.dumps(outcome, sort_keys=True, indent=2)
    if args.output == "-":
        print(text)
    else:
        with open(args.output, "w") as f:
            f.write(text + "\n")
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
