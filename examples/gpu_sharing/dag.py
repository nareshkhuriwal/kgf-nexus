# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""gpu_sharing -- MIG slices and VRAM packing on shared GPUs, with an audit that can fail.

Thirteen jobs compete for fractional GPUs (ADR-0029):

  * 4 x big-infer    a quarter of one shared GPU and 18 GiB of its VRAM
  * 4 x small-infer  a fifth of one shared GPU and 10 GiB of its VRAM
  * 3 x mig-small    one MIG 1g.10gb instance
  * 2 x mig-large    one MIG 3g.40gb instance (there is only one: they queue)

The dev fleet's agent-gpu has two 40 GiB shared devices and a partitioned A100
(3 x 1g.10gb, 1 x 3g.40gb), all SIMULATED. Each job records where it ran, the
devices it was given and its VRAM slice. The final `audit` FAILS the run if:

  * the VRAM reserved on one shared device ever exceeded 40 GiB,
  * two jobs overlapping in time were handed the same MIG instance,
  * more instances of a MIG profile were in use at once than exist,
  * nothing ever packed (no two jobs shared a device at the same time), which
    would mean the scheduler fell back to one-job-per-GPU.

On real MIG/MPS hardware the same workflow runs unchanged against the real
devices (OPERATIONS §28.8 lists what that qualifies).

    make submit-example EXAMPLE=gpu_sharing PARAMS="hold_seconds=10"
