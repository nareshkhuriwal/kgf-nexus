# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Replay one attempt on this machine, with the inputs it was given (ADR-0022).

    doc = client.get_assignment("3f2a...")         # GET .../attempts/{id}/assignment
    result = nexus.replay.replay(doc, modules=["mypkg.tasks"])

or ``nexusctl replay <attempt-id> --local``.

The assignment comes from the control plane with the TaskSpec, the resolved
input references and the idempotency key; never the lease token or a secret
value. A Python handler runs through ``python -m nexus.worker`` exactly as the
local runner runs one; a shell task runs its ``runtime.command`` with the same
NEXUS_* environment and staged inputs/ directory the agent provides.

Two things to know:
  * The replay uses the ORIGINAL idempotency key. That is the safe choice: a
    handler that hands the key to a downstream system is deduplicated there,
    where a fresh key would make the replay a second, real effect. A handler
    with un-keyed side effects will repeat them -- replay against test
    credentials when that matters.
  * Blob inputs are references into the platform's object store, which this
    process has no credentials for and the control plane never reads
    (invariant 6). Supply their bytes with ``inputs={"name": "/path"}``
    (``--input name=/path``); replay refuses to run without them rather than
    hand the task a reference where it expects data.
"""

from __future__ import annotations

import argparse
import base64
import dataclasses
import json
import os
import sys
import tempfile
import time
from typing import IO, Any, Mapping, Sequence

from . import _dag, local
from .context import DEFAULT_INLINE_THRESHOLD_BYTES, inline_ref
from .task import registry
from .worker import import_modules

__all__ = ["replay", "assignment_from_wire", "ReplayResult", "main"]


@dataclasses.dataclass
class ReplayResult:
    attempt_id: str
    node_id: str
    state: str  # SUCCEEDED | FAILED | TIMED_OUT
    error_class: str
    message: str
    exit_code: int | None
    work_dir: str
    outputs: dict[str, dict[str, Any]]
    store: Any

    @property
    def succeeded(self) -> bool:
        return self.state == "SUCCEEDED"

    def output(self, name: str = "result") -> Any:
        return self.store.get(self.outputs[name])

    def summary(self) -> str:
        lines = [f"replay of attempt {self.attempt_id} ({self.node_id}): {self.state}"]
        if not self.succeeded:
            lines.append(f"  {self.error_class or 'error'}: {self.message}")
        for name, ref in sorted(self.outputs.items()):
            where = "blob" if "blob" in ref else "inline"
            lines.append(f"  output {name}: {ref.get('size_bytes')} bytes ({where})")
        lines.append(f"  work directory: {self.work_dir}")
        return "\n".join(lines)


def _int(v: object, default: int = 0) -> int:
    """proto3 JSON renders int64 as a string; accept both."""
    if v is None or v == "":
        return default
    return int(str(v))


def assignment_from_wire(doc: Mapping[str, Any]) -> dict[str, Any]:
    """Convert the endpoint's response (or its ``assignment`` member) into the
    SDK's native assignment form.

    The wire form is proto JSON: inline payloads are base64 and int64 fields
    may be strings. Natively an inline JSON payload is text under ``inline``
    and anything else base64 under ``inline_base64`` -- so after this, no
    string's encoding has to be guessed.
    """
    a = dict(doc.get("assignment") or doc)
    out: dict[str, Any] = {
        "attempt_id": str(a.get("attempt_id", "")),
        "run_id": str(a.get("run_id", "")),
        "node_id": str(a.get("node_id", "")),
        "attempt_number": _int(a.get("attempt_number"), 1),
        "namespace": str(a.get("namespace") or doc.get("namespace") or "default"),
        "idempotency_key": str(a.get("idempotency_key", "")),
        "spec": dict(a.get("spec") or {}),
    }
    if a.get("map_index") is not None:
        out["map_index"] = _int(a.get("map_index"))
    rs = doc.get("result_store")
    if isinstance(rs, Mapping) and rs.get("inline_threshold_bytes") is not None:
        out["result_store"] = {"inline_threshold_bytes": _int(rs["inline_threshold_bytes"])}
    inputs: dict[str, dict[str, Any]] = {}
    for name, ref in (a.get("inputs") or {}).items():
        if not isinstance(ref, Mapping):
            continue
        ct = ref.get("content_type") or ref.get("contentType")
        if "inline" in ref:
            raw = ref["inline"]
            data = base64.b64decode(raw) if isinstance(raw, str) else bytes(raw)
            inputs[str(name)] = inline_ref(data, str(ct) if ct else None)
        elif "blob" in ref:
            b = dict(ref["blob"])
            inputs[str(name)] = {
                "blob": {"bucket": b.get("bucket", ""), "key": b.get("key", ""), "sha256": b.get("sha256", "")},
                "content_type": ct or "application/octet-stream",
                "size_bytes": _int(ref.get("size_bytes") or ref.get("sizeBytes")),
            }
    out["inputs"] = inputs
    return out


def replay(
    assignment: Mapping[str, Any],
    *,
    modules: Sequence[str] = (),
    inputs: Mapping[str, str | bytes] | None = None,
    out_dir: str | None = None,
    isolation: str = "subprocess",
    python: str | None = None,
    host_executor: bool = False,
    out: IO[str] | None = sys.stderr,
) -> ReplayResult:
    """Run one attempt again, locally, with its original inputs.

    ``assignment`` is what ``NexusClient.get_assignment`` returns (or the
    native form). ``inputs`` supplies or overrides input bytes by name: a path
    to read, or the bytes themselves. ``modules`` are imported so a Python
    handler can be found (``--module`` of ``python -m nexus.worker``).
    """
    native = assignment_from_wire(assignment) if "assignment" in assignment else dict(assignment)
    spec = dict(native["spec"])
    command = (spec.get("runtime") or {}).get("command")
    if modules and not command and registry.get(str(spec.get("task_ref", ""))) is None:
        # Import here too, so the preflight in THIS process can find the handler
        # the worker subprocess will run.
        import_modules(list(modules))
    node_id = native["node_id"] or str(spec.get("id") or "task")
    spec["id"] = node_id
    root = os.path.abspath(out_dir or tempfile.mkdtemp(prefix="nexus-replay-"))
    os.makedirs(root, exist_ok=True)
    with open(os.path.join(root, "assignment.json"), "w", encoding="utf-8") as f:
        json.dump(native, f, sort_keys=True, indent=2)

    threshold = _int((native.get("result_store") or {}).get("inline_threshold_bytes"), 0) or None
    runner = local._Runner(
        {"api_version": "nexus.dev/v1", "name": "replay", "namespace": native.get("namespace"),
         "tasks": [spec]},
        {}, run_id=native["run_id"] or "replay", run_dir=root,
        modules=list(modules) or local._handler_modules({"tasks": [spec]}),
        python=python or sys.executable, isolation=isolation, max_parallel=1, backoff_scale=0.0,
        fan_in_limit=_dag.DEFAULT_MAX_FAN_IN_BYTES,
        max_fanout_ceiling=_dag.DEFAULT_MAX_FANOUT_CEILING,
        inline_threshold=threshold or DEFAULT_INLINE_THRESHOLD_BYTES,
        host_executor=host_executor, seed=None,
        log=(lambda m: print(f"[replay] {m}", file=out, flush=True)) if out is not None else (lambda _m: None),
    )
    try:
        if native.get("map_index") is not None and node_id.endswith(f"[{native['map_index']}]"):
            runner.children[node_id] = (node_id[: -len(f"[{native['map_index']}]")], int(native["map_index"]))
            runner.rebuild()
        runner.fixed_idempotency_key = native.get("idempotency_key") or None
        refs: dict[str, dict[str, Any]] = {}
        for name, ref in (native.get("inputs") or {}).items():
            refs[name] = dict(ref)
        for name, value in (inputs or {}).items():
            if isinstance(value, str):
                with open(value, "rb") as fh:
                    data = fh.read()
            else:
                data = bytes(value)
            refs[name] = runner.store.put_bytes(data)
        missing = sorted(n for n, r in refs.items() if "blob" in r and not _local_blob(runner, r))
        if missing:
            raise local.LocalRunError(
                "these inputs are blobs in the platform's object store, which a replay cannot read: "
                + ", ".join(missing)
                + ". Download them and pass --input NAME=PATH (inputs={...}) for each."
            )
        runner.preflight()
        attempt = local._Attempt(
            node_id, _int(native.get("attempt_number"), 1), native["attempt_id"] or "replay", refs, root
        )
        os.makedirs(os.path.join(root, "inputs"), exist_ok=True)
        os.makedirs(os.path.join(root, "outputs"), exist_ok=True)
        started = time.time()
        outcome = runner.execute(attempt)
    finally:
        runner.events.close()
    result = ReplayResult(
        attempt_id=attempt.attempt_id, node_id=node_id, state=outcome.state,
        error_class=outcome.error_class, message=outcome.message, exit_code=outcome.exit_code,
        work_dir=root, outputs=outcome.outputs, store=runner.store,
    )
    with open(os.path.join(root, "replay.json"), "w", encoding="utf-8") as f:
        json.dump({"state": result.state, "error_class": result.error_class, "message": result.message,
                   "exit_code": result.exit_code, "duration_s": round(time.time() - started, 3),
                   "outputs": {k: {"size_bytes": v.get("size_bytes")} for k, v in result.outputs.items()}},
                  f, sort_keys=True, indent=2)
    if out is not None:
        print(result.summary(), file=out, flush=True)
    return result


def _local_blob(runner: local._Runner, ref: Mapping[str, Any]) -> bool:
    try:
        runner.store.get_bytes(ref)
    except OSError:
        return False
    return True


def main(argv: Sequence[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        prog="python -m nexus.replay",
        description="Replay one attempt locally from its assignment (nexusctl replay --local).",
    )
    p.add_argument("assignment", help="The JSON the assignment endpoint returned ('-' for stdin).")
    p.add_argument("--module", action="append", default=[], help="Module or .py path defining the handler.")
    p.add_argument("--input", action="append", default=[], help="NAME=PATH: bytes for a (blob) input.")
    p.add_argument("--out", default="", help="Directory for the attempt's work dir and results.")
    p.add_argument("--isolation", choices=("subprocess", "inprocess"), default="subprocess")
    p.add_argument("--host-executor", action="store_true")
    p.add_argument("--output", choices=("table", "json"), default="table")
    args = p.parse_args(argv)
    try:
        raw = sys.stdin.read() if args.assignment == "-" else open(args.assignment, encoding="utf-8").read()
        doc = json.loads(raw)
        overrides: dict[str, str | bytes] = {}
        for item in args.input:
            name, sep, path = item.partition("=")
            if not sep or not name:
                raise local.LocalRunError(f"--input {item!r} is not NAME=PATH")
            overrides[name] = path
        result = replay(doc, modules=args.module, inputs=overrides, out_dir=args.out or None,
                        isolation=args.isolation, host_executor=args.host_executor,
                        out=sys.stderr if args.output == "table" else None)
    except (local.LocalRunError, ValueError, OSError, KeyError) as e:
        print(f"nexus.replay: {e}", file=sys.stderr)
        return 2
    if args.output == "json":
        print(json.dumps({"state": result.state, "error_class": result.error_class,
                          "message": result.message, "work_dir": result.work_dir,
                          "outputs": sorted(result.outputs)}, sort_keys=True))
    return 0 if result.succeeded else 1


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
