# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""credit_risk_ml -- an end-to-end ML pipeline that runs for about ten minutes.

A default-risk classifier, built the way a real team would build one:

    ingest -> clean -> features -> plan_experiments
      -> cv_trial (x50: 2 model families x hyperparameters x 5 folds)   [map]
      -> select_config -> train_final -> score_holdout
      -> bootstrap_auc (x30)            [map]  -> confidence_interval
      -> permutation_importance (x16)   [map]  -> importance_report
      -> calibrate
      -> model_card (the gate)

  * ingest       60k synthetic customers with missing values, outliers and
                 categorical columns; default risk is a KNOWN nonlinear
                 function (utilization x debt interaction, late payments,
                 income), so the model has something real to find.
  * clean        median imputation, winsorizing, one-hot encoding.
  * features     standardization, log transform, interaction terms, 5-fold
                 assignment and a 15% holdout the search never sees.
  * cv_trial     logistic regression AND a one-hidden-layer neural network
                 (tanh, SGD with a decaying rate, L2), trained on four folds and
                 scored on the fifth: AUC and log-loss. Memoized.
  * select_config  mean CV AUC per configuration; the leaderboard.
  * train_final  retrains the winner on all training data for longer.
  * bootstrap / permutation  a 95% AUC confidence interval and which
                 features the model actually relies on.
  * calibrate    the decision threshold that minimizes expected cost when a
                 missed default costs five times a false alarm.
  * model_card   APPROVES the model only if the CI lower bound clears min_auc,
                 and FAILS the run otherwise.

The run time comes from real computation (the network training), not sleeps;
`epochs` and `rows` scale it roughly linearly. Measured on a 10-CPU Docker
Desktop cluster: epochs=2 takes ~3.5 min, epochs=8 ~9 min, the default
`epochs=10` ~11 min. Pure sh + awk, so it runs on the stock agent.

    make submit-example EXAMPLE=credit_risk_ml PARAMS="rows=60000 epochs=10 min_auc=0.75"
"""

from __future__ import annotations

from nexus import Cache, Resources, Runtime, TaskContext, Timeouts, task, workflow

FOLDS = 5
BOOTSTRAPS = 30
# Feature columns after engineering (see _FEATURES), used by permutation.
N_FEATURES = 16

_SMALL = Resources(cpu="250m", memory="256Mi")
_TRAIN = Resources(cpu="1", memory="512Mi")

# Read a param/literal input: files hold their JSON encoding.
_READ = 'v() { tr -d \'"\\n \' < "$NEXUS_INPUT_DIR/$1"; }; '


def _awk(program: str, *, vars: str = "", files: str = "", out: str = "") -> str:
    """One awk invocation; the program is single-quoted, so it must not contain '."""
    assert "'" not in program, "awk programs here must not contain single quotes"
    cmd = f"awk {vars} '{program}' {files}"
    return f"{cmd} > {out}" if out else cmd


# awk: numeric field from a one-line JSON object.
_GET = r'''
function get(s, k,   m) {
  if (match(s, "\"" k "\":-?[0-9.eE+-]+")) { m = substr(s, RSTART, RLENGTH); sub(/^"[^"]+":/, "", m); return m + 0 }
  if (match(s, "\"" k "\":\"[^\"]*\"")) { m = substr(s, RSTART, RLENGTH); sub(/^"[^"]+":"/, "", m); sub(/"$/, "", m); return m }
  return "" }
'''

