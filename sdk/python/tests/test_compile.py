# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Compilation: determinism, edge inference, and spec shape.

The determinism tests are the load-bearing ones. The server treats a
byte-identical resubmission as a no-op and any difference as a NEW VERSION
(ADR-0004), so a compiler whose output depends on dict insertion order, set
iteration order, or PYTHONHASHSEED would mint a version on every compile.
"""

from __future__ import annotations

import json
import subprocess
import sys
import textwrap

import pytest

from nexus import Gpu, Resources, Retry, SpecError, Timeouts, task, workflow
from nexus.task import registry


@pytest.fixture(autouse=True)
def _isolate_registry():
    """Each test registers its own task_refs; a shared registry would collide."""
    registry.clear()
    yield
    registry.clear()


def _linear_workflow():
    @task(task_ref="a", resources=Resources(cpu="2", memory="8Gi"))
    def a(ctx, source):
        return source

    @task(task_ref="b", resources=Resources(cpu="1", memory="2Gi"))
    def b(ctx, data):
        return data

    @task(task_ref="c")
    def c(ctx, data):
        return data

    @workflow(name="linear")
    def wf(source):
        return c(b(a(source)))

    return wf


# ── determinism ───────────────────────────────────────────────────────────────


def test_compiling_twice_is_byte_identical() -> None:
    wf = _linear_workflow()
    first = wf.compile()
    second = wf.compile()
    assert first.json == second.json
    assert first.hash == second.hash


def test_compile_is_stable_across_processes_with_hash_randomization() -> None:
    """The real determinism test: a fresh interpreter with a different PYTHONHASHSEED.

    Compiling twice in ONE process can pass while still being order-dependent,
    because dict and set iteration order is stable within a process. Only a new
    interpreter with a different seed exercises that.
    """
    script = textwrap.dedent(
        """
        from nexus import task, workflow, Resources
        @task(task_ref="alpha", resources=Resources(cpu="2", memory="8Gi",
              custom={"license_token": 1, "aaa": 2}))
        def alpha(ctx, source): return source
        @task(task_ref="beta", runtime={"executor": "docker", "image": "img:1",
              "env": {"Z": "1", "A": "2", "M": "3"}})
        def beta(ctx, data, extra): return data
        @workflow(name="seeded", labels={"z": "1", "a": "2"})
        def wf(source): return beta(alpha(source), extra="lit")
        print(wf.compile().hash)
        """
    )
    hashes = set()
    for seed in ("0", "1", "12345", "99991"):
        out = subprocess.run(
            [sys.executable, "-c", script],
            capture_output=True,
            text=True,
            check=True,
            env={"PYTHONHASHSEED": seed, "PYTHONPATH": ".", "PATH": "/usr/bin:/bin"},
        )
        hashes.add(out.stdout.strip())
    assert len(hashes) == 1, f"spec hash varies with PYTHONHASHSEED: {hashes}"


def test_tasks_are_sorted_by_id_not_declaration_order() -> None:
    @task(task_ref="zebra")
    def zebra(ctx):
        return 1

    @task(task_ref="alpha")
    def alpha(ctx, x):
        return x

    @workflow(name="ordered")
    def wf():
        # Declared zebra-first on purpose; the emitted document must not care.
        return alpha(zebra())

    ids = wf.compile().task_ids
    assert ids == sorted(ids) == ["alpha", "zebra"]


def test_declaration_order_does_not_change_the_hash() -> None:
    def build(reverse: bool):
        registry.clear()

        @task(task_ref="p")
        def p(ctx):
            return 1

        @task(task_ref="q")
        def q(ctx, x):
            return x

        if reverse:

            @workflow(name="same")
            def wf():
                first = p()
                return q(first)

        else:

            @workflow(name="same")
            def wf():
                return q(p())

        return wf.compile()

    assert build(False).hash == build(True).hash


# ── edge inference ────────────────────────────────────────────────────────────


def test_linear_edges_are_inferred_from_traced_calls() -> None:
    spec = _linear_workflow().compile().spec
    edges = spec["edges"]
    assert [(e["from_node_id"], e["to_node_id"]) for e in edges] == [("a", "b"), ("b", "c")]
    # The downstream PARAMETER NAME is the key; the upstream output name is the
    # value. That mapping is what lets the server type-check the edge.
    assert edges[0]["output_to_input"] == {"data": "result"}


def test_edges_are_sorted_canonically() -> None:
    @task(task_ref="root")
    def root(ctx):
        return 1

    @task(task_ref="zzz")
    def zzz(ctx, x):
        return x

    @task(task_ref="aaa")
    def aaa(ctx, x):
        return x

    @workflow(name="fanout")
    def wf():
        r = root()
        zzz(r)
        aaa(r)

    edges = wf.compile().spec["edges"]
    assert [(e["from_node_id"], e["to_node_id"]) for e in edges] == [
        ("root", "aaa"),
        ("root", "zzz"),
    ]


def test_fan_out_fan_in() -> None:
    @task(task_ref="src")
    def src(ctx):
        return 1

    @task(task_ref="branch")
    def branch(ctx, x):
        return x

    @task(task_ref="join")
    def join(ctx, left, right):
        return left

    @workflow(name="diamond")
    def wf():
        s = src()
        return join(branch(s), branch(s))

    spec = wf.compile().spec
    assert sorted(t["id"] for t in spec["tasks"]) == ["branch", "branch_2", "join", "src"]
    pairs = {(e["from_node_id"], e["to_node_id"]) for e in spec["edges"]}
    assert pairs == {("src", "branch"), ("src", "branch_2"), ("branch", "join"), ("branch_2", "join")}
    join_edges = {e["from_node_id"]: e["output_to_input"] for e in spec["edges"] if e["to_node_id"] == "join"}
    assert join_edges == {"branch": {"left": "result"}, "branch_2": {"right": "result"}}


def test_multi_output_task_requires_explicit_selection() -> None:
    @task(task_ref="train", outputs={"model": {"type": "file_ref"}, "metrics": {"type": "json"}})
    def train(ctx):
        return {}

    @task(task_ref="deploy")
    def deploy(ctx, model):
        return model

    @workflow(name="explicit")
    def good():
        t = train()
        return deploy(t["model"])

    edge = good.compile().spec["edges"][0]
    assert edge["output_to_input"] == {"model": "model"}

    @workflow(name="ambiguous")
    def bad():
        return deploy(train())

    with pytest.raises(SpecError, match="declares multiple outputs"):
        bad.compile()


def test_unknown_output_name_is_rejected() -> None:
    @task(task_ref="t1", outputs={"model": {"type": "file_ref"}})
    def t1(ctx):
        return {}

    @task(task_ref="t2")
    def t2(ctx, x):
        return x

    @workflow(name="badout")
    def wf():
        return t2(t1()["nope"])

    with pytest.raises(SpecError, match="has no output"):
        wf.compile()


def test_literals_and_params_are_distinguished() -> None:
    @task(task_ref="cfg")
    def cfg(ctx, epochs, source):
        return epochs

    @workflow(name="bindings")
    def wf(source):
        return cfg(epochs=10, source=source)

    t = wf.compile().spec["tasks"][0]
    # A literal is baked into the spec; a param records "bind at run creation",
    # so the spec hash does not depend on the author's environment.
    assert t["input_literals"] == {"epochs": "10"}
    assert t["input_params"] == {"source": "source"}


def test_output_ref_cannot_be_used_as_a_value() -> None:
    @task(task_ref="v")
    def v(ctx):
        return 1

    @task(task_ref="w")
    def w(ctx, x):
        return x

    @workflow(name="truthy")
    def wf():
        got = v()
        if got:  # a compile-time value that does not exist
            return w(got)
        return w(got)

    with pytest.raises(SpecError, match="truth of"):
        wf.compile()


def test_collection_of_outputs_is_rejected_not_guessed() -> None:
    @task(task_ref="one")
    def one(ctx):
        return 1

    @task(task_ref="many")
    def many(ctx, items):
        return items

    @workflow(name="collection")
    def wf():
        return many([one(), one()])

    with pytest.raises(SpecError, match="collection containing task outputs"):
        wf.compile()


def test_unknown_parameter_is_rejected_at_compile_time() -> None:
    @task(task_ref="strict")
    def strict(ctx, known):
        return known

    @workflow(name="typo")
    def wf():
        return strict(knwon=1)

    with pytest.raises(SpecError, match="has no parameter 'knwon'"):
        wf.compile()


def test_empty_workflow_is_rejected() -> None:
    @workflow(name="empty")
    def wf():
        return None

    with pytest.raises(SpecError, match="traced zero tasks"):
        wf.compile()


def test_explicit_node_id_allows_one_handler_as_many_nodes() -> None:
    @task(task_ref="shard")
    def shard(ctx, n):
        return n

    @workflow(name="sharded")
    def wf():
        for i in range(3):
            shard(n=i, node_id=f"shard_{i}")

    assert wf.compile().task_ids == ["shard_0", "shard_1", "shard_2"]


def test_cycle_is_rejected_locally() -> None:
    @task(task_ref="cyc")
    def cyc(ctx, x):
        return x

    from nexus.workflow import OutputRef, _Builder, _builder

    builder = _Builder("manual")
    token = _builder.set(builder)
    try:
        # Hand-build a cycle: only reachable by referencing a node id directly,
        # which is exactly the case the local check is for.
        cyc(OutputRef("cyc_2", "result"), node_id="cyc_1")
        cyc(OutputRef("cyc_1", "result"), node_id="cyc_2")
    finally:
        _builder.reset(token)

    wf = workflow(name="cyclic")(lambda: None)
    with pytest.raises(SpecError, match="cycle"):
        wf._emit_graph(builder)


# ── spec shape ────────────────────────────────────────────────────────────────


def test_resources_emit_request_and_limit_vectors() -> None:
    @task(
        task_ref="big",
        resources=Resources(
            cpu="2000m",
            memory="8Gi",
            cpu_limit="4",
            memory_limit="16Gi",
            gpu=Gpu(count=2, kind="nvidia-a100", memory="40Gi"),
            disk="50Gi",
            custom={"license_token": 1},
        ),
    )
    def big(ctx):
        return 1

    @workflow(name="res")
    def wf():
        return big()

    res = wf.compile().spec["tasks"][0]["resources"]
    assert res["request"] == {
        "cpu_millis": 2000,
        "memory_bytes": 8 * 1024**3,
        "disk_bytes": 50 * 1024**3,
        "gpu_count": 2,
        "gpu_kind": "nvidia-a100",
        "gpu_memory_bytes": 40 * 1024**3,
        "custom": {"license_token": 1},
    }
    assert res["limit"]["cpu_millis"] == 4000
    assert res["limit"]["memory_bytes"] == 16 * 1024**3
    # GPUs are not burstable, so the limit mirrors the request.
    assert res["limit"]["gpu_count"] == 2


def test_limit_below_request_is_rejected() -> None:
    with pytest.raises(SpecError, match="below memory request"):

        @task(task_ref="shrunk", resources=Resources(memory="8Gi", memory_limit="4Gi"))
        def shrunk(ctx):
            return 1


def test_all_four_timeouts_are_emitted() -> None:
    @task(
        task_ref="four",
        timeouts=Timeouts(
            schedule_to_start="30m", start_to_close="4h", schedule_to_close="6h", heartbeat="5m"
        ),
    )
    def four(ctx):
        return 1

    @workflow(name="t")
    def wf():
        return four()

    assert wf.compile().spec["tasks"][0]["timeouts"] == {
        "schedule_to_start_ms": 1_800_000,
        "start_to_close_ms": 14_400_000,
        "schedule_to_close_ms": 21_600_000,
        "heartbeat_ms": 300_000,
    }


def test_partial_timeouts_merge_over_platform_defaults() -> None:
    # Setting only start_to_close must not silently drop the heartbeat timeout --
    # that is the one that catches a wedged task.
    @task(task_ref="partial", timeouts={"start_to_close": "10m"})
    def partial(ctx):
        return 1

    @workflow(name="pt")
    def wf():
        return partial()

    tos = wf.compile().spec["tasks"][0]["timeouts"]
    assert tos["start_to_close_ms"] == 600_000
    assert tos["heartbeat_ms"] == 300_000
    assert tos["schedule_to_start_ms"] == 1_800_000


def test_schedule_to_close_shorter_than_start_to_close_is_rejected() -> None:
    with pytest.raises(SpecError, match="shorter than start_to_close"):

        @task(task_ref="impossible", timeouts={"start_to_close": "4h", "schedule_to_close": "1h"})
        def impossible(ctx):
            return 1


def test_unknown_timeout_name_is_rejected() -> None:
    with pytest.raises(SpecError, match="unknown timeout"):

        @task(task_ref="typo_to", timeouts={"start_to_finish": "1h"})
        def typo(ctx):
            return 1


def test_retry_policy_shape_and_canonical_error_classes() -> None:
    @task(
        task_ref="retried",
        retry=Retry(
            max_attempts=4,
            backoff="exponential",
            initial="10s",
            max="10m",
            multiplier=2.0,
            jitter="full",
            retry_on=["transient", "timeout", "worker_lost"],
            no_retry_on=["validation_error", "permission_denied"],
            retry_on_different_worker=True,
        ),
    )
    def retried(ctx):
        return 1

    @workflow(name="r")
    def wf():
        return retried()

    r = wf.compile().spec["tasks"][0]["retry"]
    assert r["max_attempts"] == 4
    assert r["strategy"] == "BACKOFF_STRATEGY_EXPONENTIAL"
    # Sorted, so the list order in the decorator cannot change the hash.
    assert r["retry_on"] == ["ERROR_CLASS_TIMEOUT", "ERROR_CLASS_TRANSIENT", "ERROR_CLASS_WORKER_LOST"]
    # The architecture's YAML writes `validation_error`; the proto enum is
    # ERROR_CLASS_VALIDATION. Both author spellings must land on the enum.
    assert r["no_retry_on"] == ["ERROR_CLASS_PERMISSION_DENIED", "ERROR_CLASS_VALIDATION"]
    assert r["retry_on_different_worker"] is True


def test_contradictory_retry_classes_are_rejected() -> None:
    with pytest.raises(SpecError, match="both"):

        @task(task_ref="contradiction", retry=Retry(retry_on=["timeout"], no_retry_on=["timeout"]))
        def contradiction(ctx):
            return 1


def test_idempotency_defaults_to_required() -> None:
    @task(task_ref="defaulted")
    def defaulted(ctx):
        return 1

    @workflow(name="i")
    def wf():
        return defaulted()

    assert wf.compile().spec["tasks"][0]["idempotency"] == "IDEMPOTENCY_MODE_REQUIRED"


def test_idempotency_none_is_accepted_explicitly() -> None:
    @task(task_ref="unsafe", idempotency="none")
    def unsafe(ctx):
        return 1

    @workflow(name="i2")
    def wf():
        return unsafe()

    assert wf.compile().spec["tasks"][0]["idempotency"] == "IDEMPOTENCY_MODE_NONE"


def test_unimplemented_trigger_rule_is_rejected_at_compile_time() -> None:
    # In the proto but not in the Phase 1 DAG engine. Failing here beats emitting
    # a spec the server rejects at submit.
    with pytest.raises(SpecError, match="not implemented by"):

        @task(task_ref="future", trigger_rule="one_success")
        def future(ctx):
            return 1


def test_secret_values_cannot_be_written_into_a_spec() -> None:
    from nexus import Runtime, SecretRef

    with pytest.raises(SpecError, match="never be written into a spec"):

        @task(
            task_ref="leaky",
            runtime=Runtime(executor="docker", image="i:1", secrets=[SecretRef("K", "hunter2")]),
        )
        def leaky(ctx):
            return 1


def test_docker_runtime_requires_an_image() -> None:
    from nexus import Runtime

    with pytest.raises(SpecError, match="requires an image"):

        @task(task_ref="imageless", runtime=Runtime(executor="docker"))
        def imageless(ctx):
            return 1


def test_unknown_task_option_is_rejected() -> None:
    # `task()` has an explicit keyword-only signature, so a misspelled option is
    # caught by Python itself with a clearer message than any check we could
    # write. The internal builder has its own guard for programmatic callers.
    with pytest.raises(TypeError, match="retires"):

        @task(task_ref="typo_opt", retires=3)  # type: ignore[call-arg]
        def typo(ctx):
            return 1

    from nexus.task import _build_declaration

    with pytest.raises(SpecError, match="unknown option"):
        _build_declaration(lambda ctx: None, {"task_ref": "t", "retires": 3})


def test_duplicate_task_ref_is_rejected() -> None:
    @task(task_ref="dup")
    def first(ctx):
        return 1

    with pytest.raises(SpecError, match="already registered"):

        @task(task_ref="dup")
        def second(ctx):
            return 2


def test_workflow_level_fields_are_emitted() -> None:
    @task(task_ref="only")
    def only(ctx):
        return 1

    @workflow(
        name="full",
        namespace="prod",
        description="d",
        run_timeout="6h",
        fail_fast=True,
        max_active_runs=2,
        priority=50,
        labels={"team": "data"},
        params={"sources": {"type": "json", "required": True}},
    )
    def wf():
        return only()

    spec = wf.compile().spec
    assert spec["api_version"] == "nexus.dev/v1"
    assert spec["namespace"] == "prod"
    assert spec["run_timeout_ms"] == 21_600_000
    assert spec["fail_fast"] is True
    assert spec["max_active_runs"] == 2
    assert spec["priority"] == 50
    assert spec["labels"] == {"team": "data"}
    assert spec["params"] == {"sources": {"type": "json", "required": True}}


def test_with_overrides_reuses_one_handler_at_different_sizes() -> None:
    @task(task_ref="sized", resources=Resources(cpu="1", memory="1Gi"))
    def sized(ctx, x):
        return x

    big = sized.with_overrides(task_ref="sized_big", resources=Resources(cpu="8", memory="32Gi"))

    @workflow(name="sizes")
    def wf():
        return big(sized(1))

    tasks = {t["id"]: t for t in wf.compile().spec["tasks"]}
    assert tasks["sized"]["resources"]["request"]["cpu_millis"] == 1000
    assert tasks["sized_2"]["resources"]["request"]["cpu_millis"] == 8000


def test_compiled_json_matches_the_hashed_bytes() -> None:
    from nexus.canonical import spec_hash

    compiled = _linear_workflow().compile()
    assert json.loads(compiled.json) == compiled.spec
    assert spec_hash(compiled.spec) == compiled.hash
    # Pretty-printing is for humans and is NOT what gets hashed.
    assert compiled.to_json(indent=2) != compiled.json
    assert compiled.to_json() == compiled.json


def test_direct_call_outside_a_workflow_runs_the_handler() -> None:
    # Makes a handler unit-testable without the platform.
    @task(task_ref="plain")
    def plain(ctx, x):
        return x * 2

    assert plain(None, 21) == 42
