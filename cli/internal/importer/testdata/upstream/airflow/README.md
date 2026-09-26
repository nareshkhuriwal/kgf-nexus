# Vendored Apache Airflow example DAGs (test data)

These files are **unmodified, byte-for-byte copies** of example DAGs from the
Apache Airflow repository. They are test inputs for `nexusctl import airflow`
only: they are not compiled into, linked into or shipped with any Nexus binary,
and nothing in Nexus imports or executes them (the importer parses them with
Python's `ast`).

- Source: https://github.com/apache/airflow
- Tag: `3.3.2` (annotated tag object `c6debda5cb5e34ad4ba67fe64ccd188a60020108`)
- Downloaded: 2026-09-25 from `https://raw.githubusercontent.com/apache/airflow/3.3.2/<path>`
- License: Apache License 2.0 (checked against the repository's `LICENSE` at that tag).
  Each file keeps its original ASF license header.

| File | Upstream path | sha256 |
|---|---|---|
| example_bash_operator.py | providers/standard/src/airflow/providers/standard/example_dags/example_bash_operator.py | 896ceb94c97bb9f436bb33c289da401e516d8b838f9becd78ac9083ad75b139c |
| example_dynamic_task_mapping.py | airflow-core/src/airflow/example_dags/example_dynamic_task_mapping.py | 3ab9a7df6bcf754c311a728b8f55c5c37903fd0611d3becae728b41550505767 |
| example_kubernetes.py | providers/cncf/kubernetes/tests/system/cncf/kubernetes/example_kubernetes.py | 2c50eb55dc0136edddb7cd7d8518db3319192d6060c6d0868051ad3ff844e071 |
| example_simplest_dag.py | airflow-core/src/airflow/example_dags/example_simplest_dag.py | bcb9dba755488937592c94ea980860b970883d924918edf485bd6ba11e2d991e |
| example_task_group.py | airflow-core/src/airflow/example_dags/example_task_group.py | ffbc3338453a4c5ea30b3ee4fbd421311bd7456499364bb7376cd3c196e52926 |
| example_xcomargs.py | airflow-core/src/airflow/example_dags/example_xcomargs.py | cd9a1343b044fed93c513e2e2a171a01220c649953fa2138d665c1f184147265 |
| tutorial.py | airflow-core/src/airflow/example_dags/tutorial.py | 42dbd63bfb402a0b1443a5dbe22b3875aa4a7136cad6779896e23eeee5e8ec8f |
| tutorial_taskflow_api.py | airflow-core/src/airflow/example_dags/tutorial_taskflow_api.py | 605c1b3bea5953ee147d24ac05eae9ff76d30f7e209ebf1e3ad0e6d5472fea6a |

To re-verify, download each upstream path at the tag and compare the sha256.

## Upstream NOTICE (reproduced as Apache-2.0 section 4(d) requires)

```
Apache Airflow
Copyright 2016-2026 The Apache Software Foundation

This product includes software developed at
The Apache Software Foundation (http://www.apache.org/).
```