"""

from __future__ import annotations

from nexus import Gpu, Resources, Runtime, TaskContext, Timeouts, task, workflow

_READ = 'v() { tr -d \'"\\n \' < "$NEXUS_INPUT_DIR/$1"; }; '

_JOB = (
    _READ
    + 'start=$(date +%s); sleep "$(v hold)"; end=$(date +%s); '
    + 'printf \'{"node":"%s","mode":"%s","mem":%s,"dev":"%s","host":"%s","start":%s,"end":%s}\' '
    + '"$NEXUS_NODE_ID" "${NEXUS_GPU_MODE:-none}" "${NEXUS_GPU_SHARE_MEMORY_BYTES:-0}" '
    + '"${CUDA_VISIBLE_DEVICES:-}" "$HOSTNAME" "$start" "$end" > "$NEXUS_OUTPUT_DIR/placement"; '
    + 'cat "$NEXUS_OUTPUT_DIR/placement"'
)

_INPUTS = {"hold": {"type": "json", "required": True}}
_OUTPUTS = {"placement": {"type": "json", "required": True}}


def _job(ref: str, gpu: Gpu):
    @task(
        task_ref=ref,
        resources=Resources(cpu="100m", memory="64Mi", gpu=gpu),
        queue="gpu",
        timeouts=Timeouts(start_to_close="30m", schedule_to_start="30m"),
        inputs=_INPUTS,
        outputs=_OUTPUTS,
        runtime=Runtime(executor="subprocess", command=["sh", "-c"], args=[_JOB]),
    )
    def job(ctx: TaskContext, hold: int) -> dict:
        raise NotImplementedError("runs as a shell workload")

    return job


big = _job("gpu.share_big", Gpu(count=0, share=0.25, share_memory="18Gi"))
small = _job("gpu.share_small", Gpu(count=0, share=0.2, share_memory="10Gi"))
mig_small = _job("gpu.mig_small", Gpu(count=0, mig={"1g.10gb": 1}))
mig_large = _job("gpu.mig_large", Gpu(count=0, mig={"3g.40gb": 1}))

_JOBS = [(f"big_{i}", big) for i in range(4)]
_JOBS += [(f"small_{i}", small) for i in range(4)]
_JOBS += [(f"mig_small_{i}", mig_small) for i in range(3)]
_JOBS += [(f"mig_large_{i}", mig_large) for i in range(2)]

# Capacity the audit checks: per shared device VRAM, per MIG profile count.
_SHARED_CAP = 42949672960  # 40 GiB
_MIG_CAP = "1g.10gb:3 3g.40gb:1"

_AUDIT = (
    'cd "$NEXUS_INPUT_DIR" && cat * | sed \'s/}{/}\\n{/g\' | awk -v cap=' + str(_SHARED_CAP)
    + ' -v migcap="' + _MIG_CAP + '" \'\n'
    "function get(s, k,   m) {\n"
    "  if (match(s, \"\\\"\" k \"\\\":\\\"[^\\\"]*\\\"\")) { m = substr(s, RSTART, RLENGTH); sub(/^\"[^\"]+\":\"/, \"\", m); sub(/\"$/, \"\", m); return m }\n"
    "  if (match(s, \"\\\"\" k \"\\\":[^,}]*\")) { m = substr(s, RSTART, RLENGTH); sub(/^\"[^\"]+\":/, \"\", m); return m }\n"
    "  return \"\" }\n"
    "BEGIN { n = split(migcap, c, \" \"); for (i = 1; i <= n; i++) { split(c[i], p, \":\"); mcap[p[1]] = p[2] } }\n"
    "{ j++; node[j] = get($0, \"node\"); mode[j] = get($0, \"mode\"); mem[j] = get($0, \"mem\") + 0;\n"
    "  dev[j] = get($0, \"dev\"); host[j] = get($0, \"host\"); s[j] = get($0, \"start\") + 0; e[j] = get($0, \"end\") + 0;\n"
    "  prof[j] = (node[j] ~ /^mig_small/) ? \"1g.10gb\" : ((node[j] ~ /^mig_large/) ? \"3g.40gb\" : \"\") }\n"
    "END {\n"
    "  for (a = 1; a <= j; a++) {\n"
    "    if (mode[a] ~ /^shared/) {\n"
    "      used = 0; conc = 0;\n"
    "      for (b = 1; b <= j; b++) if (mode[b] ~ /^shared/ && host[b] == host[a] && dev[b] == dev[a] && s[b] <= s[a] && e[b] > s[a]) { used += mem[b]; conc++ }\n"
    "      if (used > cap) { bad++; why = why sprintf(\"device %s on %s held %d bytes at once (capacity %d); \", dev[a], host[a], used, cap) }\n"
    "      if (conc > packed) packed = conc; if (used > peak) peak = used\n"
    "    } else if (mode[a] == \"mig\") {\n"
    "      inuse = 0;\n"
    "      for (b = 1; b <= j; b++) if (mode[b] == \"mig\" && prof[b] == prof[a] && host[b] == host[a] && s[b] <= s[a] && e[b] > s[a]) inuse++;\n"
    "      if (inuse > mcap[prof[a]]) { bad++; why = why sprintf(\"%d %s instances in use at once on %s (have %d); \", inuse, prof[a], host[a], mcap[prof[a]]) }\n"
    "      for (b = a + 1; b <= j; b++) if (mode[b] == \"mig\" && host[b] == host[a] && s[a] < e[b] && s[b] < e[a] && dev[a] == dev[b]) {\n"
    "        bad++; why = why sprintf(\"%s and %s overlapped on MIG instance %s; \", node[a], node[b], dev[a]) }\n"
    "    } else { bad++; why = why sprintf(\"%s ran with GPU mode %s; \", node[a], mode[a]) }\n"
    "  }\n"
    "  if (packed < 2) { bad++; why = why \"no two slices ever shared a device: nothing was packed; \" }\n"
    '  printf "{\\"jobs\\":%d,\\"violations\\":%d,\\"max_slices_on_one_device\\":%d,\\"peak_device_vram_bytes\\":%d", j, bad, packed, peak;\n'
    '  if (bad) { printf ",\\"problems\\":\\"%s\\"}\\n", why; print "GPU SHARING AUDIT FAILED: " why > "/dev/stderr"; exit 1 }\n'
    '  printf "}\\n" }\' > "$NEXUS_OUTPUT_DIR/audit" && cat "$NEXUS_OUTPUT_DIR/audit"'
)


@task(
    task_ref="gpu.sharing_audit",
    resources=Resources(cpu="100m", memory="64Mi"),
    timeouts=Timeouts(start_to_close="5m"),
    inputs={name: {"type": "json", "required": True} for name, _ in _JOBS},
    outputs={"audit": {"type": "json", "required": True}},
    runtime=Runtime(executor="subprocess", command=["sh", "-c"], args=[_AUDIT]),
)
def audit(ctx: TaskContext, **placements: dict) -> dict:
    """Fail the run on any per-device VRAM or MIG over-allocation, or no packing."""
    raise NotImplementedError("runs as a shell workload")


@workflow(
    name="gpu-sharing",
    description="MIG slices and VRAM-packed shared GPUs; an audit fails the run on any per-device over-allocation.",
    params={"hold_seconds": {"type": "int", "required": False, "default": 10}},
    run_timeout="1h",
)
def gpu_sharing(hold_seconds: int):
    placements = {}
    for name, job in _JOBS:
        placements[name] = job(hold=hold_seconds, node_id=name)["placement"]
    return audit(**placements)


if __name__ == "__main__":
    print(gpu_sharing.compile().to_yaml())
