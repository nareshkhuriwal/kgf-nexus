# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""image_processing_gpu -- an image pipeline whose heavy stages run on GPU workers.

Eight shards of images flow through two GPU stages, then CPU fan-in steps:

    acquire_<s> (CPU) -> enhance_<s> (GPU: A10) -> detect_<s> (GPU: A100) --\\
    ...                                                                       >-> contact_sheet (CPU)
    acquire_<s> (CPU) -> enhance_<s> (GPU: A10) -> detect_<s> (GPU: A100) --/  >-> quality_report (CPU, gate)

  * acquire   synthesizes `images` grayscale PGM images per shard: a lit
              gradient background, sensor noise, and 3-9 bright circles and
              rectangles. The TRUE object count is written in each header.
  * enhance   Gaussian denoising (repeated 3x3 blur) and a percentile
              contrast stretch. Scheduled on an A10.
  * detect    Sobel edge magnitude, an Otsu threshold, and connected-component
              labelling to COUNT the objects. Scheduled on an A100.
  * contact_sheet  every edge map as a thumbnail in one viewable PGM image.
  * quality_report compares detected with true counts and FAILS the run if
              detection accuracy is below min_accuracy.

The 16 GPU tasks contend for one A10 and two A100s, so they queue; each
records the device (CUDA_VISIBLE_DEVICES) and worker it ran on.

On the laptop dev cluster the GPUs are SIMULATED: the scheduler reserves real
GPU slots and assigns real device indices, but the pixel math below runs on the
CPU (sh + awk, so it needs nothing beyond the stock agent). On real GPU nodes
the same DAG would swap each stage's runtime for a CUDA/OpenCV container.

    make submit-example EXAMPLE=image_processing_gpu PARAMS="images=4 size=384 denoise_passes=2"
    examples/image_processing_gpu/fetch-contact-sheet.sh      # view the result (dev cluster)
