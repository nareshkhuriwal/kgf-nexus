# Task image for __WORKFLOW_NAME__.
#
# Every task in dag.py runs this image with
#   python -m nexus.entrypoint --module dag <task_ref>
# which reads the task's inputs from $NEXUS_INPUT_DIR, calls the handler, and
# writes its outputs to $NEXUS_OUTPUT_DIR (the executor file contract).
#
#   docker build -t __WORKFLOW_NAME__:0.1.0 .
#
# Push it somewhere your workers can pull from, and set IMAGE in dag.py to that
# reference. Pin by digest in production so a re-tag cannot change what runs.
FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

WORKDIR /app
COPY requirements.txt .
RUN pip install -r requirements.txt

COPY dag.py .

# Run as a non-root user: a task never needs root, and a container escape from
# root is a much worse day.
RUN useradd --uid 10001 --create-home nexus
USER 10001