# awk: the two model families, sharing one interface.
#   init(model, hidden, K)   train_row(x[], y)   score(x[]) -> p
# The input vector is x[1..K]. Globals hold the weights.
_MODELS = r'''
function sigm(z) { if (z > 30) return 1; if (z < -30) return 0; return 1 / (1 + exp(-z)) }
function tanh_(z,   e) { if (z > 15) return 1; if (z < -15) return -1; e = exp(2 * z); return (e - 1) / (e + 1) }
function init(m, h, k,   i, j) {
  MODEL = m; H = h; K = k; B2 = 0
  for (i = 1; i <= K; i++) W[i] = 0
  if (MODEL == "mlp") for (j = 1; j <= H; j++) { B1[j] = 0; W2[j] = (rand() - 0.5) * 0.5
    for (i = 1; i <= K; i++) W1[j, i] = (rand() - 0.5) * 2 / sqrt(K) }
}
function score(x,   i, j, z, a) {
  if (MODEL == "logreg") { z = B2; for (i = 1; i <= K; i++) z += W[i] * x[i]; return sigm(z) }
  z = B2
  for (j = 1; j <= H; j++) { a = B1[j]; for (i = 1; i <= K; i++) a += W1[j, i] * x[i]; HID[j] = tanh_(a); z += W2[j] * HID[j] }
  return sigm(z)
}
function train_row(x, y, lr, l2,   p, d, i, j, dh) {
  p = score(x); d = p - y
  if (MODEL == "logreg") { for (i = 1; i <= K; i++) W[i] -= lr * (d * x[i] + l2 * W[i]); B2 -= lr * d; return }
  for (j = 1; j <= H; j++) {
    dh = d * W2[j] * (1 - HID[j] * HID[j])
    W2[j] -= lr * (d * HID[j] + l2 * W2[j])
    for (i = 1; i <= K; i++) W1[j, i] -= lr * (dh * x[i] + l2 * W1[j, i])
    B1[j] -= lr * dh
  }
  B2 -= lr * d
}
function dump(   i, j, s) {
  s = sprintf("{\"model\":\"%s\",\"hidden\":%d,\"K\":%d,\"b2\":%.8g,\"w\":[", MODEL, H, K, B2)
  if (MODEL == "logreg") { for (i = 1; i <= K; i++) s = s (i > 1 ? "," : "") sprintf("%.8g", W[i]) }
  else { for (j = 1; j <= H; j++) { s = s (j > 1 ? "," : "") sprintf("%.8g", W2[j]); for (i = 1; i <= K; i++) s = s "," sprintf("%.8g", W1[j, i]); s = s "," sprintf("%.8g", B1[j]) } }
  return s "]}"
}
function load(js,   body, n, v, i, j, t) {
  MODEL = get(js, "model"); H = get(js, "hidden"); K = get(js, "K"); B2 = get(js, "b2")
  body = js; sub(/^.*"w":\[/, "", body); sub(/\].*$/, "", body); n = split(body, v, ",")
  if (MODEL == "logreg") { for (i = 1; i <= K; i++) W[i] = v[i] + 0; return }
  t = 0; for (j = 1; j <= H; j++) { W2[j] = v[++t] + 0; for (i = 1; i <= K; i++) W1[j, i] = v[++t] + 0; B1[j] = v[++t] + 0 }
}
'''

# Scratch space per task. Many trials run at once in ONE agent pod, so a fixed
# path like /tmp/val would be overwritten by a sibling mid-run.
_TMP = 'T=$(mktemp -d); trap \'rm -rf "$T"\' EXIT; '

# sh: AUC from a file of "label score" lines, by the rank-sum (Mann-Whitney)
# identity. Ties share their average rank.
_AUC = r'''auc() { sort -k2,2g "$1" | awk '
{ lab[NR] = $1; sc[NR] = $2 }
END { n = NR; i = 1
  while (i <= n) { j = i; while (j < n && sc[j + 1] == sc[i]) j++
    r = (i + j) / 2; for (k = i; k <= j; k++) if (lab[k] == 1) { sum += r; pos++ }
    i = j + 1 }
  neg = n - pos; if (pos == 0 || neg == 0) { print 0.5; exit }
  printf "%.6f\n", (sum - pos * (pos + 1) / 2) / (pos * neg) }'; }; '''


# ── stage 1: ingest ──────────────────────────────────────────────────────────

@task(
    task_ref="credit.ingest",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="10m"),
    inputs={"rows": {"type": "json", "required": True}, "seed": {"type": "json", "required": True}},
    outputs={"raw": {"type": "string", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + _awk(
                r'''
BEGIN { srand(seed)
  split("north south east west", REG, " "); split("basic plus premium", PLAN, " ")
  reff["north"] = 0.0; reff["south"] = 0.35; reff["east"] = -0.2; reff["west"] = 0.1
  peff["basic"] = 0.3; peff["plus"] = 0.0; peff["premium"] = -0.4
  print "id,age,income,debt_ratio,credit_lines,late_payments,utilization,region,plan,default"
  for (i = 1; i <= rows; i++) {
    age = 18 + rand() * 62
    u1 = rand(); if (u1 < 1e-12) u1 = 1e-12
    income = exp(10.4 + 0.55 * sqrt(-2 * log(u1)) * cos(6.2831853 * rand()))
    debt = rand() ^ 1.6
    lines = int(rand() * 21)
    u2 = rand(); if (u2 < 1e-12) u2 = 1e-12
    late = int(-log(u2) * 0.9)
    util = rand() * 1.05; if (rand() < 0.01) util = 1.5 + rand() * 3
    reg = REG[int(rand() * 4) + 1]; plan = PLAN[int(rand() * 3) + 1]
    uc = (util > 1.2 ? 1.2 : util)
    z = -2.2 + 1.6 * uc + 2.4 * uc * debt + 0.55 * late - 0.9 * (log(income) - 10.4) - 0.012 * (age - 45) + 0.04 * (lines - 10) + reff[reg] + peff[plan]
    y = (rand() < 1 / (1 + exp(-z))) ? 1 : 0
    sa = sprintf("%.1f", age); si = sprintf("%.0f", income)
    if (rand() < 0.03) si = ""
    if (rand() < 0.01) sa = ""
    printf "c%06d,%s,%s,%.4f,%d,%d,%.4f,%s,%s,%d\n", i, sa, si, debt, lines, late, util, reg, plan, y
  } }''',
                vars='-v rows="$(v rows)" -v seed="$(v seed)"',
                out='"$NEXUS_OUTPUT_DIR/raw"',
            )
        ],
    ),
)
def ingest(ctx: TaskContext, rows: int, seed: int) -> str:
    """Synthesize the raw customer table."""
    raise NotImplementedError("runs as a shell workload")


