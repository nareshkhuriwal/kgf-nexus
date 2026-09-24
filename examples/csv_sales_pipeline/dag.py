# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""csv_sales_pipeline -- a partitioned CSV ETL with real data moving between tasks.

Sixteen regions run the same three-stage branch in parallel, then one report
merges them:

    generate_<r> -> validate_<r> -> aggregate_<r> --\\
    ...                                              >-- report
    generate_<r> -> validate_<r> -> aggregate_<r> --/

  * generate   writes a sales CSV for one region (rows_per_region rows, ~1 MB
               at the default), with ~0.5% deliberately corrupt rows.
  * validate   splits clean rows from bad ones and counts each defect.
  * aggregate  revenue, units and orders per product category (awk).
  * report     merges every region and CHECKS ITS OWN ARITHMETIC: every
               generated row must be either aggregated or rejected, or the run
               fails -- a pipeline that silently drops rows is the bug this
               example exists to catch.

What it exercises: a 49-node static DAG, 16-way parallelism, and payloads far
above the 64 KiB inline threshold, so every CSV travels through the result
store (MinIO) rather than Postgres (ADR-0008). Everything is POSIX sh + awk,
so it runs on the stock agent image.

    make submit-example EXAMPLE=csv_sales_pipeline PARAMS="rows_per_region=20000 seed=7"
