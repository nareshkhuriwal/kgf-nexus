# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""gpu_scheduling -- GPU placement under contention, with an audit that can fail.

A CPU preprocessing step feeds eight GPU jobs that compete for scarce devices:

  * 4 x finetune   1 x nvidia-a100 each
  * 1 x distributed 2 x nvidia-a100 (needs a whole A100 host)
  * 3 x inference  1 x nvidia-a10 each

The dev fleet has 2 A100s on one agent and 1 A10 on another, so the A100 jobs
MUST queue behind each other and the A10 jobs must run one at a time. Each job
holds its devices for `hold_seconds` and records where it ran, the
CUDA_VISIBLE_DEVICES the agent assigned, and its start/end time.

The final `audit` replays those intervals and FAILS the run if:
  * any worker ever ran more GPUs at once than it advertises (oversubscription),
  * a job ran on a worker whose GPU model is not the one it asked for,
  * a job was handed a different number of devices than it requested,
  * two jobs overlapping in time on one worker were handed the same device.

Each job identifies its worker by what the agent tells every workload:
NEXUS_WORKER_ID, and the GPU model and count the worker ADVERTISES
(NEXUS_WORKER_GPU_KIND, NEXUS_WORKER_GPU_COUNT). The audit used to infer the
model from the hostname (Kubernetes pods named agent-gpu-a10-...), which
compose containers do not have, so every correctly placed job was reported as
misplaced there (F150). A worker that does not report its GPUs (an agent older
than these variables) fails the audit rather than passing it.

On a laptop the GPUs are SIMULATED (the dev agents advertise capacity and are
labelled simulated=true): this tests the scheduler's GPU accounting, queueing
and placement, not CUDA. On real GPU nodes the same workflow runs unchanged and
each job can call nvidia-smi on the devices it was given.

    make submit-example EXAMPLE=gpu_scheduling PARAMS="hold_seconds=15"