# ── stage 2: clean ───────────────────────────────────────────────────────────

@task(
    task_ref="credit.clean",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="10m"),
    inputs={"raw": {"type": "string", "required": True}},
    outputs={"clean": {"type": "string", "required": True}, "profile": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            'in="$NEXUS_INPUT_DIR/raw"; '
            # Medians and a 99th percentile need an ordering: sort does that.
            "q() { cut -d, -f\"$1\" \"$in\" | sed 1d | grep -v '^$' | sort -g | awk -v p=\"$2\" '{ a[NR] = $1 } END { print a[int(NR * p) > 0 ? int(NR * p) : 1] }'; }; "
            'med_age=$(q 2 0.5); med_inc=$(q 3 0.5); p99_inc=$(q 3 0.99); '
            + _awk(
                r'''
BEGIN { FS = OFS = "," }
NR == 1 { print "age,income,debt_ratio,credit_lines,late_payments,utilization,r_north,r_south,r_east,r_west,p_basic,p_plus,p_premium,default"; next }
{ n++
  if ($2 == "") { $2 = ma; imp_age++ }
  if ($3 == "") { $3 = mi; imp_inc++ }
  if ($3 > p99) { $3 = p99; clip_inc++ }
  if ($7 > 1.2) { $7 = 1.2; clip_util++ }
  print $2, $3, $4, $5, $6, $7, ($8 == "north"), ($8 == "south"), ($8 == "east"), ($8 == "west"), ($9 == "basic"), ($9 == "plus"), ($9 == "premium"), $10
  pos += $10 }
END { printf "{\"rows\":%d,\"default_rate\":%.4f,\"imputed_age\":%d,\"imputed_income\":%d,\"clipped_income\":%d,\"clipped_utilization\":%d,\"median_age\":%s,\"median_income\":%s,\"p99_income\":%s}", n, pos / n, imp_age, imp_inc, clip_inc, clip_util, ma, mi, p99 > prof }''',
                vars='-v ma="$med_age" -v mi="$med_inc" -v p99="$p99_inc" -v prof="$NEXUS_OUTPUT_DIR/profile"',
                files='"$in"',
                out='"$NEXUS_OUTPUT_DIR/clean"',
            )
        ],
    ),
)
def clean(ctx: TaskContext, raw: str) -> dict:
    """Impute, winsorize and one-hot encode."""
    raise NotImplementedError("runs as a shell workload")


# ── stage 3: features ────────────────────────────────────────────────────────

@task(
    task_ref="credit.features",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="10m"),
    inputs={"clean": {"type": "string", "required": True}, "seed": {"type": "json", "required": True}},
    outputs={
        "train_pool": {"type": "string", "required": True},
        "holdout": {"type": "string", "required": True},
        "schema": {"type": "json", "required": True},
    },
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            # Output rows: y,fold,f1..f16 -- numbers only, so training reads fast.
            + _awk(
                r'''
BEGIN { FS = "," }
NR == 1 { next }
{ n++; R[n] = $0
  v[1] = $1; v[2] = log($2 + 1); v[3] = $3; v[4] = $4; v[5] = $5; v[6] = $6
  v[7] = $3 * $6; v[8] = $5 * $5; v[9] = $6 * $6
  for (i = 1; i <= 9; i++) { s[i] += v[i]; ss[i] += v[i] * v[i]; X[n, i] = v[i] }
  for (i = 7; i <= 13; i++) X[n, i + 3] = $i
  Y[n] = $14 }
END { srand(seed)
  for (i = 1; i <= 9; i++) { mu[i] = s[i] / n; sd[i] = sqrt(ss[i] / n - mu[i] * mu[i]); if (sd[i] < 1e-9) sd[i] = 1 }
  for (r = 1; r <= n; r++) {
    line = ""
    for (i = 1; i <= 9; i++) line = line "," sprintf("%.5f", (X[r, i] - mu[i]) / sd[i])
    for (i = 10; i <= 16; i++) line = line "," X[r, i]
    if (rand() < 0.15) print Y[r] line > (dir "/holdout")
    else print Y[r] "," int(rand() * folds) line > (dir "/train_pool")
  }
  printf "{\"features\":16,\"names\":[\"age\",\"log_income\",\"debt_ratio\",\"credit_lines\",\"late_payments\",\"utilization\",\"debt_x_util\",\"late_sq\",\"util_sq\",\"r_north\",\"r_south\",\"r_east\",\"r_west\",\"p_basic\",\"p_plus\",\"p_premium\"],\"rows\":%d}", n > (dir "/schema") }''',
                vars='-v seed="$(v seed)" -v folds=' + str(FOLDS) + ' -v dir="$NEXUS_OUTPUT_DIR"',
                files='"$NEXUS_INPUT_DIR/clean"',
            )
        ],
    ),
)
def features(ctx: TaskContext, clean: str, seed: int) -> dict:
    """Standardize, engineer interactions, assign folds, split a holdout."""
    raise NotImplementedError("runs as a shell workload")