"""

from __future__ import annotations

from nexus import Gpu, Resources, Runtime, TaskContext, Timeouts, task, workflow

SHARDS = 8

_READ = 'v() { tr -d \'"\\n \' < "$NEXUS_INPUT_DIR/$1"; }; '


def _awk(program: str, *, vars: str = "", files: str = "", out: str = "") -> str:
    assert "'" not in program, "awk programs here must not contain single quotes"
    cmd = f"awk {vars} '{program}' {files}"
    return f"{cmd} > {out}" if out else cmd


# awk: read a bundle of P2 images (one pixel row per line) and call process()
# once per image with the pixels in P[x, y], size in W, HH, header in META.
_READER = r'''
$1 == "P2" { st = 1; META = ""; next }
st == 1 && /^#/ { META = substr($0, 3); next }
st == 1 { W = $1; HH = $2; st = 2; next }
st == 2 { st = 3; y = 0; next }
st == 3 { for (x = 1; x <= NF; x++) P[x - 1, y] = $x + 0; y++; if (y == HH) { NIMG++; process(); st = 0 } }
'''

# awk: write image Q (W x HH) as P2 with header comment `meta`.
_WRITER = r'''
function emit(Q, meta,   x, y, line) {
  print "P2"; print "# " meta; print W, HH; print 255
  for (y = 0; y < HH; y++) { line = ""; for (x = 0; x < W; x++) line = line (x ? " " : "") int(Q[x, y] + 0.5); print line }
}
function clamp(v) { return v < 0 ? 0 : (v > 255 ? 255 : v) }
'''

# sh: where this GPU task ran, as JSON fields (appended to its stats).
_PLACEMENT = 'place="\\"worker\\":\\"$HOSTNAME\\",\\"devices\\":\\"${CUDA_VISIBLE_DEVICES:-}\\""; '


@task(
    task_ref="img.acquire",
    resources=Resources(cpu="250m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="15m"),
    inputs={
        "shard": {"type": "json", "required": True},
        "images": {"type": "json", "required": True},
        "size": {"type": "json", "required": True},
        "seed": {"type": "json", "required": True},
    },
    outputs={"raw": {"type": "string", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + _awk(
                _WRITER + r'''
BEGIN { srand(seed * 1000 + shard * 17); W = size; HH = size
  for (im = 1; im <= images; im++) {
    nobj = 3 + int(rand() * 7); placed = 0; tries = 0
    while (placed < nobj && tries < 500) { tries++
      r = int(size * (0.035 + rand() * 0.055)); cx = r + 4 + int(rand() * (size - 2 * r - 8)); cy = r + 4 + int(rand() * (size - 2 * r - 8))
      ok = 1; for (k = 1; k <= placed; k++) { dx = cx - OX[k]; dy = cy - OY[k]; if (sqrt(dx * dx + dy * dy) < r + OR[k] + 10) { ok = 0; break } }
      if (ok) { placed++; OX[placed] = cx; OY[placed] = cy; OR[placed] = r; OS[placed] = (rand() < 0.5); OI[placed] = 200 + int(rand() * 40) } }
    for (y = 0; y < HH; y++) for (x = 0; x < W; x++) {
      v = 40 + 70 * x / W + 20 * y / HH
      for (k = 1; k <= placed; k++) { dx = x - OX[k]; dy = y - OY[k]
        if (OS[k] ? (dx * dx + dy * dy <= OR[k] * OR[k]) : (dx >= -OR[k] && dx <= OR[k] && dy >= -OR[k] * 0.7 && dy <= OR[k] * 0.7)) { v = OI[k]; break } }
      Q[x, y] = clamp(v + (rand() - 0.5) * 60) }
    emit(Q, sprintf("id=s%02d-i%02d objects=%d", shard, im, placed)) } }''',
                vars='-v shard="$(v shard)" -v images="$(v images)" -v size="$(v size)" -v seed="$(v seed)"',
                out='"$NEXUS_OUTPUT_DIR/raw"',
            )
        ],
    ),
)
def acquire(ctx: TaskContext, shard: int, images: int, size: int, seed: int) -> str:
    """Synthesize one shard of noisy images with known object counts."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="img.enhance",
    resources=Resources(cpu="1", memory="1Gi", gpu=Gpu(count=1, kind="nvidia-a10", memory="20Gi")),
    queue="gpu",
    timeouts=Timeouts(start_to_close="30m", schedule_to_start="1h"),
    inputs={"raw": {"type": "string", "required": True}, "passes": {"type": "json", "required": True}},
    outputs={"enhanced": {"type": "string", "required": True}, "stats": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + _PLACEMENT
            + _awk(
                _WRITER + r'''
function process(   p, x, y, dx, dy, s, n, h, c, i, m0, m1, v0, v1, lo, hi) {
  for (y = 0; y < HH; y++) for (x = 0; x < W; x++) { m0 += P[x, y]; v0 += P[x, y] * P[x, y] }
  for (p = 1; p <= passes; p++) {
    for (y = 0; y < HH; y++) for (x = 0; x < W; x++) { s = 0; n = 0
      for (dy = -1; dy <= 1; dy++) for (dx = -1; dx <= 1; dx++) {
        if (x + dx < 0 || x + dx >= W || y + dy < 0 || y + dy >= HH) continue
        wgt = (dx == 0 && dy == 0) ? 4 : ((dx == 0 || dy == 0) ? 2 : 1); s += wgt * P[x + dx, y + dy]; n += wgt }
      B[x, y] = s / n }
    for (y = 0; y < HH; y++) for (x = 0; x < W; x++) P[x, y] = B[x, y] }
  # Percentile contrast stretch: the 1st..99th percentile onto 0..255.
  # Deliberately NOT histogram equalization, which spreads the lit background
  # across the whole range so a global threshold splits the BACKGROUND and
  # counts its bright half as objects. A stretch keeps the two classes apart.
  for (i = 0; i < 256; i++) h[i] = 0
  for (y = 0; y < HH; y++) for (x = 0; x < W; x++) h[int(P[x, y])]++
  c = 0; lo = -1; hi = 255
  for (i = 0; i < 256; i++) { c += h[i]; if (lo < 0 && c >= 0.01 * W * HH) lo = i; if (c <= 0.99 * W * HH) hi = i }
  if (hi <= lo) hi = lo + 1
  for (y = 0; y < HH; y++) for (x = 0; x < W; x++) { Q[x, y] = clamp((P[x, y] - lo) * 255 / (hi - lo)); m1 += Q[x, y]; v1 += Q[x, y] * Q[x, y] }
  emit(Q, META " enhanced=1")
  m0 /= W * HH; m1 /= W * HH
  ST = ST sprintf("%s{\"image\":%d,\"mean_before\":%.1f,\"std_before\":%.1f,\"mean_after\":%.1f,\"std_after\":%.1f}", (NIMG > 1 ? "," : ""), NIMG, m0, sqrt(v0 / (W * HH) - m0 * m0), m1, sqrt(v1 / (W * HH) - m1 * m1))
}
''' + _READER + r'''
END { printf "{\"stage\":\"enhance\",\"images\":%d,%s,\"per_image\":[%s]}", NIMG, place, ST > stats }''',
                vars='-v passes="$(v passes)" -v place="$place" -v stats="$NEXUS_OUTPUT_DIR/stats"',
                files='"$NEXUS_INPUT_DIR/raw"',
                out='"$NEXUS_OUTPUT_DIR/enhanced"',
            )
        ],
    ),
)
def enhance(ctx: TaskContext, raw: str, passes: int) -> dict:
    """Denoise and equalize on a GPU worker (A10)."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="img.detect",
    resources=Resources(cpu="1", memory="1Gi", gpu=Gpu(count=1, kind="nvidia-a100", memory="40Gi")),
    queue="gpu",
    timeouts=Timeouts(start_to_close="30m", schedule_to_start="1h"),
    inputs={"enhanced": {"type": "string", "required": True}},
    outputs={"edges": {"type": "string", "required": True}, "detections": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _PLACEMENT
            + _awk(
                _WRITER + r'''
function process(   x, y, gx, gy, m, i, h, tot, sumall, wb, sb, best, t, mb, mf, bw, thr, ne, comps, sp, cx, cy, area, id, truth) {
  # Sobel gradient magnitude -> the edge map.
  ne = 0
  for (y = 0; y < HH; y++) for (x = 0; x < W; x++) {
    if (x == 0 || y == 0 || x == W - 1 || y == HH - 1) { E[x, y] = 0; continue }
    gx = P[x + 1, y - 1] + 2 * P[x + 1, y] + P[x + 1, y + 1] - P[x - 1, y - 1] - 2 * P[x - 1, y] - P[x - 1, y + 1]
    gy = P[x - 1, y + 1] + 2 * P[x, y + 1] + P[x + 1, y + 1] - P[x - 1, y - 1] - 2 * P[x, y - 1] - P[x + 1, y - 1]
    m = sqrt(gx * gx + gy * gy) / 4; E[x, y] = clamp(m); if (m > 60) ne++ }
  emit(E, META " edges=sobel")
  # Otsu: the intensity threshold that best separates two classes.
  for (i = 0; i < 256; i++) h[i] = 0
  for (y = 0; y < HH; y++) for (x = 0; x < W; x++) h[int(P[x, y])]++
  tot = W * HH; sumall = 0; for (i = 0; i < 256; i++) sumall += i * h[i]
  wb = 0; sb = 0; best = -1; thr = 128
  for (t = 0; t < 256; t++) { wb += h[t]; if (wb == 0) continue; if (wb == tot) break
    sb += t * h[t]; mb = sb / wb; mf = (sumall - sb) / (tot - wb); bw = wb * (tot - wb) * (mb - mf) * (mb - mf)
    if (bw > best) { best = bw; thr = t } }
  # Connected components (4-neighbour) over the bright class, via an explicit stack.
  for (y = 0; y < HH; y++) for (x = 0; x < W; x++) L[x, y] = 0
  comps = 0
  for (y = 0; y < HH; y++) for (x = 0; x < W; x++) {
    if (P[x, y] <= thr || L[x, y]) continue
    id++; area = 0; sp = 1; SX[1] = x; SY[1] = y; L[x, y] = id
    while (sp > 0) { cx = SX[sp]; cy = SY[sp]; sp--; area++
      if (cx > 0 && !L[cx - 1, cy] && P[cx - 1, cy] > thr) { L[cx - 1, cy] = id; sp++; SX[sp] = cx - 1; SY[sp] = cy }
      if (cx < W - 1 && !L[cx + 1, cy] && P[cx + 1, cy] > thr) { L[cx + 1, cy] = id; sp++; SX[sp] = cx + 1; SY[sp] = cy }
      if (cy > 0 && !L[cx, cy - 1] && P[cx, cy - 1] > thr) { L[cx, cy - 1] = id; sp++; SX[sp] = cx; SY[sp] = cy - 1 }
      if (cy < HH - 1 && !L[cx, cy + 1] && P[cx, cy + 1] > thr) { L[cx, cy + 1] = id; sp++; SX[sp] = cx; SY[sp] = cy + 1 } }
    if (area >= minarea) comps++ }
  truth = META; sub(/.*objects=/, "", truth); sub(/ .*/, "", truth)
  tag = META; sub(/^id=/, "", tag); sub(/ .*/, "", tag)
  DT = DT sprintf("%s{\"image\":\"%s\",\"true_objects\":%d,\"detected\":%d,\"otsu_threshold\":%d,\"edge_density\":%.4f}", (NIMG > 1 ? "," : ""), tag, truth, comps, thr, ne / tot)
}
''' + _READER + r'''
END { printf "{\"stage\":\"detect\",\"images\":%d,%s,\"per_image\":[%s]}", NIMG, place, DT > dets }''',
                vars='-v place="$place" -v minarea=60 -v dets="$NEXUS_OUTPUT_DIR/detections"',
                files='"$NEXUS_INPUT_DIR/enhanced"',
                out='"$NEXUS_OUTPUT_DIR/edges"',
            )
        ],
    ),
)
def detect(ctx: TaskContext, enhanced: str) -> dict:
    """Edges, Otsu threshold and object counting on a GPU worker (A100)."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="img.contact_sheet",
    resources=Resources(cpu="500m", memory="512Mi"),
    timeouts=Timeouts(start_to_close="15m"),
    inputs={f"edges_{s:02d}": {"type": "string", "required": True} for s in range(SHARDS)},
    outputs={"contact_sheet": {"type": "string", "required": True}, "sheet_info": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            # Every edge map as a 96x96 thumbnail: one row of thumbnails per shard.
            'cd "$NEXUS_INPUT_DIR" && cat edges_* | '
            + _awk(
                r'''
function process(   x, y, sx, sy, row, col) {
  row = int((NIMG - 1) / cols); col = (NIMG - 1) % cols
  for (y = 0; y < ts; y++) for (x = 0; x < ts; x++) { sx = int(x * W / ts); sy = int(y * HH / ts)
    S[col * (ts + 4) + 2 + x, row * (ts + 4) + 2 + y] = P[sx, sy] }
}
''' + _READER + r'''
END { rows = int((NIMG + cols - 1) / cols); SW = cols * (ts + 4); SH = rows * (ts + 4)
  print "P2"; print "# nexus contact sheet: " NIMG " edge maps"; print SW, SH; print 255
  for (y = 0; y < SH; y++) { line = ""; for (x = 0; x < SW; x++) line = line (x ? " " : "") int(S[x, y] + 0); print line }
  printf "{\"images\":%d,\"columns\":%d,\"width\":%d,\"height\":%d,\"format\":\"PGM (P2)\"}", NIMG, cols, SW, SH > info }''',
                vars='-v ts=96 -v cols="$(grep -c "^P2" edges_00)" -v info="$NEXUS_OUTPUT_DIR/sheet_info"',
                out='"$NEXUS_OUTPUT_DIR/contact_sheet"',
            )
        ],
    ),
)
def contact_sheet(ctx: TaskContext, **edges: str) -> dict:
    """Assemble every edge map into one viewable image."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="img.quality_report",
    resources=Resources(cpu="100m", memory="64Mi"),
    timeouts=Timeouts(start_to_close="5m"),
    inputs={
        **{f"detections_{s:02d}": {"type": "json", "required": True} for s in range(SHARDS)},
        **{f"stats_{s:02d}": {"type": "json", "required": True} for s in range(SHARDS)},
        "min_accuracy": {"type": "json", "required": True},
    },
    outputs={"report": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + 'cd "$NEXUS_INPUT_DIR" && cat detections_* stats_* | sed \'s/}{/}\\n{/g; s/},{/}\\n{/g\' | '
            + _awk(
                r'''
/"true_objects"/ { n++; t = $0; sub(/.*"true_objects":/, "", t); t += 0; d = $0; sub(/.*"detected":/, "", d); d += 0
  if (t == d) hit++; err += (t > d ? t - d : d - t); objs += t }
/"worker"/ { w = $0; sub(/.*"worker":"/, "", w); sub(/".*/, "", w); dev = $0; sub(/.*"devices":"/, "", dev); sub(/".*/, "", dev)
  st = ($0 ~ /"stage":"enhance"/) ? "enhance" : "detect"; used[st "@" w " gpu" dev]++ }
END { acc = hit / n
  printf "{\"images\":%d,\"objects\":%d,\"exact_count_accuracy\":%.4f,\"mean_abs_count_error\":%.3f,\"min_accuracy\":%s,\"gpu_tasks\":{", n, objs, acc, err / n, min
  for (k in used) printf "%s\"%s\":%d", (s++ ? "," : ""), k, used[k]
  printf "}}\n"
  if (acc < min) { printf "detection accuracy %.3f < %s\n", acc, min > "/dev/stderr"; exit 1 } }''',
                vars='-v min="$(v min_accuracy)"',
                out='"$NEXUS_OUTPUT_DIR/report"',
            )
            + ' && cat "$NEXUS_OUTPUT_DIR/report"'
        ],
    ),
)
def quality_report(ctx: TaskContext, **inputs: dict) -> dict:
    """Detection accuracy against ground truth; fail below min_accuracy."""
    raise NotImplementedError("runs as a shell workload")


@workflow(
    name="image-processing-gpu",
    description="8 shards of images: CPU acquire, GPU (A10) denoise+contrast stretch, GPU (A100) edges+object counting, contact sheet, accuracy gate.",
    params={
        "images": {"type": "int", "required": False, "default": 4},
        "size": {"type": "int", "required": False, "default": 384},
        "denoise_passes": {"type": "int", "required": False, "default": 2},
        "seed": {"type": "int", "required": False, "default": 5},
        "min_accuracy": {"type": "float", "required": False, "default": 0.9},
    },
    run_timeout="3h",
)
def image_processing_gpu(images: int, size: int, denoise_passes: int, seed: int, min_accuracy: float):
    sheets, reports = {}, {}
    for s in range(SHARDS):
        raw = acquire(shard=s, images=images, size=size, seed=seed, node_id=f"acquire_{s:02d}")
        enh = enhance(raw=raw["raw"], passes=denoise_passes, node_id=f"enhance_{s:02d}")
        det = detect(enhanced=enh["enhanced"], node_id=f"detect_{s:02d}")
        sheets[f"edges_{s:02d}"] = det["edges"]
        reports[f"detections_{s:02d}"] = det["detections"]
        reports[f"stats_{s:02d}"] = enh["stats"]
    contact_sheet(**sheets)
    return quality_report(min_accuracy=min_accuracy, **reports)


if __name__ == "__main__":
    print(image_processing_gpu.compile().to_yaml())
