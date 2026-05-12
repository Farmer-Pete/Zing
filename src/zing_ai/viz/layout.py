"""Run Graphviz layout per step. Returns positions in SVG user units."""

from __future__ import annotations

import json
import logging
from typing import Any

import graphviz

from zing_ai.viz import sizing

logger = logging.getLogger(__name__)
logger.info("Graphviz dot version: %s", ".".join(map(str, graphviz.version())))


def layout_step(step: dict[str, Any]) -> dict[str, Any]:
    """Return *step* augmented with viewBox + per-node x/y/w/h.

    Coordinates are in SVG user units (post-conversion from Graphviz's
    inches @ 72 DPI).
    """
    g = graphviz.Digraph(
        graph_attr={"rankdir": "TB", "nodesep": "0.4", "ranksep": "0.78"},
        node_attr={"shape": "box"},
    )
    for n in step["nodes"]:
        sz = sizing.size_for_node(n)
        g.node(
            n["id"],
            label=n["label"],
            width=f"{sz['width'] / 72}",
            height=f"{sz['height'] / 72}",
            fixedsize="true",
        )
    for e in step["edges"]:
        g.edge(e["from"], e["to"])

    rendered = g.pipe(format="json").decode()
    return _ingest(json.loads(rendered), step)


def _ingest(dot_json: dict[str, Any], step: dict[str, Any]) -> dict[str, Any]:
    """Convert dot's JSON output into our augmented step dict."""
    bb = [float(x) for x in dot_json["bb"].split(",")]
    height = bb[3] - bb[1]

    new_nodes = []
    by_name = {n["id"]: n for n in step["nodes"]}
    for obj in dot_json.get("objects", []):
        name = obj.get("name")
        node = by_name.get(name)
        if node is None:
            continue
        cx, cy_bottom = (float(v) for v in obj["pos"].split(","))
        cy = height - cy_bottom
        w = float(obj["width"]) * 72
        h = float(obj["height"]) * 72
        new_nodes.append(
            {
                **node,
                "x": cx - w / 2,
                "y": cy - h / 2,
                "w": w,
                "h": h,
            }
        )

    pad = 16
    return {
        **step,
        "nodes": new_nodes,
        "viewBox": [
            -pad,
            -pad,
            (bb[2] - bb[0]) + 2 * pad,
            height + 2 * pad,
        ],
    }


def layout_graph(graph: dict[str, Any]) -> dict[str, Any]:
    """Return *graph* with each step laid out independently."""
    return {
        **graph,
        "steps": [layout_step(s) for s in graph["steps"]],
    }
