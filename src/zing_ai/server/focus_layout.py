"""Compute focus layout: predecessors left, focused centre, successors right.

Port of prototypes/plan-viz/v4/convene.html's computeFocusLayout (positioning)
and fitToCluster (viewport math).
"""

from __future__ import annotations

from typing import Any, TypedDict

FOCUS_GAP_X = 80
FOCUS_CARD_GAP_Y = 60
GRID_GAP_Y = 60
FIT_PADDING = 80
SCALE_MIN = 0.06
SCALE_MAX = 1.4

# Default-grid camera. The first paint and post-release view both use these
# so they stop disagreeing. ``DEFAULT_PAN_Y`` is the top padding (in SVG user
# units) above the first card; ``DEFAULT_SCALE`` is the zoom level that lets
# a typical 10-15 step plan fit on a 1400 px-tall stage.
DEFAULT_PAN_Y = 40
DEFAULT_SCALE = 0.4


class CardPosition(TypedDict):
    x: float
    y: float


class ViewportFit(TypedDict):
    """Return type of fit_to_cluster: pan + scale to send in SSE.patch_signals."""

    pan: CardPosition
    scale: float


def step_size(step: dict[str, Any]) -> dict[str, float]:
    """Derive card width/height from a step's laid-out viewBox.

    layout.layout_graph annotates every step with viewBox = [x, y, w, h].
    The card chrome adds 70px header above the inner DAG, so card height is
    viewBox[3] + 70. Card width matches viewBox[2] (no horizontal chrome).
    """
    vb = step["viewBox"]
    return {"w": vb[2], "h": vb[3] + 70}


def compute(graph: dict, focused_step: int) -> dict[int, CardPosition]:
    """Return step → position mapping for focus mode."""
    preds = sorted(
        {cf["from_step"] for cf in graph.get("cross_flows", []) if cf["to_step"] == focused_step}
    )
    succs = sorted(
        {cf["to_step"] for cf in graph.get("cross_flows", []) if cf["from_step"] == focused_step}
    )
    sizes = {s["step"]: step_size(s) for s in graph["steps"]}
    focus_size = sizes[focused_step]
    layout: dict[int, CardPosition] = {
        focused_step: {"x": -focus_size["w"] / 2, "y": -focus_size["h"] / 2},
    }

    pred_total = sum(sizes[s]["h"] for s in preds) + max(0, len(preds) - 1) * FOCUS_CARD_GAP_Y
    py = -pred_total / 2
    for s in preds:
        sz = sizes[s]
        layout[s] = {"x": -focus_size["w"] / 2 - FOCUS_GAP_X - sz["w"], "y": py}
        py += sz["h"] + FOCUS_CARD_GAP_Y

    succ_total = sum(sizes[s]["h"] for s in succs) + max(0, len(succs) - 1) * FOCUS_CARD_GAP_Y
    sy = -succ_total / 2
    for s in succs:
        sz = sizes[s]
        layout[s] = {"x": focus_size["w"] / 2 + FOCUS_GAP_X, "y": sy}
        sy += sz["h"] + FOCUS_CARD_GAP_Y

    placed = set(layout.keys())
    grid = default_grid(graph)
    for step_num, pos in grid.items():
        if step_num not in placed:
            layout[step_num] = pos
    return layout


def default_camera(viewport_w: float) -> ViewportFit:
    """Return the camera (pan + scale) for the default-grid view.

    Used by both the initial GET (rendered into ``data-signals``) and
    ``plan_release`` (sent as SSE patch signals) so the two paths always
    produce the same view. ``pan.x`` centres the cards horizontally in the
    viewport; ``pan.y`` and ``scale`` are constants.

    Viewport height isn't needed: the default grid scrolls vertically rather
    than fitting all cards into the viewport. If we ever want fit-to-content
    on first paint, this is the place to compute it.
    """
    return {
        "pan": {"x": viewport_w / 2, "y": float(DEFAULT_PAN_Y)},
        "scale": DEFAULT_SCALE,
    }


def default_grid(graph: dict) -> dict[int, CardPosition]:
    """Position every step in a single vertical column, top-to-bottom by step number."""
    sizes = {s["step"]: step_size(s) for s in graph["steps"]}
    layout: dict[int, CardPosition] = {}
    y = 0.0
    for step in sorted(graph["steps"], key=lambda s: s["step"]):
        sz = sizes[step["step"]]
        layout[step["step"]] = {"x": -sz["w"] / 2, "y": y}
        y += sz["h"] + GRID_GAP_Y
    return layout


def fit_to_cluster(
    graph: dict,
    positions: dict[int, CardPosition],
    focused_step: int,
    viewport_w: float,
    viewport_h: float,
) -> ViewportFit:
    """Compute pan/scale so the focused step + its direct preds/succs fit in viewport.

    Direct port of prototypes/plan-viz/v4/convene.html lines 952–975. Steps
    outside the cluster (unconnected, faded) are NOT in the fit calculation —
    they stay positioned but the viewport ignores them.
    """
    cluster_steps = {focused_step}
    for cf in graph.get("cross_flows", []):
        if cf["to_step"] == focused_step:
            cluster_steps.add(cf["from_step"])
        if cf["from_step"] == focused_step:
            cluster_steps.add(cf["to_step"])

    sizes = {s["step"]: step_size(s) for s in graph["steps"]}

    min_x = min_y = float("inf")
    max_x = max_y = float("-inf")
    for step_num in cluster_steps:
        pos = positions.get(step_num)
        if pos is None:
            continue
        sz = sizes[step_num]
        min_x = min(min_x, pos["x"])
        min_y = min(min_y, pos["y"])
        max_x = max(max_x, pos["x"] + sz["w"])
        max_y = max(max_y, pos["y"] + sz["h"])

    if not (min_x < float("inf")):
        return {"pan": {"x": 0.0, "y": 0.0}, "scale": 1.0}

    w = max_x - min_x
    h = max_y - min_y

    # Degenerate viewport (e.g. layout has not measured yet) — fall back to identity.
    if viewport_w <= FIT_PADDING * 2 or viewport_h <= FIT_PADDING * 2 or w <= 0 or h <= 0:
        return {"pan": {"x": 0.0, "y": 0.0}, "scale": 1.0}

    sx = (viewport_w - FIT_PADDING * 2) / w
    sy = (viewport_h - FIT_PADDING * 2) / h
    new_scale = max(SCALE_MIN, min(SCALE_MAX, min(sx, sy)))
    cx = (min_x + max_x) / 2
    cy = (min_y + max_y) / 2
    return {
        "pan": {
            "x": viewport_w / 2 - cx * new_scale,
            "y": viewport_h / 2 - cy * new_scale,
        },
        "scale": new_scale,
    }