"""

from __future__ import annotations

from nexus import Resources, Runtime, TaskContext, Timeouts, task, workflow

REGIONS = 16
CATEGORIES = ("electronics", "grocery", "apparel", "home", "toys", "sports", "books", "beauty")

_TASK = Resources(cpu="250m", memory="256Mi")
_TIMEOUT = Timeouts(start_to_close="15m")

# Params arrive as files holding their JSON encoding; strip quotes so a number
# or a string reads the same way in sh.
_READ = 'v() { tr -d \'"\\n \' < "$NEXUS_INPUT_DIR/$1"; }; '


@task(
    task_ref="csv.generate",
    resources=_TASK,
    timeouts=_TIMEOUT,
    inputs={
        "region": {"type": "string", "required": True},
        "rows": {"type": "json", "required": True},
        "seed": {"type": "json", "required": True},
    },
    outputs={"csv": {"type": "string", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + 'awk -v region="$(v region)" -v rows="$(v rows)" -v seed="$(v seed)" \'\n'
            "BEGIN {\n"
            '  split("' + " ".join(CATEGORIES) + '", cat, " ");\n'
            # A stable seed per region: same params, same data, every run, and
            # DIFFERENT data per region. (It once summed only character
            # positions, so every same-length name got the same seed and all
            # 16 regions were identical -- the report's totals were all
            # multiples of 16, which is how it was caught.)
            '  n = region; gsub(/[^0-9]/, "", n); s = seed * 100003 + n * 7919;\n'
            "  srand(s);\n"
            '  print "order_id,region,category,product,qty,unit_price,order_date";\n'
            "  for (i = 1; i <= rows; i++) {\n"
            "    c = cat[int(rand() * 8) + 1];\n"
            '    p = c "-sku-" int(rand() * 25);\n'
            "    q = int(rand() * 9) + 1;\n"
            '    pr = sprintf("%.2f", 2 + rand() * 198);\n'
            '    d = sprintf("2026-%02d-%02d", int(rand() * 12) + 1, int(rand() * 28) + 1);\n'
            "    r = rand();\n"
            # ~0.5% corrupt rows of two kinds, which validate must catch.
            '    if (r < 0.0025) q = "";\n'
            '    else if (r < 0.005) pr = "-" pr;\n'
            '    printf "%s-%07d,%s,%s,%s,%s,%s,%s\\n", region, i, region, c, p, q, pr, d;\n'
            "  }\n"
            "}' > \"$NEXUS_OUTPUT_DIR/csv\""
        ],
    ),
)
def generate(ctx: TaskContext, region: str, rows: int, seed: int) -> str:
    """Synthesize one region's sales CSV."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="csv.validate",
    resources=_TASK,
    timeouts=_TIMEOUT,
    inputs={"csv": {"type": "string", "required": True}},
    outputs={
        "clean": {"type": "string", "required": True},
        "quality": {"type": "json", "required": True},
    },
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            "awk -F, -v out=\"$NEXUS_OUTPUT_DIR/clean\" '\n"
            "NR == 1 { print > out; next }\n"
            "{ total++ }\n"
            'NF != 7              { bad++; reason["columns"]++;   next }\n'
            '$5 !~ /^[0-9]+$/     { bad++; reason["qty"]++;       next }\n'
            '$6 !~ /^[0-9.]+$/    { bad++; reason["price"]++;     next }\n'
            "{ print > out; good++ }\n"
            "END {\n"
            '  printf "{\\"rows\\":%d,\\"clean\\":%d,\\"rejected\\":%d,\\"bad_qty\\":%d,\\"bad_price\\":%d,\\"bad_columns\\":%d}",\n'
            '    total, good, bad, reason["qty"], reason["price"], reason["columns"];\n'
            "}' \"$NEXUS_INPUT_DIR/csv\" > \"$NEXUS_OUTPUT_DIR/quality\""
        ],
    ),
)
def validate(ctx: TaskContext, csv: str) -> dict:
    """Split clean rows from defective ones and count each defect."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="csv.aggregate",
    resources=_TASK,
    timeouts=_TIMEOUT,
    inputs={"clean": {"type": "string", "required": True}},
    outputs={"summary": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        # Valid JSON with ONE category object per line in a fixed field order,
        # so the report can read it with awk instead of a JSON parser.
        args=[
            "awk -F, 'NR > 1 { rev[$3] += $5 * $6; units[$3] += $5; orders[$3]++; region = $2; n++ }\n"
            "END {\n"
            '  printf "{\\"region\\":\\"%s\\",\\"rows\\":%d,\\"categories\\":[\\n", region, n;\n'
            "  sep = \"\";\n"
            "  for (c in rev) {\n"
            '    printf "%s{\\"category\\":\\"%s\\",\\"revenue\\":%.2f,\\"units\\":%d,\\"orders\\":%d}", sep, c, rev[c], units[c], orders[c];\n'
            '    sep = ",\\n";\n'
            "  }\n"
            '  printf "\\n]}\\n";\n'
            "}' \"$NEXUS_INPUT_DIR/clean\" > \"$NEXUS_OUTPUT_DIR/summary\""
        ],
    ),
)
def aggregate(ctx: TaskContext, clean: str) -> dict:
    """Revenue, units and orders per category for one region."""
    raise NotImplementedError("runs as a shell workload")


_REPORT_INPUTS = {}
for _i in range(REGIONS):
    _REPORT_INPUTS[f"summary_{_i:02d}"] = {"type": "json", "required": True}
    _REPORT_INPUTS[f"quality_{_i:02d}"] = {"type": "json", "required": True}


@task(
    task_ref="csv.report",
    resources=_TASK,
    timeouts=_TIMEOUT,
    inputs=_REPORT_INPUTS,
    outputs={"report": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        # quality_* is one JSON object with a fixed key order; summary_* has one
        # category per line. Splitting on JSON punctuation turns both into
        # fields awk can sum. Then the row accounting is CHECKED, not assumed.
        args=[
            'cd "$NEXUS_INPUT_DIR" && awk -F\'[{}":,]+\' \'\n'
            'FILENAME ~ /^quality_/ { for (i = 2; i < NF; i += 2) q[$i] += $(i + 1); next }\n'
            '$2 == "category" { rev[$3] += $5; units[$3] += $7; orders[$3] += $9; agg += $9 }\n'
            "END {\n"
            '  if (q["clean"] != agg || q["clean"] + q["rejected"] != q["rows"]) {\n'
            '    printf "ROW ACCOUNTING FAILED: rows=%d clean=%d rejected=%d aggregated=%d\\n", q["rows"], q["clean"], q["rejected"], agg > "/dev/stderr";\n'
            "    exit 1;\n"
            "  }\n"
            "  for (c in rev) { total += rev[c]; if (rev[c] > best) { best = rev[c]; top = c } }\n"
            '  printf "{\\"regions\\":%d,\\"rows\\":%d,\\"clean\\":%d,\\"rejected\\":%d,\\"reject_rate_pct\\":%.3f,\\"categories\\":{", '
            + str(REGIONS)
            + ', q["rows"], q["clean"], q["rejected"], 100 * q["rejected"] / q["rows"];\n'
            "  sep = \"\";\n"
            '  for (c in rev) { printf "%s\\"%s\\":{\\"revenue\\":%.2f,\\"units\\":%d,\\"orders\\":%d}", sep, c, rev[c], units[c], orders[c]; sep = "," }\n'
            '  printf "},\\"total_revenue\\":%.2f,\\"top_category\\":\\"%s\\",\\"row_accounting\\":\\"ok\\"}\\n", total, top;\n'
            "}\' quality_* summary_* > \"$NEXUS_OUTPUT_DIR/report\" && cat \"$NEXUS_OUTPUT_DIR/report\""
        ],
    ),
)
def report(ctx: TaskContext, **parts: dict) -> dict:
    """Merge every region; fail if any row is unaccounted for."""
    raise NotImplementedError("runs as a shell workload")


@workflow(
    name="csv-sales-pipeline",
    description="16-region CSV ETL: generate, validate, aggregate per region, then a checked report.",
    params={
        "rows_per_region": {"type": "int", "required": False, "default": 20000},
        "seed": {"type": "int", "required": False, "default": 7},
    },
    run_timeout="1h",
)
def csv_sales_pipeline(rows_per_region: int, seed: int):
    parts = {}
    for i in range(REGIONS):
        region = f"region-{i:02d}"
        raw = generate(region=region, rows=rows_per_region, seed=seed, node_id=f"generate_{i:02d}")
        checked = validate(csv=raw["csv"], node_id=f"validate_{i:02d}")
        summary = aggregate(clean=checked["clean"], node_id=f"aggregate_{i:02d}")
        parts[f"summary_{i:02d}"] = summary["summary"]
        parts[f"quality_{i:02d}"] = checked["quality"]
    return report(**parts)


if __name__ == "__main__":
    print(csv_sales_pipeline.compile().to_yaml())
