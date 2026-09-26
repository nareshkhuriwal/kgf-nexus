# Recipes

Task-oriented guides. Each one starts from something runnable in this
repository: an `nexusctl init` template, which is a complete project with tests
and a task image, or an example under `examples/`.

| Recipe | Start from |
| --- | --- |
| [ETL](etl.md) | `nexusctl init etl` |
| [ML training](ml-training.md) | `nexusctl init ml-training`, `examples/ml_training_pipeline`, `examples/credit_risk_ml` |
| [Batch inference](batch-inference.md) | `nexusctl init batch-inference [--gpu]`, `examples/batch_inference` |
| [File fan-out](file-fanout.md) | `nexusctl init file-fanout`, `examples/fan_out_map` |
| [Schedules and backfills](schedules-backfills.md) | any registered workflow |
| [Secrets](secrets.md) | read before putting a credential anywhere near a spec |
| [AI workloads](ai-workloads.md) | `examples/llm_batch_inference`, `examples/llm_eval` |
| [GPU jobs](gpu-jobs.md) | `examples/gpu_scheduling`, `examples/image_processing_gpu` |
| [Kubernetes executor](kubernetes-executor.md) | `examples/kubernetes_executor` |
| [Local development and replay](local-development.md) | `run --local`, `replay` |

All the templates share one layout:

```text
dag.py              tasks and the workflow (resources, retries, timeouts, caching set deliberately)
tests/              pytest for the task functions, with no platform needed
requirements.txt    third-party dependencies of the task image
Dockerfile          the task image: python -m nexus.entrypoint --module dag <task_ref>
.dockerignore       keeps venvs, tests and caches out of the image
README.md           what each task does and why its policies are what they are
```

Build the task image with the SDK from your checkout:

```bash
docker build --build-context nexus-sdk=/path/to/kgf-nexus/sdk/python -t NAME:0.1.0 .
```

Do not `pip install nexus-sdk` from PyPI. That name belongs to an unrelated
project (CONTRACT-FINDINGS F144).
