# ML training

```bash
nexusctl init ml-training tuner
```

```text
load_dataset --+--> train (one child per trial, created at run time) --> select_best
make_grid -----+                                                            |
                                                         evaluate <---------+
                                                            |
                                                       quality_gate
```

The template ([source](repo:cli/internal/templates/files/ml-training/dag.py))
runs a hyperparameter grid as a **map**, selects the best trial, refits it,
scores a held-out set, and fails the run when the score is below `min_r2`.
The model is dependency-free (ridge regression by gradient descent), so it
runs anywhere. Swap `_fit` and `_predict` for scikit-learn, XGBoost or PyTorch;
the DAG does not change.

```python
trials = make_grid(learning_rates=..., l2=...)
results = train.map(trials, item="hparams", max_fanout=MAX_TRIALS, dataset=data)
selection = select_best(results=results)          # trigger_rule=all_done
evaluation = evaluate(selection=selection, dataset=data)
quality_gate(evaluation=evaluation, min_r2=min_r2)
```

## What makes it cheap to iterate

- **Each trial is memoized** on `(task version, inputs)`. Re-running with the
  same data and a grid with two new points runs two trials. The rest are
  `CacheHit`s ([memoization](../concepts/results-artifacts.md#memoization)).
- **Partial failure is tolerated where it should be.** `select_best` uses
  `all_done` and ignores `null` entries from failed trials. `quality_gate` does
  not tolerate failure: a bad model must fail the run, not look finished.
- **The dataset is shared, not copied.** Every trial receives the same blob
  reference.

## Resources

Give `train` the resources one trial needs, not the whole search's. Nexus
places the children across the fleet up to `max_fanout`. Use
`concurrency_key`/`concurrency_limit` to cap how many run at once, for example
against a shared feature store.

## Bigger examples

- [`examples/ml_training_pipeline`](repo:examples/ml_training_pipeline/): a
  24-trial search with real gradient descent and an R² gate. A re-run reuses
  every trial.
- [`examples/credit_risk_ml`](repo:examples/credit_risk_ml/): about 11 minutes
  of real computation. It goes from messy rows through feature engineering and
  a 50-trial cross-validated search to a bootstrap AUC confidence interval and a
  model card that fails the run if the interval's lower bound is below
  `min_auc`.
