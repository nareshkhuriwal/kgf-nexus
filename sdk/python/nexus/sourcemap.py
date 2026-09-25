# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Where in the author's Python a compiled node came from.

The control plane reports a spec problem by JSON path (``/tasks/<id>/...`` for
a semantic problem, ``/tasks/<index>/...`` for a schema one). For a YAML spec
that path maps to a line in the file; for a ``.py`` spec the document was
produced by tracing, so the useful location is the ``@task`` definition (for a
problem with the task's declaration) and the call in the workflow body that
created the node (for a problem with its wiring).

Only the SDK can know either, because only the author's process ran the code.
The map is carried NEXT TO the compiled document, never inside it: file paths
and line numbers are properties of one checkout, and putting them in the spec
would change its content address whenever a comment moved a line.
"""

from __future__ import annotations

import dataclasses
import inspect
import os
import re
import sys
from types import FrameType
from typing import Any, Callable, Mapping

__all__ = ["SourceLocation", "NodeSource", "locate"]

_PKG_DIR = os.path.dirname(os.path.abspath(__file__))


@dataclasses.dataclass(frozen=True, slots=True)
class SourceLocation:
    """A file and 1-based line (and column, when known)."""

    file: str
    line: int
    column: int = 0

    def __str__(self) -> str:
        return f"{self.file}:{self.line}" + (f":{self.column}" if self.column else "")

    def to_json(self) -> dict[str, Any]:
        out: dict[str, Any] = {"file": self.file, "line": self.line}
        if self.column:
            out["column"] = self.column
        return out

    @staticmethod
    def of_callable(fn: Callable[..., Any]) -> "SourceLocation | None":
        """The first line of a function's definition, decorators included.

        ``co_firstlineno`` is the line of the first decorator since Python 3.8,
        which is exactly the ``@task(...)`` line an author wants to be sent to.
        """
        code = getattr(inspect.unwrap(fn), "__code__", None)
        if code is None:
            return None
        return SourceLocation(file=code.co_filename, line=code.co_firstlineno)

    @staticmethod
    def of_caller() -> "SourceLocation | None":
        """The first frame outside this package: the author's call site."""
        frame: FrameType | None = sys._getframe(1)
        while frame is not None:
            filename = frame.f_code.co_filename
            if os.path.dirname(os.path.abspath(filename)) != _PKG_DIR:
                return SourceLocation(file=filename, line=frame.f_lineno)
            frame = frame.f_back
        return None


@dataclasses.dataclass(frozen=True, slots=True)
class NodeSource:
    """Where one node came from: its ``@task`` and the call that traced it."""

    definition: SourceLocation | None
    call_site: SourceLocation | None

    def to_json(self) -> dict[str, Any]:
        out: dict[str, Any] = {}
        if self.definition is not None:
            out["definition"] = self.definition.to_json()
        if self.call_site is not None:
            out["call_site"] = self.call_site.to_json()
        return out


# Fields that describe the task's DECLARATION (so live on the @task line) as
# opposed to its wiring in the workflow body (the call site).
_WIRING_FIELDS = frozenset({"input_literals", "input_params", "map", "edge"})
_TASK_PATH = re.compile(r"^/tasks/([^/]+)(?:/(.*))?$")


def locate(
    source_map: Mapping[str, NodeSource], task_ids: list[str], path: str
) -> SourceLocation | None:
    """Map a problem path from the gateway or the SDK to a Python location.

    ``task_ids`` is the compiled document's task order, which resolves the
    numeric index a JSON Schema path carries. Problems with a node's wiring
    (edges, literals, params, map) point at the call site; everything else at the
    ``@task`` definition. Returns None when the path names no node.
    """
    m = _TASK_PATH.match(path)
    if not m:
        return None
    key, rest = m.group(1), m.group(2) or ""
    if key.isdigit() and int(key) < len(task_ids):
        key = task_ids[int(key)]
    node = source_map.get(key)
    if node is None:
        return None
    head = rest.split("/", 1)[0].split(".", 1)[0].split(" ", 1)[0]
    if head in _WIRING_FIELDS:
        return node.call_site or node.definition
    return node.definition or node.call_site
