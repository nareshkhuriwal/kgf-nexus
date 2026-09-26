# Task image for __WORKFLOW_NAME__.
#
# Every task in dag.py runs this image with
#   python -m nexus.entrypoint --module dag <task_ref>
# which reads the task's inputs from $NEXUS_INPUT_DIR, calls the handler, and
# writes its outputs to $NEXUS_OUTPUT_DIR (the executor file contract).
#
#   docker build --build-context nexus-sdk=/path/to/kgf-nexus/sdk/python \
#       -t __WORKFLOW_NAME__:0.1.0 .
#
# The Nexus SDK comes from the named build context "nexus-sdk", never from a
# package index by name: "nexus-sdk" on PyPI is an unrelated project (see
# requirements.txt). Without --build-context the build fails at the COPY below
# instead of silently installing the wrong package. Named build contexts need
# BuildKit (the default builder since Docker Engine 23).
#
# Push the image somewhere your workers can pull from, and set IMAGE in dag.py
# to that reference. Pin by digest in production so a re-tag cannot change
# what runs.
FROM python:3.12-slim

# PYTHONPATH, because executors do not run the task in WORKDIR: the docker
# executor sets the working directory to the attempt's /nexus mount, so
# `--module dag` must be importable from anywhere (F144).
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PYTHONPATH=/app

WORKDIR /app
COPY --from=nexus-sdk . /tmp/nexus-sdk
RUN pip install /tmp/nexus-sdk && rm -rf /tmp/nexus-sdk
COPY requirements.txt .
RUN pip install -r requirements.txt

# The project itself; .dockerignore keeps virtualenvs, tests and caches out.
COPY . .

# Run as a non-root user: a task never needs root, and a container escape from
# root is a much worse day.
RUN useradd --uid 10001 --create-home nexus
USER 10001
