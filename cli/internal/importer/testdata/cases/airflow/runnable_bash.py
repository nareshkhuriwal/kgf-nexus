# Copyright 2026 Khuriwal Group. SPDX-License-Identifier: Apache-2.0
# Hand-written test input for `nexusctl import airflow` (original to this repository). Fully translatable, and run for real by TestConvertedPipelinesRunLocally.
from datetime import timedelta

import pendulum

from airflow.providers.standard.operators.bash import BashOperator
from airflow.providers.standard.operators.empty import EmptyOperator
from airflow.sdk import DAG, TaskGroup

with DAG(
    dag_id="runnable_bash",
    schedule="30 2 * * *",
    start_date=pendulum.datetime(2026, 1, 1, tz="UTC"),
    catchup=False,
    params={"greeting": "hello"},
    default_args={"retries": 1, "retry_delay": timedelta(seconds=1)},
) as dag:
    start = EmptyOperator(task_id="start")
    with TaskGroup("prepare") as prepare:
        mk = BashOperator(task_id="make", bash_command='echo "{{ params.greeting }} from {{ task.task_id }}"')
        check = BashOperator(task_id="check", bash_command='test "{{ params.greeting }}" = hello')
        mk >> check
    finish = BashOperator(task_id="finish", bash_command="echo done")
    start >> prepare >> finish
