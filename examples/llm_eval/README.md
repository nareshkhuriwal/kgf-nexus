# llm_eval — a model evaluation whose quality gate fails the run

```
load --> answer x N (one per batch) --> grade --> verdict
```

50 reference items (40 arithmetic questions, 10 sentiment reviews) answered by
the model under test through `nexus.llm.LLMClient`, then graded three ways:

| Grader | Passes when |
|---|---|
| `exact` | the normalised answer equals the reference |
| `regex` | the item's pattern matches the whole answer |
| `judge` | a judge model, prompted as "a strict grader" with the reference and the candidate, answers `PASS` |

`verdict` computes accuracy per grader and per category and **fails the run**
when any grader is below `min_accuracy` (0.9), when an item was not graded
exactly once, or when the graders disagree on an item (a judge that
rubber-stamps would show up there). With the default `model=mock-large` every
grader scores 1.0; `mock-small` gets a deterministic ~30% of arithmetic wrong
and scores 0.68, so the gate fails.

Every call takes a permit on the shared rate limiter (keys `mock` and
`mock/<model>`), retries 429s in-task, and reports usage under
`tokens:mock/<model>:...`; the judge's usage is recorded under its own model.

## Run it locally (no stack)

```bash
python3 -m venv .venv && ./.venv/bin/pip install -e sdk/python
./.venv/bin/python examples/llm_eval/check_local.py
```

`check_local.py` starts `nexus.testing.MockLLMServer` in-process (a 429 every
9th call) and runs the workflow twice with the SDK's local runner: `mock-large`
must succeed with every grader at 1.0 and exactly one answer and one judgement
per item at the mock; `mock-small` must fail at the gate.

## Run it on a stack

Same requirements as [`llm_batch_inference`](../llm_batch_inference/): an
agent with Python 3 and the SDK, this directory at `/opt/nexus/examples/llm_eval`,
and the mock at `http://mock-llm:8089` (the AI overlay provides them).

```bash
nexusctl workflow submit examples/llm_eval/compiled.yaml
nexusctl run create llm-eval --wait                           # SUCCEEDED
nexusctl run create llm-eval --param model=mock-small --wait  # FAILED at verdict: below min_accuracy
```

For a project to start from (JSONL datasets, configurable graders, real
providers), see `nexusctl init llm-eval`.
