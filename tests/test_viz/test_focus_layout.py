"""Tests for focus_layout (compute, default_grid, fit_to_cluster)."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from zing_ai.server import focus_layout as fl
from zing_ai.viz import layout as viz_layout

FIXTURE = Path(__file__).parent / "fixtures" / "BAK-1321" / "BAK-1321-direct-flatten.viz.json"


@pytest.fixture
def laid_out_graph() -> dict:
    return viz_layout.layout_graph(json.loads(FIXTURE.read_text()))


def test_default_grid_positions_steps_in_vertical_column(laid_out_graph: dict) -> None:
    positions = fl.default_grid(laid_out_graph)
    sorted_steps = sorted(laid_out_graph["steps"], key=lambda s: s["step"])
    # Each successive step is strictly below the previous one.
    for prev, curr in zip(sorted_steps, sorted_steps[1:], strict=False):
        prev_pos = positions[prev["step"]]
        curr_pos = positions[curr["step"]]
        assert curr_pos["y"] > prev_pos["y"]
    # All cards are horizontally centred at x = -w/2.
    for step in laid_out_graph["steps"]:
        sz = fl._step_size(step)
        assert positions[step["step"]]["x"] == -sz["w"] / 2


def test_compute_places_predecessors_left_focused_centre_successors_right(
    laid_out_graph: dict,
) -> None:
    # Step 6 has predecessors (4) and successors (7, 8).
    focused = 6
    positions = fl.compute(laid_out_graph, focused)

    preds = sorted(
        {cf["from_step"] for cf in laid_out_graph["cross_flows"] if cf["to_step"] == focused}
    )
    succs = sorted(
        {cf["to_step"] for cf in laid_out_graph["cross_flows"] if cf["from_step"] == focused}
    )
    assert preds, "fixture must have at least one predecessor for step 6"
    assert succs, "fixture must have at least one successor for step 6"

    focused_x = positions[focused]["x"]
    for p in preds:
        assert positions[p]["x"] < focused_x, f"pred {p} should be left of focused"
    for s in succs:
        assert positions[s]["x"] > focused_x, f"succ {s} should be right of focused"


def test_compute_centres_focused_step_at_origin(laid_out_graph: dict) -> None:
    focused = 6
    positions = fl.compute(laid_out_graph, focused)
    step = next(s for s in laid_out_graph["steps"] if s["step"] == focused)
    sz = fl._step_size(step)
    # Focused step: top-left corner is at (-w/2, -h/2) so center is at (0, 0)
    assert positions[focused]["x"] == -sz["w"] / 2
    assert positions[focused]["y"] == -sz["h"] / 2


def test_fit_to_cluster_scale_clamps_between_min_and_max(laid_out_graph: dict) -> None:
    focused = 6
    positions = fl.compute(laid_out_graph, focused)

    # Tiny viewport → scale clamps to SCALE_MIN
    fit_tiny = fl.fit_to_cluster(laid_out_graph, positions, focused, 200, 200)
    assert fit_tiny["scale"] == fl.SCALE_MIN

    # Huge viewport → scale clamps to SCALE_MAX
    fit_huge = fl.fit_to_cluster(laid_out_graph, positions, focused, 999_999, 999_999)
    assert fit_huge["scale"] == fl.SCALE_MAX


def test_fit_to_cluster_centres_cluster_in_viewport(laid_out_graph: dict) -> None:
    focused = 6
    positions = fl.compute(laid_out_graph, focused)
    fit = fl.fit_to_cluster(laid_out_graph, positions, focused, 1600, 900)
    # The post-transform cluster centre should land at viewport centre.
    # pan.x = viewport_w/2 - cx * scale  →  cx * scale + pan.x = viewport_w/2
    # So (cx * scale + pan.x) ≈ 800 and (cy * scale + pan.y) ≈ 450.
    cluster_steps = {focused}
    for cf in laid_out_graph["cross_flows"]:
        if cf["to_step"] == focused:
            cluster_steps.add(cf["from_step"])
        if cf["from_step"] == focused:
            cluster_steps.add(cf["to_step"])
    sizes = {s["step"]: fl._step_size(s) for s in laid_out_graph["steps"]}
    xs = [positions[s]["x"] for s in cluster_steps]
    ys = [positions[s]["y"] for s in cluster_steps]
    xs_max = [positions[s]["x"] + sizes[s]["w"] for s in cluster_steps]
    ys_max = [positions[s]["y"] + sizes[s]["h"] for s in cluster_steps]
    cx = (min(xs) + max(xs_max)) / 2
    cy = (min(ys) + max(ys_max)) / 2
    assert abs(cx * fit["scale"] + fit["pan"]["x"] - 800) < 1e-6
    assert abs(cy * fit["scale"] + fit["pan"]["y"] - 450) < 1e-6


def test_fit_to_cluster_returns_identity_for_degenerate_viewport(
    laid_out_graph: dict,
) -> None:
    positions = fl.compute(laid_out_graph, 6)
    fit = fl.fit_to_cluster(laid_out_graph, positions, 6, 0, 0)
    assert fit["scale"] == 1.0
    assert fit["pan"] == {"x": 0.0, "y": 0.0}


def test_compute_includes_unconnected_steps_in_default_positions(
    laid_out_graph: dict,
) -> None:
    focused = 6
    positions = fl.compute(laid_out_graph, focused)
    # Every step in the graph gets a position; unconnected ones fall back to grid.
    for step in laid_out_graph["steps"]:
        assert step["step"] in positions
