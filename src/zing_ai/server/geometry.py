"""SVG path generation for inner-node, inner-edge, and cross-flow primitives.

Port of prototypes/plan-viz/v4/convene.html's renderInnerNode / renderInnerEdge
/ pickPortPair / makeBezier as pure functions. Templates are thin loops over
these pre-computed strings — no math in Jinja, no math in JS.

All inputs are expected to have absolute coordinates (post-layout), in SVG
user units. Templates render the returned ``d`` strings directly in
``<path d=...>``.
"""

from __future__ import annotations

import math
from collections.abc import Mapping
from typing import Any, TypedDict

PORT_NORMAL: dict[str, dict[str, float]] = {
    "left": {"x": -1.0, "y": 0.0},
    "right": {"x": 1.0, "y": 0.0},
    "top": {"x": 0.0, "y": -1.0},
    "bottom": {"x": 0.0, "y": 1.0},
}


class Point(TypedDict):
    x: float
    y: float


class _BoxLayout(TypedDict):
    x: float
    y: float
    width: float
    height: float


class _LineLayout(TypedDict):
    x1: float
    y1: float
    x2: float
    y2: float


class DivergedLayout(TypedDict):
    """Discrete layout primitives for the diverged shape.

    Templates render an SVG group containing: an outer rect, two inner halves,
    two divider lines, and four text anchors (concern, today-side label,
    today formula, proposed-side label, proposed formula).
    """

    outer: str
    today_half: _BoxLayout
    proposed_half: _BoxLayout
    divider_top: _LineLayout
    divider_mid: _LineLayout
    concern_anchor: Point
    today_side_label_pos: Point
    today_label_pos: Point
    proposed_side_label_pos: Point
    proposed_label_pos: Point


def rounded_rect_path(x: float, y: float, w: float, h: float, rx: float) -> str:
    """SVG ``d`` string for a rounded rectangle."""
    return (
        f"M {x + rx} {y} "
        f"H {x + w - rx} A {rx} {rx} 0 0 1 {x + w} {y + rx} "
        f"V {y + h - rx} A {rx} {rx} 0 0 1 {x + w - rx} {y + h} "
        f"H {x + rx} A {rx} {rx} 0 0 1 {x} {y + h - rx} "
        f"V {y + rx} A {rx} {rx} 0 0 1 {x + rx} {y} Z"
    )


def node_outline(node: dict[str, Any]) -> str:
    """SVG ``d`` string for a non-diverged, non-struct node's outline.

    Multi-primitive shapes raise ``ValueError`` — use ``diverged_paths(node)``
    or ``struct_paths(node)`` instead.
    """
    shape = node["shape"]
    x, y, w, h = node["x"], node["y"], node["w"], node["h"]
    if shape == "rect":
        return rounded_rect_path(x, y, w, h, 3)
    if shape == "diamond":
        cx, cy = x + w / 2, y + h / 2
        return f"M {cx} {y} L {x + w} {cy} L {cx} {y + h} L {x} {cy} Z"
    if shape == "hexagon":
        inset = 18
        return (
            f"M {x + inset} {y} L {x + w - inset} {y} L {x + w} {y + h / 2} "
            f"L {x + w - inset} {y + h} L {x + inset} {y + h} L {x} {y + h / 2} Z"
        )
    if shape == "parallelogram":
        skew = 14
        return f"M {x + skew} {y} L {x + w} {y} L {x + w - skew} {y + h} L {x} {y + h} Z"
    if shape == "diverged":
        raise ValueError("Diverged shapes must use diverged_paths()")
    if shape == "struct":
        raise ValueError("Struct shapes must use struct_paths()")
    return rounded_rect_path(x, y, w, h, 3)


