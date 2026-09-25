"""Tests for the task functions. They run the real handlers, no platform needed."""

from __future__ import annotations

import hashlib
import json
import os

import pytest

from dag import MAX_OBJECTS, RESULT_MAX_BYTES, build_manifest, file_fanout, list_objects, process_object


@pytest.fixture
def prefix(tmp_path):
    root = tmp_path / "in"
    (root / "sub").mkdir(parents=True)
    (root / "a.csv").write_bytes(b"x\n1\n2\n")
    (root / "b.txt").write_bytes(b"hello")
    (root / "sub" / "c.csv").write_bytes(b"y\n3\n")
    return str(root)


def test_listing_is_sorted_filtered_and_versioned(make_ctx, prefix):
    objs = list_objects(make_ctx("list_objects"), prefix=prefix, pattern="*.csv")
    assert [o["key"] for o in objs] == ["a.csv", os.path.join("sub", "c.csv")]
    assert all(o["version"] for o in objs)


def test_an_empty_listing_fails(make_ctx, prefix):
    with pytest.raises(FileNotFoundError, match="no objects"):
        list_objects(make_ctx("list_objects"), prefix=prefix, pattern="*.parquet")


def test_too_many_objects_fails_before_fanning_out(make_ctx, tmp_path):
    for i in range(MAX_OBJECTS + 1):
        (tmp_path / f"f{i}").write_bytes(b"")
    with pytest.raises(ValueError, match="MAX_OBJECTS"):
        list_objects(make_ctx("list_objects"), prefix=str(tmp_path))


def test_processing_hashes_and_counts_an_object(make_ctx, prefix):
    obj = {"key": "a.csv", "size": 6, "version": "1"}
    out = process_object(make_ctx("process_object"), obj=obj, prefix=prefix)
    assert out == {"key": "a.csv", "bytes": 6, "lines": 3, "sha256": hashlib.sha256(b"x\n1\n2\n").hexdigest()}
    assert len(json.dumps(out)) <= RESULT_MAX_BYTES


def test_an_object_that_changed_since_listing_fails(make_ctx, prefix):
    with pytest.raises(RuntimeError, match="when listed"):
        process_object(make_ctx("process_object"), obj={"key": "b.txt", "size": 99, "version": "1"}, prefix=prefix)


def test_end_to_end_writes_a_manifest(make_ctx, prefix, tmp_path):
    objs = list_objects(make_ctx("list_objects"), prefix=prefix)
    results = process_object.map(objs, item="obj", max_fanout=MAX_OBJECTS, ctx=make_ctx("process_object"),
                                 prefix=prefix)
    path = str(tmp_path / "out" / "manifest.json")
    summary = build_manifest(make_ctx("build_manifest"), results=results, objects=objs, prefix=prefix,
                             manifest_path=path)
    manifest = json.loads(open(path).read())
    assert summary["objects"] == 3 and manifest["total_bytes"] == 6 + 5 + 4
    assert summary["manifest_sha256"] == hashlib.sha256(open(path, "rb").read()).hexdigest()


def test_manifest_names_failed_objects(make_ctx):
    objs = [{"key": "a"}, {"key": "b"}]
    with pytest.raises(RuntimeError, match="1 of 2 objects failed: b"):
        build_manifest(make_ctx("build_manifest"), results=[{"bytes": 1, "lines": 0}, None], objects=objs,
                       prefix="p", manifest_path="")


def test_workflow_declares_the_fan_in_bound():
    spec = file_fanout.compile().spec
    node = next(t for t in spec["tasks"] if t["id"] == "process_object")
    assert node["map"]["max_fanout"] == MAX_OBJECTS
    assert node["outputs"]["result"]["max_bytes"] == RESULT_MAX_BYTES
