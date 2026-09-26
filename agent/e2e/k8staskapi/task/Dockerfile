# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# The task image for the kubernetes workload-API end-to-end test
# (agent/e2e/k8staskapi/taskapi_e2e_test.go): CPython, the Nexus SDK from this checkout
# (zero runtime dependencies) and the test's handler. Test-only.
#
#   docker build -f agent/e2e/k8staskapi/task/Dockerfile -t <tag> .
FROM python:3.12-alpine@sha256:4c47124a8391cb7a9f571164147d154777cf012a4ece5f86097130d7a4478111
COPY sdk/python/nexus /opt/nexus/sdk/nexus
COPY agent/e2e/k8staskapi/task/workload.py /opt/nexus/task/workload.py
ENV PYTHONPATH=/opt/nexus/sdk \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1
# The kubernetes executor runs task pods as a non-root uid (65532 by default)
# with a read-only root filesystem; nothing here needs to be writable.
USER 65532:65532