def diverged_paths(node: dict[str, Any]) -> DivergedLayout:
    """Discrete layout primitives for the diverged shape."""
    x, y, w, h = node["x"], node["y"], node["w"], node["h"]
    header_h = 24
    half_h = (h - header_h) / 2
    cx = x + w / 2
    return {
        "outer": rounded_rect_path(x, y, w, h, 3),
        "today_half": {"x": x + 1, "y": y + header_h, "width": w - 2, "height": half_h},
        "proposed_half": {
            "x": x + 1,
            "y": y + header_h + half_h,
            "width": w - 2,
            "height": half_h - 1,
        },
        "divider_top": {"x1": x, "y1": y + header_h, "x2": x + w, "y2": y + header_h},
        "divider_mid": {
            "x1": x,
            "y1": y + header_h + half_h,
            "x2": x + w,
            "y2": y + header_h + half_h,
        },
        "concern_anchor": {"x": cx, "y": y + 16},
        "today_side_label_pos": {"x": x + 8, "y": y + header_h + 13},
        "today_label_pos": {"x": cx, "y": y + header_h + half_h / 2 + 8},
        "proposed_side_label_pos": {"x": x + 8, "y": y + header_h + half_h + 13},
        "proposed_label_pos": {
            "x": cx,
            "y": y + header_h + half_h + half_h / 2 + 8,
        },
    }


class _StructRow(TypedDict):
    """One field/variant/collection row inside a struct node."""

    side: str
    rect: _BoxLayout
    text: str
    text_pos: Point


class StructLayout(TypedDict):
    """Discrete layout primitives for the struct shape.

    Templates render: outer rounded rect, header strip (band + type name +
    optional kind badge), one row per field (rect + text). Per-row ``side``
    drives the CSS class for change marking.
    """

    outer: str
    header_rect: _BoxLayout
    header_text_pos: Point
    kind_badge_pos: Point | None
    kind_badge_text: str | None
    rows: list[_StructRow]


