"""Per-shape size heuristic. Sizes scale to label length.

The literal multipliers and minimums in ``size_for_node`` are ported verbatim
from ``prototypes/plan-viz/v4/loom.html``'s ``autoSizeNode`` — they're tuned
for the Convene aesthetic. Adjusting them without re-checking the prototype
will produce a visibly different layout.
"""

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
    if shape == "struct":
        # Variable height: header + one row per field. Width scales to the
        # widest displayed row text or the struct's type name, whichever is
        # longer. Field rows display "name : type" or, for diverged slots,
        # "name : today → proposed".
        fields = node.get("fields", [])
        max_row_len = max((_field_display_len(f) for f in fields), default=0)
        widest = max(label_len, max_row_len)
        w = max(240, min(520, widest * 6.5 + 36))
        h = STRUCT_HEADER_H + len(fields) * STRUCT_ROW_H
        return {"width": w, "height": h}
    if shape == "diamond":
        return {"width": max(140, label_len * 7.5 + 28), "height": 70}
    if shape == "hexagon":
        return {"width": max(180, label_len * 7 + 60), "height": 48}
    if shape == "parallelogram":
        return {"width": max(180, label_len * 7 + 30), "height": 44}
    return {"width": max(160, label_len * 7 + 24), "height": 44}


STRUCT_HEADER_H = 32
STRUCT_ROW_H = 22


def _field_display_len(field: dict) -> int:
    """Length of the text we'll draw for one struct field row.

    Mirrors the rendering logic in geometry.struct_paths so width measurements
    match what actually gets drawn — drift here means clipped or over-padded
    rows.
    """
    name = field.get("name", "")
    if field.get("side") == "diverged":
        today = field.get("today", "")
        proposed = field.get("proposed", "")
        text = f"{name} : {today} → {proposed}"
    elif field.get("type"):
        text = f"{name} : {field['type']}"
    else:
        text = name
    if field.get("note"):
        text += f"   {field['note']}"
    return len(text)