# ── stage 4: plan the search ─────────────────────────────────────────────────

@task(
    task_ref="credit.plan_experiments",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="5m"),
    inputs={"schema": {"type": "json", "required": True}, "epochs": {"type": "json", "required": True}},
    outputs={"experiments": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + _awk(
                r'''
BEGIN {
  # config id | model | hidden | lr | l2
  n = split("lr_a|logreg|0|0.05|0 lr_b|logreg|0|0.05|0.001 lr_c|logreg|0|0.1|0.0001 lr_d|logreg|0|0.02|0 lr_e|logreg|0|0.1|0.01 lr_f|logreg|0|0.2|0.001 nn_a|mlp|4|0.05|0.0001 nn_b|mlp|8|0.05|0.0001 nn_c|mlp|8|0.1|0.0001 nn_d|mlp|12|0.03|0.0001", C, " ")
  printf "["
  for (c = 1; c <= n; c++) { split(C[c], p, "|")
    for (f = 0; f < folds; f++) {
      printf "%s{\"cfg\":\"%s\",\"model\":\"%s\",\"hidden\":%s,\"lr\":%s,\"l2\":%s,\"epochs\":%d,\"fold\":%d}", (t++ ? "," : ""), p[1], p[2], p[3], p[4], p[5], epochs, f } }
  printf "]" }''',
                vars='-v epochs="$(v epochs)" -v folds=' + str(FOLDS),
                out='"$NEXUS_OUTPUT_DIR/experiments"',
            )
        ],
    ),
)
def plan_experiments(ctx: TaskContext, schema: dict, epochs: int) -> list:
    """Two model families x hyperparameters x folds: 50 trials."""
    raise NotImplementedError("runs as a shell workload")


# ── stage 5: cross-validated search (map) ────────────────────────────────────

@task(
    task_ref="credit.cv_trial",
    resources=_TRAIN,
    timeouts=Timeouts(start_to_close="45m"),
    cache=Cache(enabled=True, ttl="24h"),
    inputs={"experiment": {"type": "json", "required": True}, "train_pool": {"type": "string", "required": True}},
    outputs={"metrics": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _TMP + _AUC
            + 'e=$(tr -d "\\n" < "$NEXUS_INPUT_DIR/experiment"); '
            + _awk(
                _GET + _MODELS + r'''
BEGIN { FS = ","; srand(7 + get(e, "fold"))
  fold = get(e, "fold"); epochs = get(e, "epochs"); lr0 = get(e, "lr"); l2 = get(e, "l2")
  init(get(e, "model"), get(e, "hidden"), 16) }
{ if ($2 == fold) { v++; VY[v] = $1; for (i = 1; i <= 16; i++) VX[v, i] = $(i + 2) }
  else { t++; TY[t] = $1; for (i = 1; i <= 16; i++) TX[t, i] = $(i + 2); ORD[t] = t } }
END {
  for (ep = 1; ep <= epochs; ep++) {
    for (a = t; a > 1; a--) { b = int(rand() * a) + 1; tmp = ORD[a]; ORD[a] = ORD[b]; ORD[b] = tmp }
    lr = lr0 / (1 + 0.15 * (ep - 1))
    for (a = 1; a <= t; a++) { r = ORD[a]; for (i = 1; i <= 16; i++) x[i] = TX[r, i]; train_row(x, TY[r], lr, l2) } }
  for (a = 1; a <= v; a++) { for (i = 1; i <= 16; i++) x[i] = VX[a, i]; p = score(x)
    pc = (p < 1e-7 ? 1e-7 : (p > 1 - 1e-7 ? 1 - 1e-7 : p)); ll -= VY[a] * log(pc) + (1 - VY[a]) * log(1 - pc)
    acc += ((p >= 0.5) == VY[a]); print VY[a], p > tv }
  printf "{\"cfg\":\"%s\",\"model\":\"%s\",\"hidden\":%d,\"lr\":%s,\"l2\":%s,\"fold\":%d,\"train_rows\":%d,\"val_rows\":%d,\"logloss\":%.6f,\"accuracy\":%.6f", get(e, "cfg"), MODEL, H, lr0, l2, fold, t, v, ll / v, acc / v }''',
                vars='-v e="$e" -v tv="$T/val"',
                files='"$NEXUS_INPUT_DIR/train_pool"',
                out='"$T/m"',
            )
            + ' && printf \',"auc":%s}\' "$(auc "$T/val")" >> "$T/m" && cp "$T/m" "$NEXUS_OUTPUT_DIR/metrics" && cat "$T/m"'
        ],
    ),
)
def cv_trial(ctx: TaskContext, experiment: dict, train_pool: str) -> dict:
    """Train one (config, fold) and score it on the held-out fold."""
    raise NotImplementedError("runs as a shell workload")


