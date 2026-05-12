"""Routes for the plan-detail surface — detail, focus, release.

Mounted under /command-center/<session_id>/plan. No list page.
"""

from __future__ import annotations

from typing import Any

from datastar_py.consts import ElementPatchMode
from datastar_py.fastapi import datastar_response
from datastar_py.sse import ServerSentEventGenerator as SSE
from fastapi import APIRouter, Request
from fastapi.responses import HTMLResponse

from zing_ai.server import focus_layout as focus_layout_mod
from zing_ai.server import geometry as geom
from zing_ai.server.plan_loader import load_plan_for_session
from zing_ai.server.templates import render, render_markdown
from zing_ai.viz import layout as viz_layout

router = APIRouter()

HEADER_H = 70


def _annotate_step_geometry(step: dict[str, Any]) -> dict[str, Any]:
    """Return a copy of *step* with per-node outline_path and per-edge path."""
    new_nodes = []
    for n in step["nodes"]:
        annotated = dict(n)
        if n["shape"] == "diverged":
            annotated["diverged"] = geom.diverged_paths(n)
        else:
            annotated["outline_path"] = geom.node_outline(n)
        new_nodes.append(annotated)
    by_id = {n["id"]: n for n in step["nodes"]}
    new_edges = []
    for e in step["edges"]:
        annotated = dict(e)
        annotated["path"] = geom.inner_edge_path(e, by_id[e["from"]], by_id[e["to"]])
        new_edges.append(annotated)
    return {**step, "nodes": new_nodes, "edges": new_edges}


def _step_ports(
    step: dict[str, Any], position: focus_layout_mod.CardPosition
) -> dict[str, dict[str, geom.Point]]:
    """Compute absolute port positions for every node in a step.

    Returns ``{node_id: {left: Point, right: Point, top: Point, bottom: Point}}``.
    Cards are not per-card-scaled in the current render (scale = 1.0). The
    viewBox origin is subtracted so node coordinates align with the card's
    inner-DAG translate.
    """
    vb = step["viewBox"]
    ports: dict[str, dict[str, geom.Point]] = {}
    for n in step["nodes"]:
        ix = position["x"] + n["x"] - vb[0]
        iy = position["y"] + HEADER_H + n["y"] - vb[1]
        iw, ih = n["w"], n["h"]
        cx, cy = ix + iw / 2, iy + ih / 2
        ports[n["id"]] = {
            "left": {"x": ix, "y": cy},
            "right": {"x": ix + iw, "y": cy},
            "top": {"x": cx, "y": iy},
            "bottom": {"x": cx, "y": iy + ih},
        }
    return ports


def _annotate_cross_flows(
    graph: dict[str, Any],
    positions: dict[int, focus_layout_mod.CardPosition],
    focused_step: int | None,
) -> list[dict[str, Any]]:
    """Return cross_flows with d/a/b annotated, filtered to those touching focused_step.

    When ``focused_step`` is None, every cross-flow is included.
    """
    step_by_num = {s["step"]: s for s in graph["steps"]}
    port_cache: dict[int, dict[str, dict[str, geom.Point]]] = {}

    def _ports(step_num: int) -> dict[str, dict[str, geom.Point]]:
        if step_num not in port_cache:
            port_cache[step_num] = _step_ports(step_by_num[step_num], positions[step_num])
        return port_cache[step_num]

    annotated: list[dict[str, Any]] = []
    for cf in graph.get("cross_flows", []):
        if focused_step is not None and (
            cf["from_step"] != focused_step and cf["to_step"] != focused_step
        ):
            continue
        from_ports = _ports(cf["from_step"])[cf["from_node"]]
        to_ports = _ports(cf["to_step"])[cf["to_node"]]
        result = geom.cross_flow_path(from_ports, to_ports)
        annotated.append({**cf, "d": result["d"], "a": result["a"], "b": result["b"]})
    return annotated


def _step_with_layout(
    step: dict[str, Any],
    position: focus_layout_mod.CardPosition,
    focused_step: int | None,
    connected_preds: set[int],
    connected_succs: set[int],
) -> dict[str, Any]:
    """Return *step* augmented with position, card dimensions, and class state."""
    sz = focus_layout_mod._step_size(step)
    if focused_step is None:
        class_state = "default"
    elif step["step"] == focused_step:
        class_state = "focused"
    elif step["step"] in connected_preds:
        class_state = "pred"
    elif step["step"] in connected_succs:
        class_state = "succ"
    else:
        class_state = "faded"
    return {
        **step,
        "position": position,
        "card_w": sz["w"],
        "card_h": sz["h"],
        "scale": 1.0,
        "class_state": class_state,
    }


