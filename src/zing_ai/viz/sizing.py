"""Per-shape size heuristic. Sizes scale to label length."""

from __future__ import annotations

from typing import TypedDict


class NodeSize(TypedDict):
    width: float
    height: float


def size_for_node(node: dict) -> NodeSize:
    """Return width/height in SVG user units for a node, given its shape and label."""
    label_len = len(node.get("label", ""))
    shape = node["shape"]
    if shape == "diverged":
        tl = len(node.get("today_label", ""))
        pl = len(node.get("proposed_label", ""))
        cl = len(node.get("concern", ""))
        w = max(280, min(440, max(tl, pl, cl) * 6.5 + 40))
        return {"width": w, "height": 100}
    if shape == "diamond":
        return {"width": max(140, label_len * 7.5 + 28), "height": 70}
    if shape == "hexagon":
        return {"width": max(180, label_len * 7 + 60), "height": 48}
    if shape == "parallelogram":
        return {"width": max(180, label_len * 7 + 30), "height": 44}
    return {"width": max(160, label_len * 7 + 24), "height": 44}