def _struct_row_text(field: Mapping[str, Any]) -> str:
    """Render one field as `name : type` (or `name : today → proposed` when
    diverged), with an optional trailing note. Mirrored by sizing._field_display_len
    so width measurements line up.
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
    return text


def struct_paths(node: dict[str, Any]) -> StructLayout:
    """Discrete layout primitives for the struct shape.

    Geometry constants here mirror ``sizing.STRUCT_HEADER_H`` /
    ``STRUCT_ROW_H`` — drift means the laid-out node won't match the
    drawn rows.
    """
    x, y, w, h = node["x"], node["y"], node["w"], node["h"]
    header_h = 32
    row_h = 22
    fields = node.get("fields", []) or []
    kind = node.get("kind") or "struct"

    rows: list[_StructRow] = []
    for i, f in enumerate(fields):
        ry = y + header_h + i * row_h
        rows.append(
            {
                "side": f.get("side", "shared"),
                "rect": {"x": x + 1, "y": ry, "width": w - 2, "height": row_h},
                "text": _struct_row_text(f),
                "text_pos": {"x": x + 12, "y": ry + row_h / 2 + 4},
            }
        )

    badge_pos: Point | None = None
    badge_text: str | None = None
    if kind != "struct":
        badge_text = kind
        badge_pos = {"x": x + w - 10, "y": y + 20}

    return {
        "outer": rounded_rect_path(x, y, w, h, 4),
        "header_rect": {"x": x + 1, "y": y + 1, "width": w - 2, "height": header_h - 1},
        "header_text_pos": {"x": x + 12, "y": y + 21},
        "kind_badge_pos": badge_pos,
        "kind_badge_text": badge_text,
        "rows": rows,
    }


def make_bezier(x1: float, y1: float, x2: float, y2: float) -> str:
    """Return a cubic Bezier ``d`` string between two points.

    Bend direction picked by the dominant axis: vertical if |dy| >= |dx|,
    horizontal otherwise. Control-point offset is 40% of the dominant
    delta, clamped to a 20 px minimum.
    """
    dx, dy = x2 - x1, y2 - y1
    if abs(dy) >= abs(dx):
        c = max(20, abs(dy) * 0.4)
        return f"M {x1} {y1} C {x1} {y1 + c}, {x2} {y2 - c}, {x2} {y2}"
    c = max(20, abs(dx) * 0.4)
    return f"M {x1} {y1} C {x1 + c} {y1}, {x2 - c} {y2}, {x2} {y2}"


def inner_edge_path(
    edge: dict[str, Any],
    from_node: dict[str, Any],
    to_node: dict[str, Any],
) -> str:
    """Bezier path for an in-step edge between two laid-out nodes.

    The departure side is given by ``edge.from_side`` (default bottom). The
    arrival point is chosen on whichever side of ``to_node`` the source is
    closest to, weighted toward the dominant axis.
    """
    fs = edge.get("from_side", "bottom")
    if fs == "left":
        x1, y1 = from_node["x"], from_node["y"] + from_node["h"] / 2
    elif fs == "right":
        x1, y1 = from_node["x"] + from_node["w"], from_node["y"] + from_node["h"] / 2
    elif fs == "top":
        x1, y1 = from_node["x"] + from_node["w"] / 2, from_node["y"]
    else:
        x1, y1 = (
            from_node["x"] + from_node["w"] / 2,
            from_node["y"] + from_node["h"],
        )
    to_cx = to_node["x"] + to_node["w"] / 2
    to_cy = to_node["y"] + to_node["h"] / 2
    dx, dy = to_cx - x1, to_cy - y1
    if abs(dy) > abs(dx) * 0.6:
        x2 = to_cx
        y2 = to_node["y"] if dy > 0 else to_node["y"] + to_node["h"]
    else:
        x2 = to_node["x"] if dx > 0 else to_node["x"] + to_node["w"]
        y2 = to_cy
    return make_bezier(x1, y1, x2, y2)


_PORT_CANDIDATES: list[tuple[str, str]] = [
    ("right", "left"),
    ("left", "right"),
    ("bottom", "top"),
    ("top", "bottom"),
    ("right", "top"),
    ("right", "bottom"),
    ("left", "top"),
    ("left", "bottom"),
    ("bottom", "left"),
    ("bottom", "right"),
    ("top", "left"),
    ("top", "right"),
]


def pick_port_pair(
    from_ports: Mapping[str, Point], to_ports: Mapping[str, Point]
) -> dict[str, Any]:
    """Pick the shortest-distance port pair, biased toward horizontal pairs.

    Horizontal-to-horizontal pairs get a 0.7 multiplier on their score, and
    since lower score wins, the layout prefers them when the result is close.
    Returns ``{from_side, to_side, a, b}`` where ``a`` and ``b`` are the
    chosen points.
    """
    best: dict[str, Any] | None = None
    for fs, ts in _PORT_CANDIDATES:
        a = from_ports[fs]
        b = to_ports[ts]
        d = math.hypot(b["x"] - a["x"], b["y"] - a["y"])
        horiz_pair = fs in ("left", "right") and ts in ("left", "right")
        score = d * (0.7 if horiz_pair else 1.0)
        if best is None or score < best["score"]:
            best = {"from_side": fs, "to_side": ts, "a": a, "b": b, "score": score}
    assert best is not None
    return best


def cross_flow_path(
    from_ports: Mapping[str, Point], to_ports: Mapping[str, Point]
) -> dict[str, Any]:
    """Pick best ports and return the cross-flow bezier path + endpoints.

    Returns ``{d, a, b, from_side, to_side}`` where ``d`` is the SVG path
    string and ``a``/``b`` are the source/target endpoint coords (templates
    use them for the hub-dot circles at each end).
    """
    pair = pick_port_pair(from_ports, to_ports)
    a, b = pair["a"], pair["b"]
    offset = max(70, math.hypot(b["x"] - a["x"], b["y"] - a["y"]) * 0.32)
    n_a = PORT_NORMAL[pair["from_side"]]
    n_b = PORT_NORMAL[pair["to_side"]]
    d = (
        f"M {a['x']} {a['y']} "
        f"C {a['x'] + n_a['x'] * offset} {a['y'] + n_a['y'] * offset}, "
        f"{b['x'] + n_b['x'] * offset} {b['y'] + n_b['y'] * offset}, "
        f"{b['x']} {b['y']}"
    )
    return {
        "d": d,
        "a": a,
        "b": b,
        "from_side": pair["from_side"],
        "to_side": pair["to_side"],
    }