def _connected_steps(graph: dict[str, Any], focused_step: int | None) -> tuple[set[int], set[int]]:
    if focused_step is None:
        return set(), set()
    preds = {
        cf["from_step"] for cf in graph.get("cross_flows", []) if cf["to_step"] == focused_step
    }
    succs = {
        cf["to_step"] for cf in graph.get("cross_flows", []) if cf["from_step"] == focused_step
    }
    return preds, succs


def _build_render_context(
    graph: dict[str, Any],
    positions: dict[int, focus_layout_mod.CardPosition],
    focused_step: int | None,
) -> dict[str, Any]:
    """Return everything templates need: annotated steps + cross_flows."""
    preds, succs = _connected_steps(graph, focused_step)
    geom_annotated = [_annotate_step_geometry(s) for s in graph["steps"]]
    laid_out_steps = [
        _step_with_layout(s, positions[s["step"]], focused_step, preds, succs)
        for s in geom_annotated
    ]
    cross_flows = _annotate_cross_flows(graph, positions, focused_step)
    return {
        "steps": laid_out_steps,
        "cross_flows": cross_flows,
        "focused_step": focused_step,
        "kinds": graph.get("kinds", {}),
    }


@router.get("/command-center/{session_id}/plan", response_class=HTMLResponse)
async def plan_detail(session_id: str, request: Request) -> HTMLResponse:
    """Render the plan-detail page: markdown left, Convene viewer right."""
    sm = request.app.state.session_manager
    md_text, graph = load_plan_for_session(session_id, sm)
    laid_out = viz_layout.layout_graph(graph)
    positions = focus_layout_mod.default_grid(laid_out)
    context = _build_render_context(laid_out, positions, focused_step=None)
    return HTMLResponse(
        render(
            "plan_detail.html",
            session_id=session_id,
            rendered_markdown=render_markdown(md_text),
            **context,
        )
    )


@router.post("/command-center/{session_id}/plan/focus")
@datastar_response
async def plan_focus(session_id: str, payload: dict[str, Any], request: Request):  # noqa: ANN201
    """Enter focus mode on a step. Returns SSE patches."""

    async def _stream():  # noqa: ANN202
        sm = request.app.state.session_manager
        _, graph = load_plan_for_session(session_id, sm)
        laid_out = viz_layout.layout_graph(graph)
        focused_step = int(payload["step"])
        viewport_w = float(payload.get("viewport", {}).get("w") or 1600)
        viewport_h = float(payload.get("viewport", {}).get("h") or 900)
        positions = focus_layout_mod.compute(laid_out, focused_step)
        fit = focus_layout_mod.fit_to_cluster(
            laid_out, positions, focused_step, viewport_w, viewport_h
        )
        context = _build_render_context(laid_out, positions, focused_step=focused_step)
        yield SSE.patch_signals(
            {
                "focusedStep": focused_step,
                "pan": {"x": fit["pan"]["x"], "y": fit["pan"]["y"]},
                "scale": fit["scale"],
            }
        )
        for step in context["steps"]:
            yield SSE.patch_elements(
                render(
                    "viz/_card.html",
                    step=step,
                    session_id=session_id,
                    focused_step=focused_step,
                ),
                selector=f"#card-{step['step']}",
                mode=ElementPatchMode.OUTER,
            )
        yield SSE.patch_elements(
            render(
                "viz/_xflow_layer.html",
                cross_flows=context["cross_flows"],
            ),
            selector="#xflow-layer",
            mode=ElementPatchMode.OUTER,
        )

    return _stream()


@router.post("/command-center/{session_id}/plan/release")
@datastar_response
async def plan_release(session_id: str, request: Request):  # noqa: ANN201
    """Exit focus mode — restore default-grid layout."""

    async def _stream():  # noqa: ANN202
        sm = request.app.state.session_manager
        _, graph = load_plan_for_session(session_id, sm)
        laid_out = viz_layout.layout_graph(graph)
        positions = focus_layout_mod.default_grid(laid_out)
        context = _build_render_context(laid_out, positions, focused_step=None)
        yield SSE.patch_signals(
            {
                "focusedStep": "",
                "pan": {"x": 0, "y": 0},
                "scale": 0.4,
            }
        )
        for step in context["steps"]:
            yield SSE.patch_elements(
                render(
                    "viz/_card.html",
                    step=step,
                    session_id=session_id,
                    focused_step=None,
                ),
                selector=f"#card-{step['step']}",
                mode=ElementPatchMode.OUTER,
            )
        yield SSE.patch_elements(
            render(
                "viz/_xflow_layer.html",
                cross_flows=context["cross_flows"],
            ),
            selector="#xflow-layer",
            mode=ElementPatchMode.OUTER,
        )

    return _stream()
