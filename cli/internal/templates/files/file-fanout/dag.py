"""__WORKFLOW_NAME__ -- process every object under a prefix, then write a manifest.

    list_objects --> process_object (one child per object) --> build_manifest

  * list_objects    lists the objects under `prefix` (a local directory,
                    file://, or s3://bucket/path with boto3 installed) that
                    match `pattern`, each with a VERSION (mtime or ETag).
  * process_object  a MAP over the listing: streams one object and returns its
                    size, line count and SHA-256. Memoized on (key, version), so
                    an unchanged object is never read twice and a changed one
                    always is.
  * build_manifest  the fan-in: checks every object was processed and writes a
                    manifest JSON (atomically) next to the data.

The fan-in is bounded BY PROOF: process_object declares `max_bytes` on its
output, so at submit the control plane checks max_fanout x max_bytes against
its fan-in limit, and nexus.entrypoint refuses to emit a larger result.

    pytest
    nexusctl workflow compile dag.py --show
    nexusctl workflow submit dag.py
    nexusctl run create __WORKFLOW_NAME__ --param prefix=/data/incoming --wait
"""

from __future__ import annotations

import datetime as dt
import fnmatch
import hashlib
import json
import os
from typing import Any, Iterator

from nexus import Cache, Resources, Retry, Runtime, TaskContext, Timeouts, task, workflow

IMAGE = "__WORKFLOW_NAME__:0.1.0"

# Ceiling on the fan-out, and on each child's result. The listing itself
# travels inline (it is the map's collection), so it must stay under 64 KiB:
# list_objects checks that too, and says so, rather than failing obscurely.
MAX_OBJECTS = 500
RESULT_MAX_BYTES = 512
MAX_LISTING_BYTES = 60 * 1024

_CHUNK = 1024 * 1024


def _task(name: str, **kwargs: Any):
    ref = f"__WORKFLOW_NAME__.{name}"
    return task(
        task_ref=ref,
        runtime=Runtime(
            executor="docker",
            image=IMAGE,
            command=["python", "-m", "nexus.entrypoint", "--module", "dag"],
            args=[ref],
        ),
        **kwargs,
    )


# ── storage: local filesystem, or S3 when boto3 is installed ─────────────────


def _is_s3(prefix: str) -> bool:
    return prefix.startswith("s3://")


def _s3():
    try:
        import boto3  # type: ignore[import-not-found]
    except ModuleNotFoundError:
        raise ModuleNotFoundError("s3:// prefixes need boto3: add it to requirements.txt and rebuild the image") from None
    return boto3.client("s3")


def _split_s3(uri: str) -> tuple[str, str]:
    bucket, _, key = uri[len("s3://"):].partition("/")
    return bucket, key


def _local(prefix: str) -> str:
    return prefix[len("file://"):] if prefix.startswith("file://") else prefix


def _list(prefix: str) -> Iterator[dict]:
    if _is_s3(prefix):
        bucket, root = _split_s3(prefix.rstrip("/") + "/")
        for page in _s3().get_paginator("list_objects_v2").paginate(Bucket=bucket, Prefix=root):
            for obj in page.get("Contents", []):
                yield {"key": obj["Key"][len(root):], "size": obj["Size"], "version": obj["ETag"].strip('"')}
        return
    root = _local(prefix)
    if not os.path.isdir(root):
        raise FileNotFoundError(f"prefix {prefix} is not a directory")
    for dirpath, _, files in os.walk(root):
        for name in files:
            path = os.path.join(dirpath, name)
            st = os.stat(path)
            yield {"key": os.path.relpath(path, root), "size": st.st_size, "version": str(st.st_mtime_ns)}


def _open_chunks(prefix: str, key: str) -> Iterator[bytes]:
    if _is_s3(prefix):
        bucket, root = _split_s3(prefix.rstrip("/") + "/")
        body = _s3().get_object(Bucket=bucket, Key=root + key)["Body"]
        yield from iter(lambda: body.read(_CHUNK), b"")
        return
    with open(os.path.join(_local(prefix), key), "rb") as f:
        yield from iter(lambda: f.read(_CHUNK), b"")


# ── tasks ────────────────────────────────────────────────────────────────────