# ── stage 6: select the configuration ────────────────────────────────────────

@task(
    task_ref="credit.select_config",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="5m"),
    trigger_rule="all_done",
    inputs={"trials": {"type": "json", "required": True}, "epochs": {"type": "json", "required": True}},
    outputs={"selection": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + "tr -d '\\n' < \"$NEXUS_INPUT_DIR/trials\" | sed 's/},/}\\n/g' | "
            + _awk(
                _GET + r'''
/"auc"/ { c = get($0, "cfg"); a = get($0, "auc"); n[c]++; s[c] += a; ss[c] += a * a; ok++
  model[c] = get($0, "model"); hid[c] = get($0, "hidden"); lr[c] = get($0, "lr"); l2[c] = get($0, "l2") }
/null/ { failed++ }
END {
  if (!ok) { print "no trial succeeded" > "/dev/stderr"; exit 1 }
  for (c in n) { m = s[c] / n[c]; sd = sqrt(ss[c] / n[c] - m * m); if (best == "" || m > bm) { bm = m; best = c }
    lb = lb sprintf("%s{\"cfg\":\"%s\",\"model\":\"%s\",\"hidden\":%d,\"mean_auc\":%.5f,\"std_auc\":%.5f,\"folds\":%d}", (lb == "" ? "" : ",\n"), c, model[c], hid[c], m, sd, n[c]) }
  printf "{\"best\":{\"cfg\":\"%s\",\"model\":\"%s\",\"hidden\":%d,\"lr\":%s,\"l2\":%s,\"epochs\":%d,\"mean_cv_auc\":%.5f},\"trials_ok\":%d,\"trials_failed\":%d,\"leaderboard\":[\n%s\n]}\n", best, model[best], hid[best], lr[best], l2[best], epochs, bm, ok, failed, lb }''',
                vars='-v epochs="$(v epochs)"',
                out='"$NEXUS_OUTPUT_DIR/selection"',
            )
            + ' && cat "$NEXUS_OUTPUT_DIR/selection"'
        ],
    ),
)
def select_config(ctx: TaskContext, trials: list, epochs: int) -> dict:
    """Mean CV AUC per configuration; pick the best."""
    raise NotImplementedError("runs as a shell workload")


# ── stage 7: final model ─────────────────────────────────────────────────────

