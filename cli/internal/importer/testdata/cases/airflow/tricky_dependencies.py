# Copyright 2026 Khuriwal Group. SPDX-License-Identifier: Apache-2.0
# Hand-written test input for `nexusctl import airflow` (original to this repository). Dependency shapes that are easy to get wrong.
from datetime import timedelta

import pendulum

from airflow.providers.standard.operators.bash import BashOperator
from airflow.providers.standard.operators.empty import EmptyOperator
from airflow.sdk import DAG, Param, TaskGroup, chain, cross_downstream
from airflow.utils.trigger_rule import TriggerRule

default_args = {
    "owner": "data",
    "retries": 2,
    "retry_delay": timedelta(seconds=30),
    "retry_exponential_backoff": True,
    "max_retry_delay": timedelta(minutes=10),
    "execution_timeout": timedelta(minutes=15),
    "depends_on_past": False,
}

with DAG(
    dag_id="Tricky_Dependencies",
    schedule="@daily",
    start_date=pendulum.datetime(2026, 1, 1, tz="Europe/Amsterdam"),
    catchup=True,
    max_active_runs=1,
    default_args=default_args,
    params={"greeting": Param("hello", type="string", minLength=1), "limit": 10},
    tags=["test"],
) as dag:
    a = EmptyOperator(task_id="a")
    b = EmptyOperator(task_id="b")
    c = EmptyOperator(task_id="c")
    d = EmptyOperator(task_id="d")
    e = EmptyOperator(task_id="e")
    f = EmptyOperator(task_id="f")

    # A list on the left, a list on the right: Airflow raises TypeError.
    [a, b] >> [c, d]

    # chain() between equal-length lists is pairwise.
    chain(a, [b, c], [d, e], f)

    # chain() between lists of different lengths is an error in Airflow.
    chain([a, b], [c, d, e])

    # Every pair.
    cross_downstream([a, b], [e, f])

    # << and the explicit methods.
    f << e
    a.set_downstream(e)
    f.set_upstream([c, d])

    greet = BashOperator(
        task_id="greet",
        bash_command='echo "{{ params.greeting }} from {{ dag.dag_id }}/{{ task.task_id }}" && exit 0',
        queue="High-Memory",
        priority_weight=5,
        pool="db_pool",
        max_active_tis_per_dag=2,
        trigger_rule=TriggerRule.ALL_DONE,
    )
    report = BashOperator(
        task_id="report",
        bash_command="echo run {{ run_id }} for {{ ds }}",
        trigger_rule="one_success",
        retries=0,
    )

    with TaskGroup("outer") as outer:
        o1 = EmptyOperator(task_id="first")
        with TaskGroup("inner") as inner:
            i1 = BashOperator(task_id="work", bash_command="echo inner")
            i2 = EmptyOperator(task_id="done")
            i1 >> i2
        o1 >> inner

    f >> greet >> outer >> report