@_task(
    "list_objects",
    resources=Resources(cpu="250m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="10m", heartbeat="2m"),
    retry=Retry(max_attempts=4, retry_on=["transient", "timeout", "infrastructure"]),
)
def list_objects(ctx: TaskContext, prefix: str, pattern: str = "*") -> list:
    """The objects to process, sorted by key, each with its current version."""
    objects = sorted((o for o in _list(prefix) if fnmatch.fnmatch(o["key"], pattern)), key=lambda o: o["key"])
    if not objects:
        raise FileNotFoundError(f"no objects under {prefix} match {pattern!r}")
    if len(objects) > MAX_OBJECTS:
        raise ValueError(
            f"{len(objects)} objects under {prefix} match {pattern!r}, over MAX_OBJECTS={MAX_OBJECTS}; "
            "narrow the pattern, or split the prefix into several runs"
        )
    size = len(json.dumps(objects, separators=(",", ":")))
    if size > MAX_LISTING_BYTES:
        raise ValueError(f"the listing is {size} bytes, over the {MAX_LISTING_BYTES}-byte inline limit for a map collection")
    ctx.log.info("listed", prefix=prefix, objects=len(objects), bytes=sum(o["size"] for o in objects))
    return objects


@_task(
    "process_object",
    resources=Resources(cpu="500m", memory="512Mi"),
    timeouts=Timeouts(start_to_close="30m", heartbeat="5m"),
    retry=Retry(max_attempts=3, retry_on=["transient", "timeout", "infrastructure", "worker_lost"]),
    # Keyed on the object element, which includes its version: an unchanged
    # object is a cache hit, a modified one is processed again.
    cache=Cache(enabled=True, ttl="30d"),
    outputs={"result": {"type": "json", "required": True, "max_bytes": RESULT_MAX_BYTES}},
)
def process_object(ctx: TaskContext, obj: dict, prefix: str) -> dict:
    """Stream one object: size, line count and SHA-256. Replace with your processing."""
    digest = hashlib.sha256()
    size = lines = 0
    for chunk in _open_chunks(prefix, obj["key"]):
        digest.update(chunk)
        size += len(chunk)
        lines += chunk.count(b"\n")
        ctx.heartbeat()
    if size != obj["size"]:
        # The object changed between listing and reading. Failing is right: the
        # manifest would otherwise describe bytes that no longer exist.
        raise RuntimeError(f"{obj['key']} was {obj['size']} bytes when listed and {size} when read; re-run")
    return {"key": obj["key"], "bytes": size, "lines": lines, "sha256": digest.hexdigest()}


@_task(
    "build_manifest",
    resources=Resources(cpu="250m", memory="512Mi"),
    timeouts=Timeouts(start_to_close="10m", heartbeat="2m"),
    retry=Retry(max_attempts=3, retry_on=["transient", "infrastructure"]),
    trigger_rule="all_done",  # report every failed object at once
)
def build_manifest(ctx: TaskContext, results: list, objects: list, prefix: str, manifest_path: str) -> dict:
    """Check every object was processed and write the manifest."""
    failed = [o["key"] for o, r in zip(objects, results) if r is None]
    if failed or len(results) != len(objects):
        raise RuntimeError(f"{len(failed)} of {len(objects)} objects failed: {', '.join(failed[:20])}")
    manifest = {
        "prefix": prefix,
        "generated_at": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds"),
        "run_id": ctx.run_id,
        "objects": results,
        "total_bytes": sum(r["bytes"] for r in results),
        "total_lines": sum(r["lines"] for r in results),
    }
    body = json.dumps(manifest, indent=2, sort_keys=True).encode()
    if manifest_path:
        path = _local(manifest_path)
        if os.path.dirname(path):
            os.makedirs(os.path.dirname(path), exist_ok=True)
        tmp = f"{path}.tmp"
        with open(tmp, "wb") as f:
            f.write(body)
        os.replace(tmp, path)
    ctx.log.info("manifest written", path=manifest_path, objects=len(results))
    return {
        "manifest_path": manifest_path,
        "manifest_sha256": hashlib.sha256(body).hexdigest(),
        "objects": len(results),
        "total_bytes": manifest["total_bytes"],
    }


# ── workflow ─────────────────────────────────────────────────────────────────


@workflow(
    name="__WORKFLOW_NAME__",
    namespace="__NAMESPACE__",
    description="Process every object under a prefix in parallel and fan in a manifest.",
    params={
        "prefix": {"type": "string", "required": True},
        "pattern": {"type": "string", "required": False, "default": "*"},
        "manifest_path": {"type": "string", "required": False, "default": "/data/manifest.json"},
    },
    run_timeout="4h",
)
def file_fanout(prefix: str, pattern: str, manifest_path: str):
    objects = list_objects(prefix=prefix, pattern=pattern)
    results = process_object.map(objects, item="obj", max_fanout=MAX_OBJECTS, prefix=prefix)
    return build_manifest(results=results, objects=objects, prefix=prefix, manifest_path=manifest_path)


if __name__ == "__main__":
    compiled = file_fanout.compile()
    print(compiled.to_json(indent=2))
    print(f"\nspec hash: {compiled.hash}")