@task(
    task_ref="credit.train_final",
    resources=_TRAIN,
    timeouts=Timeouts(start_to_close="45m"),
    inputs={"selection": {"type": "json", "required": True}, "train_pool": {"type": "string", "required": True}},
    outputs={"model": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            "sel=$(tr -d '\\n' < \"$NEXUS_INPUT_DIR/selection\" | sed 's/.*\"best\":{//; s/}.*//'); "
            + _awk(
                _GET + _MODELS + r'''
BEGIN { FS = ","; srand(99); s = "{" sel "}"
  epochs = int(get(s, "epochs") * 1.5); lr0 = get(s, "lr"); l2 = get(s, "l2")
  init(get(s, "model"), get(s, "hidden"), 16) }
{ t++; TY[t] = $1; for (i = 1; i <= 16; i++) TX[t, i] = $(i + 2); ORD[t] = t }
END {
  for (ep = 1; ep <= epochs; ep++) {
    for (a = t; a > 1; a--) { b = int(rand() * a) + 1; tmp = ORD[a]; ORD[a] = ORD[b]; ORD[b] = tmp }
    lr = lr0 / (1 + 0.15 * (ep - 1))
    for (a = 1; a <= t; a++) { r = ORD[a]; for (i = 1; i <= 16; i++) x[i] = TX[r, i]; train_row(x, TY[r], lr, l2) } }
  print dump() }''',
                vars='-v sel="$sel"',
                files='"$NEXUS_INPUT_DIR/train_pool"',
                out='"$NEXUS_OUTPUT_DIR/model"',
            )
        ],
    ),
)
def train_final(ctx: TaskContext, selection: dict, train_pool: str) -> dict:
    """Retrain the winning configuration on every training row."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="credit.score_holdout",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="10m"),
    inputs={"model": {"type": "json", "required": True}, "holdout": {"type": "string", "required": True}},
    outputs={
        "scores": {"type": "string", "required": True},
        "holdout_metrics": {"type": "json", "required": True},
        "bootstrap_plan": {"type": "json", "required": True},
        "features_plan": {"type": "json", "required": True},
    },
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _AUC
            + 'm=$(tr -d "\\n" < "$NEXUS_INPUT_DIR/model"); '
            + _awk(
                _GET + _MODELS + r'''
BEGIN { FS = ","; load(m) }
{ for (i = 1; i <= 16; i++) x[i] = $(i + 2 - 1); print $1, score(x) }''',
                vars='-v m="$m"',
                files='"$NEXUS_INPUT_DIR/holdout"',
                out='"$NEXUS_OUTPUT_DIR/scores"',
            )
            + ' && a=$(auc "$NEXUS_OUTPUT_DIR/scores") && '
            + _awk(
                r'''
{ n++; pos += $1; p = $2; pc = (p < 1e-7 ? 1e-7 : (p > 1 - 1e-7 ? 1 - 1e-7 : p)); ll -= $1 * log(pc) + (1 - $1) * log(1 - pc) }
END { printf "{\"rows\":%d,\"default_rate\":%.4f,\"auc\":%s,\"logloss\":%.6f}", n, pos / n, a, ll / n }''',
                vars='-v a="$a"',
                files='"$NEXUS_OUTPUT_DIR/scores"',
                out='"$NEXUS_OUTPUT_DIR/holdout_metrics"',
            )
            + " && awk -v b=" + str(BOOTSTRAPS) + ' \'BEGIN { printf "["; for (i = 1; i <= b; i++) printf "%s%d", (i > 1 ? "," : ""), i; printf "]" }\' > "$NEXUS_OUTPUT_DIR/bootstrap_plan"'
            + " && awk -v k=" + str(N_FEATURES) + ' \'BEGIN { printf "["; for (i = 1; i <= k; i++) printf "%s%d", (i > 1 ? "," : ""), i; printf "]" }\' > "$NEXUS_OUTPUT_DIR/features_plan"'
            + ' && cat "$NEXUS_OUTPUT_DIR/holdout_metrics"'
        ],
    ),
)
def score_holdout(ctx: TaskContext, model: dict, holdout: str) -> dict:
    """Score the untouched holdout; plan the bootstrap and permutation fan-outs."""
    raise NotImplementedError("runs as a shell workload")


# ── stage 8: uncertainty and explanation (maps) ──────────────────────────────

@task(
    task_ref="credit.bootstrap_auc",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="10m"),
    inputs={"replicate": {"type": "json", "required": True}, "scores": {"type": "string", "required": True}},
    outputs={"auc": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _TMP + _AUC
            + 'r=$(tr -d " \\n" < "$NEXUS_INPUT_DIR/replicate"); '
            + _awk(
                r'''
{ n++; L[n] = $0 }
END { srand(1000 + r); for (i = 1; i <= n; i++) print L[int(rand() * n) + 1] }''',
                vars='-v r="$r"',
                files='"$NEXUS_INPUT_DIR/scores"',
                out='"$T/bs"',
            )
            + ' && printf \'{"replicate":%s,"auc":%s}\' "$r" "$(auc "$T/bs")" > "$NEXUS_OUTPUT_DIR/auc"'
        ],
    ),
)
def bootstrap_auc(ctx: TaskContext, replicate: int, scores: str) -> dict:
    """AUC on one bootstrap resample of the holdout."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="credit.confidence_interval",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="5m"),
    inputs={"replicates": {"type": "json", "required": True}},
    outputs={"interval": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            "tr -d '\\n' < \"$NEXUS_INPUT_DIR/replicates\" | sed 's/},/}\\n/g' | grep -o '\"auc\":[0-9.]*' | cut -d: -f2 | sort -g | "
            + _awk(
                r'''
{ a[++n] = $1; s += $1 }
END { if (n < 10) { print "too few bootstrap replicates" > "/dev/stderr"; exit 1 }
  lo = a[int(n * 0.025) + 1]; hi = a[int(n * 0.975) > 0 ? int(n * 0.975) : n]
  printf "{\"replicates\":%d,\"mean_auc\":%.5f,\"ci95_low\":%.5f,\"ci95_high\":%.5f}", n, s / n, lo, hi }''',
                out='"$NEXUS_OUTPUT_DIR/interval"',
            )
        ],
    ),
)
def confidence_interval(ctx: TaskContext, replicates: list) -> dict:
    """95% percentile interval over the bootstrap AUCs."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="credit.permutation_importance",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="10m"),
    inputs={
        "feature": {"type": "json", "required": True},
        "model": {"type": "json", "required": True},
        "holdout": {"type": "string", "required": True},
        "holdout_metrics": {"type": "json", "required": True},
    },
    outputs={"importance": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _TMP + _AUC
            + 'f=$(tr -d " \\n" < "$NEXUS_INPUT_DIR/feature"); m=$(tr -d "\\n" < "$NEXUS_INPUT_DIR/model"); '
            + 'base=$(grep -o \'"auc":[0-9.]*\' "$NEXUS_INPUT_DIR/holdout_metrics" | cut -d: -f2); '
            # Shuffle ONE feature column across rows, re-score, measure the AUC drop.
            + _awk(
                _GET + _MODELS + r'''
BEGIN { FS = ","; load(m); srand(500 + f) }
{ n++; Y[n] = $1; for (i = 1; i <= 16; i++) X[n, i] = $(i + 1); P[n] = X[n, f] }
END { for (a = n; a > 1; a--) { b = int(rand() * a) + 1; t = P[a]; P[a] = P[b]; P[b] = t }
  for (r = 1; r <= n; r++) { for (i = 1; i <= 16; i++) x[i] = X[r, i]; x[f] = P[r]; print Y[r], score(x) } }''',
                vars='-v m="$m" -v f="$f"',
                files='"$NEXUS_INPUT_DIR/holdout"',
                out='"$T/perm"',
            )
            + ' && p=$(auc "$T/perm") && awk -v f="$f" -v b="$base" -v p="$p" \'BEGIN { printf "{\\"feature\\":%d,\\"auc_permuted\\":%s,\\"importance\\":%.5f}", f, p, b - p }\' > "$NEXUS_OUTPUT_DIR/importance"'
        ],
    ),
)
def permutation_importance(ctx: TaskContext, feature: int, model: dict, holdout: str, holdout_metrics: dict) -> dict:
    """AUC drop when one feature is scrambled."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="credit.importance_report",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="5m"),
    inputs={"importances": {"type": "json", "required": True}, "schema": {"type": "json", "required": True}},
    outputs={"report": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            "names=$(tr -d '\\n' < \"$NEXUS_INPUT_DIR/schema\" | sed 's/.*\"names\":\\[//; s/\\].*//; s/\"//g'); "
            "tr -d '\\n' < \"$NEXUS_INPUT_DIR/importances\" | sed 's/},/}\\n/g' | "
            + _awk(
                _GET + r'''
BEGIN { split(names, NM, ",") }
/"importance"/ { f = get($0, "feature"); imp[f] = get($0, "importance"); n++ }
END { # rank descending by importance
  for (i = 1; i <= 16; i++) ord[i] = i
  for (i = 1; i <= 16; i++) for (j = i + 1; j <= 16; j++) if (imp[ord[j]] > imp[ord[i]]) { t = ord[i]; ord[i] = ord[j]; ord[j] = t }
  printf "{\"features\":%d,\"ranking\":[", n
  for (i = 1; i <= 16; i++) printf "%s{\"feature\":\"%s\",\"auc_drop\":%.5f}", (i > 1 ? "," : ""), NM[ord[i]], imp[ord[i]]
  printf "]}" }''',
                vars='-v names="$names"',
                out='"$NEXUS_OUTPUT_DIR/report"',
            )
        ],
    ),
)
def importance_report(ctx: TaskContext, importances: list, schema: dict) -> dict:
    """Rank features by how much scrambling them hurts."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="credit.calibrate",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="5m"),
    inputs={"scores": {"type": "string", "required": True}},
    outputs={"threshold": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            # Missing a default costs 5, a false alarm costs 1: pick the cut
            # that minimizes total cost, and report a reliability table.
            _awk(
                r'''
{ n++; Y[n] = $1; S[n] = $2; b = int($2 * 10); if (b > 9) b = 9; bn[b]++; bp[b] += $2; by[b] += $1 }
END {
  for (k = 1; k <= 99; k++) { th = k / 100; fn = fp = tp = 0
    for (i = 1; i <= n; i++) { d = (S[i] >= th); if (d && Y[i]) tp++; else if (d) fp++; else if (Y[i]) fn++ }
    cost = 5 * fn + fp; if (best == "" || cost < bc) { bc = cost; best = th; btp = tp; bfp = fp; bfn = fn } }
  printf "{\"threshold\":%.2f,\"expected_cost_per_1000\":%.1f,\"recall\":%.4f,\"precision\":%.4f,\"reliability\":[", best, 1000 * bc / n, btp / (btp + bfn), (btp + bfp ? btp / (btp + bfp) : 0)
  for (b = 0; b <= 9; b++) if (bn[b]) printf "%s{\"bin\":%d,\"predicted\":%.3f,\"observed\":%.3f,\"n\":%d}", (s++ ? "," : ""), b, bp[b] / bn[b], by[b] / bn[b], bn[b]
  printf "]}" }''',
                files='"$NEXUS_INPUT_DIR/scores"',
                out='"$NEXUS_OUTPUT_DIR/threshold"',
            )
        ],
    ),
)
def calibrate(ctx: TaskContext, scores: str) -> dict:
    """Cost-weighted decision threshold and a reliability table."""
    raise NotImplementedError("runs as a shell workload")


# ── stage 9: the gate ────────────────────────────────────────────────────────

@task(
    task_ref="credit.model_card",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="5m"),
    inputs={
        "selection": {"type": "json", "required": True},
        "holdout_metrics": {"type": "json", "required": True},
        "interval": {"type": "json", "required": True},
        "importance": {"type": "json", "required": True},
        "threshold": {"type": "json", "required": True},
        "profile": {"type": "json", "required": True},
        "model": {"type": "json", "required": True},
        "min_auc": {"type": "json", "required": True},
    },
    outputs={"model_card": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + 'd="$NEXUS_INPUT_DIR"; j() { tr -d "\\n" < "$d/$1"; }; '
            + 'best=$(j selection | sed \'s/.*"best":\\({[^}]*}\\).*/\\1/\'); '
            + 'hash=$(sha256sum "$d/model" | cut -c1-16); '
            + 'lo=$(j interval | grep -o \'"ci95_low":[0-9.]*\' | cut -d: -f2); '
            + 'min=$(v min_auc); '
            + 'status=$(awk -v lo="$lo" -v min="$min" \'BEGIN { print (lo >= min) ? "approved" : "rejected" }\'); '
            + 'top=$(j importance | sed \'s/.*"ranking":\\[//; s/\\].*//\' | sed \'s/},{/}\\n{/g\' | head -n 3 | grep -o \'"feature":"[a-z_]*"\' | cut -d\\" -f4 | tr "\\n" , | sed "s/,$//"); '
            + 'printf \'{"status":"%s","model_sha256_prefix":"%s","min_auc":%s,"selected":%s,"holdout":%s,"auc_ci95":%s,"top_features":"%s","decision":%s,"data":%s}\\n\' '
            + '"$status" "$hash" "$min" "$best" "$(j holdout_metrics)" "$(j interval)" "$top" "$(j threshold)" "$(j profile)" '
            + '> "$NEXUS_OUTPUT_DIR/model_card" && cat "$NEXUS_OUTPUT_DIR/model_card" && '
            + '[ "$status" = approved ] || { echo "model rejected: AUC 95% CI lower bound $lo < $min" >&2; exit 1; }'
        ],
    ),
)
def model_card(ctx: TaskContext, **inputs: dict) -> dict:
    """Approve only if the AUC confidence interval clears the bar."""
    raise NotImplementedError("runs as a shell workload")


@workflow(
    name="credit-risk-ml",
    description="Credit-default model: clean, engineer, 50-trial CV search (logreg + neural net), final fit, bootstrap CI, permutation importance, calibration, gated model card.",
    params={
        "rows": {"type": "int", "required": False, "default": 60000},
        "epochs": {"type": "int", "required": False, "default": 10},
        "seed": {"type": "int", "required": False, "default": 2026},
        "min_auc": {"type": "float", "required": False, "default": 0.75},
    },
    run_timeout="3h",
)
def credit_risk_ml(rows: int, epochs: int, seed: int, min_auc: float):
    raw = ingest(rows=rows, seed=seed)
    cleaned = clean(raw=raw["raw"])
    feats = features(clean=cleaned["clean"], seed=seed)
    plan = plan_experiments(schema=feats["schema"], epochs=epochs)
    trials = cv_trial.map(plan["experiments"], item="experiment", max_fanout=200, train_pool=feats["train_pool"])
    selection = select_config(trials=trials, epochs=epochs)
    model = train_final(selection=selection["selection"], train_pool=feats["train_pool"])
    scored = score_holdout(model=model["model"], holdout=feats["holdout"])
    boots = bootstrap_auc.map(scored["bootstrap_plan"], item="replicate", max_fanout=200, scores=scored["scores"])
    interval = confidence_interval(replicates=boots)
    perms = permutation_importance.map(
        scored["features_plan"], item="feature", max_fanout=64,
        model=model["model"], holdout=feats["holdout"], holdout_metrics=scored["holdout_metrics"],
    )
    importance = importance_report(importances=perms, schema=feats["schema"])
    threshold = calibrate(scores=scored["scores"])
    return model_card(
        selection=selection["selection"], holdout_metrics=scored["holdout_metrics"],
        interval=interval["interval"], importance=importance["report"], threshold=threshold["threshold"],
        profile=cleaned["profile"], model=model["model"], min_auc=min_auc,
    )


if __name__ == "__main__":
    print(credit_risk_ml.compile().to_yaml())
