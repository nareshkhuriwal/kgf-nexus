# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""The handler the kubernetes workload-API end-to-end test runs in a pod.

Six "calls", each taking a shared permit on the key ``mock`` and reporting
10 input + 5 output tokens as ``tokens:mock/e2e:*`` (plus one request). The
first attempt checkpoints after three and then fails TRANSIENT; the retry
resumes from that checkpoint, does the other three, checkpoints again and
succeeds. Every one of those calls must go through the agent's network
listener (NEXUS_TASK_API=https://...): the handler refuses to run on the SDK's
in-process fallback, so the test cannot pass by accident.

Each attempt holds for ``HOLD_S`` seconds before it ends, so the test can
exercise the attempt's token from outside while the attempt is live.
"""

from __future__ import annotations

import os
import time

from nexus import TaskContext, task

CALLS = 6
CHECKPOINT_AT = 3
HOLD_S = float(os.environ.get("E2E_HOLD_S", "15"))


# The spec (kubernetes runtime, image, retry on transient) is the test's; the
# decorator only names the handler for nexus.entrypoint.
@task(task_ref="k8s_e2e.work")
def work(ctx: TaskContext) -> dict:
    api = os.environ.get("NEXUS_TASK_API", "")
    if not api.startswith("https://") or not os.environ.get("NEXUS_TASK_API_CA"):
        raise ValueError(f"expected the agent's network workload API, got NEXUS_TASK_API={api!r}")
    state = ctx.resume()
    resumed_from = 0 if state is None else int(state["done"])
    if state is None:
        state = {"done": 0, "attempts": []}
    state["attempts"].append(ctx.attempt_number)
    for i in range(resumed_from, CALLS):
        ctx.acquire_permits(["mock"], requests=1, tokens=15, timeout=60)
        ctx.report_tokens("mock", "e2e", input_tokens=10, output_tokens=5)
        state["done"] = i + 1
        ctx.heartbeat(f"call {i + 1}/{CALLS}")
        if state["done"] == CHECKPOINT_AT and resumed_from == 0:
            ctx.checkpoint(state)
            ctx.log.info("checkpointed; holding, then failing deliberately", done=state["done"])
            time.sleep(HOLD_S)
            raise ConnectionError("deliberate transient failure after the checkpoint")
    ctx.checkpoint(state)
    ctx.log.info("done; holding before success", done=state["done"], resumed_from=resumed_from)
    time.sleep(HOLD_S)
    return {"resumed_from": resumed_from, "attempt": ctx.attempt_number, "done": state["done"],
            "attempts": state["attempts"], "usage": ctx.usage_totals}