"""

from __future__ import annotations

from nexus import Gpu, Resources, Runtime, TaskContext, Timeouts, task, workflow

_READ = 'v() { tr -d \'"\\n \' < "$NEXUS_INPUT_DIR/$1"; }; '

# One job: hold the devices, then report where and when it ran. `date +%s` is
# the agent's clock; the GPU agents share the host's (compose) or node's
# (dev cluster) clock, so intervals are comparable. The worker is the one the
# scheduler placed the job on, as the agent reports it; HOSTNAME is kept only
# as a readable label.
_GPU_JOB = (
    _READ
    + 'start=$(date +%s); '
    + 'sleep "$(v hold)"; '
    + 'end=$(date +%s); '
    + 'devs="${CUDA_VISIBLE_DEVICES:-}"; '
    + 'n=$(printf "%s" "$devs" | awk -F, \'{print ($0 == "") ? 0 : NF}\'); '
    + 'if command -v nvidia-smi >/dev/null 2>&1; then smi=real; else smi=simulated; fi; '
    + 'printf \'{"node":"%s","kind":"%s","requested":%s,"assigned":%s,"devices":"%s",'
    + '"worker":"%s","worker_kind":"%s","worker_gpus":%s,"host":"%s","start":%s,"end":%s,"gpu":"%s"}\' '
    + '"$NEXUS_NODE_ID" "$(v kind)" "$(v count)" "$n" "$devs" '
    + '"${NEXUS_WORKER_ID:-}" "${NEXUS_WORKER_GPU_KIND:-}" "${NEXUS_WORKER_GPU_COUNT:-0}" "$HOSTNAME" '
    + '"$start" "$end" "$smi" '
    + '> "$NEXUS_OUTPUT_DIR/placement"; cat "$NEXUS_OUTPUT_DIR/placement"'
)

_JOB_INPUTS = {
    "features": {"type": "json", "required": True},
    "hold": {"type": "json", "required": True},
    "kind": {"type": "string", "required": True},
    "count": {"type": "json", "required": True},
}
_JOB_OUTPUTS = {"placement": {"type": "json", "required": True}}


@task(
    task_ref="gpu.prepare",
    resources=Resources(cpu="250m", memory="128Mi"),
    timeouts=Timeouts(start_to_close="5m"),
    outputs={"features": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=['printf \'{"batches":64,"shape":[512,768]}\' > "$NEXUS_OUTPUT_DIR/features"'],
    ),
)
def prepare(ctx: TaskContext) -> dict:
    """CPU preprocessing that every GPU job depends on."""
    raise NotImplementedError("runs as a shell workload")


def _gpu_task(ref: str, count: int, kind: str, memory: str):
    @task(
        task_ref=ref,
        resources=Resources(cpu="1", memory="1Gi", gpu=Gpu(count=count, kind=kind, memory=memory)),
        queue="gpu",
        timeouts=Timeouts(start_to_close="30m", schedule_to_start="30m"),
        inputs=_JOB_INPUTS,
        outputs=_JOB_OUTPUTS,
        runtime=Runtime(executor="subprocess", command=["sh", "-c"], args=[_GPU_JOB]),
    )
    def job(ctx: TaskContext, features: dict, hold: int, kind: str, count: int) -> dict:
        raise NotImplementedError("runs as a shell workload")

    return job


finetune = _gpu_task("gpu.finetune", 1, "nvidia-a100", "40Gi")
distributed = _gpu_task("gpu.distributed_train", 2, "nvidia-a100", "40Gi")
inference = _gpu_task("gpu.inference", 1, "nvidia-a10", "20Gi")

_JOBS = [(f"finetune_{i}", finetune, "nvidia-a100", 1) for i in range(4)]
_JOBS += [("distributed_0", distributed, "nvidia-a100", 2)]
_JOBS += [(f"inference_{i}", inference, "nvidia-a10", 1) for i in range(3)]


@task(
    task_ref="gpu.audit",
    resources=Resources(cpu="100m", memory="64Mi"),
    timeouts=Timeouts(start_to_close="5m"),
    inputs={name: {"type": "json", "required": True} for name, *_ in _JOBS},
    outputs={"audit": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            'cd "$NEXUS_INPUT_DIR" && cat * | sed \'s/}{/}\\n{/g\' | awk \'\n'
            # A quoted value is read to its closing quote (devices can be "0,1");
            # a bare one to the next delimiter.
            "function get(s, k,   m) {\n"
            "  if (match(s, \"\\\"\" k \"\\\":\\\"[^\\\"]*\\\"\")) { m = substr(s, RSTART, RLENGTH); sub(/^\"[^\"]+\":\"/, \"\", m); sub(/\"$/, \"\", m); return m }\n"
            "  if (match(s, \"\\\"\" k \"\\\":[^,}]*\")) { m = substr(s, RSTART, RLENGTH); sub(/^\"[^\"]+\":/, \"\", m); return m }\n"
            "  return \"\" }\n"
            "{\n"
            "  j++; node[j] = get($0, \"node\"); kind[j] = get($0, \"kind\"); req[j] = get($0, \"requested\") + 0;\n"
            # Numbers as numbers: get() returns strings, and "100" < "20" as strings.
            "  got[j] = get($0, \"assigned\") + 0; s[j] = get($0, \"start\") + 0; e[j] = get($0, \"end\") + 0;\n"
            "  mode = get($0, \"gpu\"); dev[j] = get($0, \"devices\");\n"
            # The worker as its agent reports it, with the GPUs it advertises.
            "  host[j] = get($0, \"worker\"); wkind = get($0, \"worker_kind\"); wgpus = get($0, \"worker_gpus\") + 0;\n"
            "  label[host[j]] = get($0, \"host\");\n"
            "  if (host[j] == \"\") { bad++; why = why sprintf(\"%s: its agent did not report NEXUS_WORKER_ID (too old for this audit); \", node[j]) }\n"
            # One worker, one advertisement: a worker that reported two
            # different GPU models or counts is itself a finding.
            "  if (host[j] in wk && (wk[host[j]] != wkind || wcap[host[j]] != wgpus)) { bad++; why = why sprintf(\"worker %s reported different GPUs to different jobs; \", host[j]) }\n"
            "  wk[host[j]] = wkind; wcap[host[j]] = wgpus;\n"
            # Placement: the job's worker must advertise the GPU model it asked for.
            "  if (wkind != kind[j]) { bad++; why = why sprintf(\"%s asked for %s but ran on worker %s (%s), which advertises %s; \", node[j], kind[j], host[j], label[host[j]], (wkind == \"\" ? \"no GPUs\" : wkind)) }\n"
            "  if (got[j] != req[j]) { bad++; why = why sprintf(\"%s asked for %s GPUs, was given %s; \", node[j], req[j], got[j]) }\n"
            "}\n"
            "END {\n"
            # Peak concurrent GPUs per host: sweep every start instant.
            "  for (a = 1; a <= j; a++) {\n"
            "    used = 0; for (b = 1; b <= j; b++) if (host[b] == host[a] && s[b] <= s[a] && e[b] > s[a]) used += req[b];\n"
            "    if (used > peak[host[a]]) peak[host[a]] = used;\n"
            "  }\n"
            # Device isolation: jobs overlapping in time on one host must hold
            # DISJOINT devices. Counting GPUs alone passed while two concurrent
            # jobs were both handed device 0.
            "  for (a = 1; a <= j; a++) for (b = a + 1; b <= j; b++)\n"
            "    if (host[a] == host[b] && s[a] < e[b] && s[b] < e[a]) {\n"
            "      na = split(dev[a], da, \",\"); nb = split(dev[b], db, \",\");\n"
            "      for (x = 1; x <= na; x++) for (y = 1; y <= nb; y++) if (da[x] == db[y]) {\n"
            "        bad++; why = why sprintf(\"%s and %s ran at the same time on %s and both used device %s; \", node[a], node[b], host[a], da[x]) }\n"
            "    }\n"
            "  for (h in peak) {\n"
            "    if (peak[h] > wcap[h]) { bad++; why = why sprintf(\"worker %s (%s) ran %d GPUs at once but advertises %d; \", h, label[h], peak[h], wcap[h]) }\n"
            '    peaks = peaks sprintf("%s\\"%s\\":{\\"host\\":\\"%s\\",\\"gpu_kind\\":\\"%s\\",\\"peak_gpus\\":%d,\\"capacity\\":%d}", (peaks == "" ? "" : ","), h, label[h], wk[h], peak[h], wcap[h]);\n'
            "  }\n"
            "  first = s[1]; last = e[1]; for (a = 2; a <= j; a++) { if (s[a] < first) first = s[a]; if (e[a] > last) last = e[a] }\n"
            '  printf "{\\"jobs\\":%d,\\"violations\\":%d,\\"gpu\\":\\"%s\\",\\"makespan_seconds\\":%d,\\"hosts\\":{%s}", j, bad, mode, last - first, peaks;\n'
            '  if (bad) { printf ",\\"problems\\":\\"%s\\"}\\n", why; print "GPU AUDIT FAILED: " why > "/dev/stderr"; exit 1 }\n'
            '  printf "}\\n";\n'
            "}' > \"$NEXUS_OUTPUT_DIR/audit\" && cat \"$NEXUS_OUTPUT_DIR/audit\""
        ],
    ),
)
def audit(ctx: TaskContext, **placements: dict) -> dict:
    """Fail the run on any GPU oversubscription or misplacement."""
    raise NotImplementedError("runs as a shell workload")


@workflow(
    name="gpu-scheduling",
    description="Eight GPU jobs contend for 2x A100 + 1x A10; an audit fails the run on any oversubscription.",
    params={"hold_seconds": {"type": "int", "required": False, "default": 15}},
    run_timeout="1h",
)
def gpu_scheduling(hold_seconds: int):
    features = prepare()
    placements = {}
    for name, job, kind, count in _JOBS:
        placements[name] = job(
            features=features, hold=hold_seconds, kind=kind, count=count, node_id=name
        )["placement"]
    return audit(**placements)


if __name__ == "__main__":
    print(gpu_scheduling.compile().to_yaml())
