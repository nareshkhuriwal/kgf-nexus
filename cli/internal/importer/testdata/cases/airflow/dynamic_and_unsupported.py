# Hand-written test input for `nexusctl import airflow` (original to this
# repository). Mapping, and the constructs a static converter must refuse.
from datetime import datetime, timedelta

from kubernetes.client import models as k8s

from airflow.models import Variable
from airflow.providers.cncf.kubernetes.operators.pod import KubernetesPodOperator
from airflow.providers.standard.operators.bash import BashOperator
from airflow.providers.standard.operators.python import BranchPythonOperator
from airflow.providers.standard.sensors.filesystem import FileSensor
from airflow.sdk import dag, task

BUCKET = Variable.get("bucket")


def choose():
    return "train"


@dag(schedule=timedelta(hours=6), start_date=datetime(2026, 1, 1), catchup=False, dagrun_timeout=timedelta(hours=2))
def dynamic_and_unsupported(source: str = "s3://in", batch: int = 32):
    """Mapping over literals and upstream outputs; sensors and branches."""

    @task(retries=3)
    def list_files(src: str) -> list:
        return [src + "/a", src + "/b"]

    @task
    def process(path: str, batch: int) -> int:
        return len(path) * batch

    @task
    def total(counts: list) -> int:
        return sum(counts)

    @task
    def square(x: int) -> int:
        return x * x

    @task
    def pair(x: int, y: int) -> int:
        return x + y

    files = list_files(source)
    counts = process.partial(batch=batch).expand(path=files)
    summed = total(counts)
    squares = square.expand(x=[1, 2, 3])
    pair.expand(x=[1, 2], y=[3, 4])

    wait = FileSensor(task_id="wait_for_file", filepath="/data/ready", mode="reschedule", poke_interval=60)
    branch = BranchPythonOperator(task_id="branch", python_callable=choose)

    @task.branch
    def pick() -> str:
        return "train"

    train = KubernetesPodOperator(
        task_id="train",
        image="ghcr.io/acme/train:1.4",
        cmds=["python", "-m", "train"],
        arguments=["--epochs", "3"],
        env_vars={"MODE": "full"},
        container_resources=k8s.V1ResourceRequirements(
            requests={"cpu": "2", "memory": "8Gi"},
            limits={"memory": "16Gi", "nvidia.com/gpu": "1"},
        ),
        namespace="ml",
    )

    for i in range(3):
        shard = BashOperator(task_id=f"shard_{i}", bash_command=f"echo {i}")
        shard >> train

    mapped_op = BashOperator.partial(task_id="echo_each").expand(bash_command=["echo 1", "echo 2"])

    wait >> branch >> pick() >> train
    summed >> train
    squares >> train
    mapped_op >> train


dynamic_and_unsupported()
