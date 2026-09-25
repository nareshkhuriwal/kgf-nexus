#!/usr/bin/env python3
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Render deploy/docker-compose.yml into a file a remote Docker daemon can run.

The development compose file builds all twelve first-party services from source
and names no image at all. A daemon on a VM has no source tree, so it can only
pull -- which means the deployable file is the dev one with every `build:`
replaced by the `image:` this release actually pushed.

GENERATED, NOT COMMITTED. The alternative is a second compose file in the repo
holding the same topology, and the first thing that happens to a second copy of
a topology is that it stops matching the first. Rendering means the deployed
stack is the dev stack, with images substituted, every time.

The substitution is BY DIGEST. A tag can be moved after the fact -- by the next
release, or by anyone with push rights -- so a stack deployed from a tag can
pull something this pipeline never built, and `docker compose up` on a restart
would silently pick it up. A digest names one image forever, so what runs is
what this release produced and nothing else.

One image serves several services: scheduler-gpu runs the scheduler image with
different environment, and all three agents run the agent image. The map is
keyed by IMAGE name, so the lookup goes through SERVICE_TO_IMAGE below rather
than assuming the compose service and the image share a name.

Usage:
    render_compose.py --digests digests.json --out docker-compose.deploy.yml
"""

import argparse
import json
import os
import sys

import yaml

# Compose service -> image name in the digest map. Derived from the SERVICE and
# SERVICE_PATH build args in the dev compose file; a service absent here has no
# first-party image and is left exactly as it is (postgres, minio, minio-init
# are upstream images and must not be rewritten).
SERVICE_TO_IMAGE = {
    "migrate": "migrate",
    "api-gateway": "api-gateway",
    "workflow-manager": "workflow-manager",
    "dag-engine": "dag-engine",
    "scheduler": "scheduler",
    "scheduler-gpu": "scheduler",
    "resource-manager": "resource-manager",
    "event-service": "event-service",
    "agent-cpu": "agent",
    "agent-gpu": "agent",
    "agent-gpu-a10": "agent",
    "ui": "ui",
}


# Scripts the dev compose file bind-mounts from deploy/ into an upstream image
# (published path in the container -> source relative to deploy/). The host has
# no source tree, so the rendered file carries the script's text instead: the
# service's entrypoint becomes `sh -c <script> <name>` and its command keeps
# supplying the arguments. One script, two deliveries -- never a second copy.
INLINE_SCRIPTS = {
    "/nexus/nexus-nats-auth.sh": "nats/nexus-nats-auth.sh",
}


def inline_scripts(services: dict, script_root: str) -> None:
    for name, spec in services.items():
        vols = spec.get("volumes") or []
        kept = []
        for v in vols:
            target = None
            if isinstance(v, str):
                parts = v.split(":")
                if len(parts) >= 2 and parts[0].startswith("./"):
                    target = parts[1]
            if target in INLINE_SCRIPTS:
                ep = spec.get("entrypoint") or []
                if ep[-1:] != [target]:
                    raise SystemExit(
                        f"{name}: mounts {target} but its entrypoint does not run it; cannot inline"
                    )
                with open(os.path.join(script_root, INLINE_SCRIPTS[target])) as fh:
                    text = fh.read()
                # `$` must reach the container's shell, not compose interpolation.
                spec["entrypoint"] = ep[:-1] + ["-c", text.replace("$", "$$"), os.path.basename(target)]
                continue
            kept.append(v)
        if vols:
            spec["volumes"] = kept


def render(compose: dict, digests: dict, script_root: str = os.path.dirname(os.path.abspath(__file__))) -> dict:
    services = compose.get("services") or {}

    # Every service that builds must end up with an image, or the daemon is
    # asked to build from a source tree it does not have and fails with a
    # message about a missing Dockerfile rather than about a missing image.
    missing = []
    for name, spec in services.items():
        if "build" not in spec:
            continue
        image_name = SERVICE_TO_IMAGE.get(name)
        if image_name is None:
            missing.append(f"{name}: builds from source but is not in SERVICE_TO_IMAGE")
        elif image_name not in digests:
            missing.append(f"{name}: needs image {image_name!r}, which the digest map does not have")
    if missing:
        raise SystemExit(
            "Cannot render a deployable compose file:\n  - " + "\n  - ".join(missing)
        )

    for name, spec in services.items():
        if "build" not in spec:
            continue
        # Drop `build` entirely rather than leaving it beside `image`: with both
        # present, a `docker compose up --build` on the VM would try to build
        # and fail, and the failure would be confusing because the file plainly
        # names an image.
        spec.pop("build", None)
        spec["image"] = digests[SERVICE_TO_IMAGE[name]]

    inline_scripts(services, script_root)

    # Anchors are already resolved by yaml.safe_load, so the rendered file is
    # flat. That is fine for a machine-read file and avoids emitting aliases
    # that a different compose version might interpret differently.
    for key in [k for k in compose if k.startswith("x-")]:
        del compose[key]

    return compose


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--compose", default="deploy/docker-compose.yml")
    ap.add_argument("--digests", required=True, help="digests.json from the build stage")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    with open(args.compose) as fh:
        compose = yaml.safe_load(fh)
    with open(args.digests) as fh:
        digests = json.load(fh)

    # Refuse a tag reference here as well as in the build stage. This script is
    # the last point before the file reaches a daemon, and a tag that slipped
    # through would not be visible again until something restarted weeks later
    # and came up on a different image.
    for name, ref in digests.items():
        if "@sha256:" not in ref:
            raise SystemExit(f"{name}: not a digest-pinned reference: {ref!r}")

    rendered = render(compose, digests)
    with open(args.out, "w") as fh:
        fh.write(
            "# GENERATED by deploy/render_compose.py -- do not edit and do not commit.\n"
            "# Rendered from deploy/docker-compose.yml with every build: replaced by\n"
            "# the digest-pinned image this release pushed.\n"
        )
        yaml.safe_dump(rendered, fh, sort_keys=False, default_flow_style=False)

    built = sum(1 for s in rendered["services"].values() if "image" in s)
    print(f"rendered {args.out}: {len(rendered['services'])} services, {built} with images")


if __name__ == "__main__":
    sys.exit(main())
