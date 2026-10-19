#!/usr/bin/env python3
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Check rendered manifests against Pod Security Standards "restricted".

    deploy/helm/pss-restricted.py <rendered.yaml>...

Every pod template (Deployment, StatefulSet, DaemonSet, Job, CronJob, Pod)
must pass the restricted profile as of Kubernetes 1.29, the chart's floor:
no host namespaces or hostPath, only the restricted volume types, safe sysctls,
runAsNonRoot and a non-zero runAsUser, a RuntimeDefault/Localhost seccomp
profile, and, for EVERY container including init containers,
allowPrivilegeEscalation false and capabilities dropping ALL (adding at most
NET_BIND_SERVICE). Pod-level settings count for the containers that do not
override them, as the admission plugin applies them.

deploy/helm/validate.sh runs it over every ci/ values file, so a template that
would be refused by a namespace labelled
pod-security.kubernetes.io/enforce=restricted fails CI instead of an install.
Exit 1 and one line per violation when anything fails.
"""
import sys

import yaml

ALLOWED_VOLUMES = {"configMap", "csi", "downwardAPI", "emptyDir", "ephemeral",
                   "persistentVolumeClaim", "projected", "secret"}
# The "safe" set the baseline and restricted profiles admit.
SAFE_SYSCTLS = {"kernel.shm_rmid_forced", "net.ipv4.ip_local_port_range",
                "net.ipv4.ip_unprivileged_port_start", "net.ipv4.tcp_syncookies",
                "net.ipv4.ping_group_range", "net.ipv4.ip_local_reserved_ports",
                "net.ipv4.tcp_keepalive_time", "net.ipv4.tcp_fin_timeout",
                "net.ipv4.tcp_keepalive_intvl", "net.ipv4.tcp_keepalive_probes"}
SECCOMP_OK = ("RuntimeDefault", "Localhost")


def pod_specs(doc):
    kind = doc.get("kind")
    if kind in ("Deployment", "StatefulSet", "DaemonSet", "Job", "ReplicaSet"):
        yield doc["spec"]["template"]["spec"]
    elif kind == "CronJob":
        yield doc["spec"]["jobTemplate"]["spec"]["template"]["spec"]
    elif kind == "Pod":
        yield doc["spec"]


def violations(where, spec):
    psc = spec.get("securityContext") or {}
    for field in ("hostNetwork", "hostPID", "hostIPC"):
        if spec.get(field):
            yield f"{where}: {field}"
    for vol in spec.get("volumes") or []:
        types = set(vol) - {"name"}
        if not types <= ALLOWED_VOLUMES:
            yield f"{where}: volume {vol['name']} is {sorted(types)}"
    for sysctl in psc.get("sysctls") or []:
        if sysctl["name"] not in SAFE_SYSCTLS:
            yield f"{where}: sysctl {sysctl['name']}"
    if psc.get("runAsUser") == 0:
        yield f"{where}: pod runAsUser 0"
    pod_nonroot = psc.get("runAsNonRoot") is True
    pod_seccomp = (psc.get("seccompProfile") or {}).get("type") in SECCOMP_OK
    for c in (spec.get("initContainers") or []) + (spec.get("containers") or []):
        cw = f"{where}[{c['name']}]"
        sc = c.get("securityContext") or {}
        if sc.get("privileged"):
            yield f"{cw}: privileged"
        if sc.get("allowPrivilegeEscalation") is not False:
            yield f"{cw}: allowPrivilegeEscalation is not false"
        caps = sc.get("capabilities") or {}
        if "ALL" not in (caps.get("drop") or []):
            yield f"{cw}: capabilities do not drop ALL"
        if set(caps.get("add") or []) - {"NET_BIND_SERVICE"}:
            yield f"{cw}: adds capabilities {caps.get('add')}"
        if not (sc.get("runAsNonRoot") is True or (pod_nonroot and sc.get("runAsNonRoot") is not False)):
            yield f"{cw}: runAsNonRoot is not true"
        if sc.get("runAsUser") == 0:
            yield f"{cw}: runAsUser 0"
        seccomp = (sc.get("seccompProfile") or {}).get("type")
        if not (seccomp in SECCOMP_OK or (pod_seccomp and seccomp is None)):
            yield f"{cw}: no RuntimeDefault/Localhost seccomp profile"
        for port in c.get("ports") or []:
            if port.get("hostPort"):
                yield f"{cw}: hostPort {port['hostPort']}"


def main(paths):
    found, pods = [], 0
    for path in paths:
        with open(path, encoding="utf-8") as fh:
            for doc in yaml.safe_load_all(fh):
                if not doc:
                    continue
                for spec in pod_specs(doc):
                    pods += 1
                    where = f"{doc['kind']}/{doc['metadata']['name']}"
                    found.extend(violations(where, spec))
    for v in found:
        print(f"    restricted: {v}")
    print(f"pss-restricted: {pods} pod template(s), {len(found)} violation(s)")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
