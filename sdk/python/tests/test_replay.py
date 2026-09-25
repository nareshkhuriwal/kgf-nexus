# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""nexus.replay: re-running one attempt from the assignment endpoint's response.

The documents here are shaped exactly like the gateway's response
(services/api-gateway/rest/assignment.go): proto JSON, so an inline payload is
base64 and an int64 may be a string.
"""

from __future__ import annotations

import base64
import json
import pathlib
import textwrap

import pytest

from nexus import local, replay
from nexus.replay import assignment_from_wire
from nexus.task import registry


@pytest.fixture(autouse=True)
def _isolate_registry():
    registry.clear()
    yield
    registry.clear()


def _b64(v: object) -> str:
    return base64.b64encode(json.dumps(v).encode()).decode()


def _response(spec: dict, inputs: dict, *, node="load", map_index=None) -> dict:
    a = {
        "attempt_id": "22222222-2222-2222-2222-222222222222",
        "run_id": "11111111-1111-1111-1111-111111111111",
        "node_id": node, "attempt_number": 2, "namespace": "team-ml",
        "idempotency_key": "idem-from-the-control-plane",
        "spec": spec, "inputs": inputs,
    }
    if map_index is not None:
        a["map_index"] = map_index
    return {"namespace": "team-ml", "assignment": a, "encoding": "protojson",
            "result_store": {"inline_threshold_bytes": "4096"}, "secrets": [],
            "omitted": ["lease_token"]}


def test_wire_form_is_converted_without_guessing():
    doc = _response({"task_ref": "t"}, {
        "json": {"inline": _b64({"total": 135}), "content_type": "application/json"},
        "csv": {"inline": base64.b64encode(b"a,b\n1,2\n").decode(), "content_type": "application/octet-stream"},
        "big": {"blob": {"bucket": "b", "key": "k", "sha256": "ab"}, "size_bytes": "900000"},
    })
    native = assignment_from_wire(doc)
    assert native["inputs"]["json"]["inline"] == '{"total": 135}'
    assert base64.b64decode(native["inputs"]["csv"]["inline_base64"]) == b"a,b\n1,2\n"
    assert native["inputs"]["big"]["size_bytes"] == 900000
    assert native["result_store"] == {"inline_threshold_bytes": 4096}
    assert native["attempt_number"] == 2


def test_python_handler_replays_with_its_original_inputs_and_key(tmp_path):
    mod = tmp_path / "handlers_replay.py"
    seen = tmp_path / "seen.json"
    mod.write_text(textwrap.dedent(f"""
        import json
        from nexus import task

        @task(task_ref="etl.load")
        def load(ctx, summary):
            with open({str(seen)!r}, "w") as f:
                json.dump({{"summary": summary, "key": ctx.idempotency_key,
                           "attempt": ctx.attempt_number, "node": ctx.node_id}}, f)
            if summary["total"] > 100:
                raise RuntimeError("total too large: %d" % summary["total"])
            return summary
    """))
    doc = _response({"id": "load", "task_ref": "etl.load", "version": 1,
                     "timeouts": {"start_to_close_ms": "60000"}},
                    {"summary": {"inline": _b64({"total": 135}), "content_type": "application/json"}})
    res = replay.replay(doc, modules=[str(mod)], out_dir=str(tmp_path / "out"), out=None)
    assert res.state == "FAILED"
    assert res.error_class == "ERROR_CLASS_APPLICATION" and "total too large: 135" in res.message
    got = json.loads(seen.read_text())
    assert got == {"summary": {"total": 135}, "key": "idem-from-the-control-plane", "attempt": 2, "node": "load"}
    assert (tmp_path / "out" / "assignment.json").exists()


def test_shell_task_replays_with_staged_inputs(tmp_path):
    spec = {"id": "work[2]", "task_ref": "t.work", "version": 1, "outputs": {"result": {"type": "json"}},
            "runtime": {"executor": "subprocess", "command": ["sh", "-c"],
                        "args": ['printf \'{"item":%s,"key":"%s"}\' "$(cat "$NEXUS_INPUT_DIR/item")" '
                                 '"$NEXUS_IDEMPOTENCY_KEY" > "$NEXUS_OUTPUT_FILE"']}}
    doc = _response(spec, {"item": {"inline": _b64("shard-2"), "content_type": "application/json"}},
                    node="work[2]", map_index=2)
    res = replay.replay(doc, out_dir=str(tmp_path), out=None)
    assert res.succeeded, res.summary()
    assert res.output() == {"item": "shard-2", "key": "idem-from-the-control-plane"}


def test_blob_inputs_need_their_bytes(tmp_path):
    spec = {"id": "a", "task_ref": "t.a", "runtime": {"command": ["sh", "-c", 'wc -c < "$NEXUS_INPUT_DIR/big"']},
            "outputs": {"n": {"type": "json"}}}
    spec["runtime"]["stdout_output"] = "n"
    doc = _response(spec, {"big": {"blob": {"bucket": "b", "key": "team-ml/ab", "sha256": "ab"},
                                   "size_bytes": "12"}}, node="a")
    with pytest.raises(local.LocalRunError, match="--input NAME=PATH"):
        replay.replay(doc, out_dir=str(tmp_path / "x"), out=None)
    data = tmp_path / "big.bin"
    data.write_bytes(b"0123456789ab")
    res = replay.replay(doc, inputs={"big": str(data)}, out_dir=str(tmp_path / "y"), out=None)
    assert res.succeeded and int(res.output("n")) == 12


def test_cli_entrypoint(tmp_path, capsys):
    spec = {"id": "a", "task_ref": "t.a", "runtime": {"command": ["sh", "-c", "exit 4"]}}
    path = tmp_path / "assignment.json"
    path.write_text(json.dumps(_response(spec, {}, node="a")))
    assert replay.main([str(path), "--out", str(tmp_path / "o"), "--output", "json"]) == 1
    out = json.loads(capsys.readouterr().out)
    assert out["state"] == "FAILED" and "exit code 4" in out["message"]
    assert replay.main([str(tmp_path / "missing.json")]) == 2
    assert replay.main([str(path), "--input", "bad"]) == 2


def test_the_run_json_is_kept_beside_the_work(tmp_path):
    spec = {"id": "a", "task_ref": "t.a", "runtime": {"command": ["true"]}}
    replay.replay(_response(spec, {}, node="a"), out_dir=str(tmp_path), out=None)
    assert json.loads((pathlib.Path(tmp_path) / "replay.json").read_text())["state"] == "SUCCEEDED"
