"""Routes for the plan-detail surface — detail, focus, release.

Mounted under /command-center/<session_id>/plan. No list page.
"""

from __future__ import annotations

from collections import OrderedDict
from pathlib import Path
from typing import Any, TypedDict

from datastar_py.consts import ElementPatchMode, ElementPatchNamespace
from datastar_py.fastapi import datastar_response
from datastar_py.sse import ServerSentEventGenerator as SSE
from fastapi import APIRouter, Request
from fastapi.responses import HTMLResponse

from zing_ai.server import focus_layout as focus_layout_mod
from zing_ai.server import geometry as geom
from zing_ai.server.plan_loader import load_plan_for_session
from zing_ai.server.sse_helpers import sse_toast
from zing_ai.server.templates import render, render_markdown
from zing_ai.viz import layout as viz_layout

router = APIRouter()

HEADER_H = 70


def _patch_svg(elements: str, selector: str) -> Any:
    """``patch_elements`` wrapper that pins the SVG namespace.

    Datastar treats patched markup as HTML by default; for SVG fragments
    that means children land in the XHTML namespace and don't render
    (zero-size <g>). Passing ``namespace=svg`` keeps the parsed elements
    in the SVG namespace so the browser actually paints them.

    The ``# pyright: ignore`` is intentional: the datastar-py SDK ships
    ``@overload`` declarations for ``patch_elements`` that predate the
    runtime ``namespace`` keyword. The implementation accepts it; the
    overloads don't, so static analysis reports a call-site error.
    Centralising the call here pins the suppression to one place.
    """
    return SSE.patch_elements(  # pyright: ignore[reportCallIssue]
        elements,
        selector=selector,
        mode=ElementPatchMode.OUTER,
        namespace=ElementPatchNamespace.SVG,
    )


# LRU cache for laid-out graphs keyed on (viz_path, mtime_ns). A typical
# focus → release click cycle re-uses the same layout three times; without
# caching that's 3× the dot subprocess cost for no new information. Cache
# size of 10 covers ~10 plans being viewed concurrently before the LRU
# starts evicting; well within memory budget for a localhost dev tool.
_LAYOUT_CACHE: OrderedDict[tuple[str, int], dict[str, Any]] = OrderedDict()
_LAYOUT_CACHE_MAX = 10


def _laid_out_graph(viz_path: Path, graph: dict[str, Any]) -> dict[str, Any]:
    """Return *graph* with per-step Graphviz layout applied; LRU-cached by mtime.

    A mtime bump (Claude rewriting the file) invalidates the entry on the next
    request because the cache key includes ``viz_path.stat().st_mtime_ns``.
    """
    key = (str(viz_path), viz_path.stat().st_mtime_ns)
    cached = _LAYOUT_CACHE.get(key)
    if cached is not None:
        _LAYOUT_CACHE.move_to_end(key)
        return cached
    laid_out = viz_layout.layout_graph(graph)
    _LAYOUT_CACHE[key] = laid_out
    if len(_LAYOUT_CACHE) > _LAYOUT_CACHE_MAX:
        _LAYOUT_CACHE.popitem(last=False)
    return laid_out


def _annotate_step_geometry(step: dict[str, Any]) -> dict[str, Any]:
    """Return a copy of *step* with per-node outline_path and per-edge path."""
    new_nodes = []
    for n in step["nodes"]:
        annotated = dict(n)
        if n["shape"] == "diverged":
            annotated["diverged"] = geom.diverged_paths(n)
        elif n["shape"] == "struct":
            annotated["struct"] = geom.struct_paths(n)
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
    sz = focus_layout_mod.step_size(step)
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


class RenderContext(TypedDict):
    """Everything the plan-detail templates need to render the viewer.

    Templates depend on all four keys; if you add another, surface it here so
    pyright catches missing populators.
    """

    steps: list[dict[str, Any]]
    cross_flows: list[dict[str, Any]]
    focused_step: int | None
    kinds: dict[str, Any]


def _build_render_context(
    graph: dict[str, Any],
    positions: dict[int, focus_layout_mod.CardPosition],
    focused_step: int | None,
) -> RenderContext:
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
    md_text, graph, viz_path = load_plan_for_session(session_id, sm)
    laid_out = _laid_out_graph(viz_path, graph)
    positions = focus_layout_mod.default_grid(laid_out)
    context = _build_render_context(laid_out, positions, focused_step=None)
    return HTMLResponse(
        render(
            "plan_detail.html",
            session_id=session_id,
            rendered_markdown=render_markdown(md_text),
            # Server-known default-grid camera defaults baked into data-signals
            # so the initial view and plan_release stay in sync. pan.x is
            # client-centred in data-init because the server doesn't know the
            # viewport width at first paint.
            default_pan_y=focus_layout_mod.DEFAULT_PAN_Y,
            default_scale=focus_layout_mod.DEFAULT_SCALE,
            **context,
        )
    )


@router.post("/command-center/{session_id}/plan/focus")
@datastar_response
async def plan_focus(session_id: str, payload: dict[str, Any], request: Request):  # noqa: ANN201
    """Enter focus mode on a step. Returns SSE patches."""

    async def _stream():  # noqa: ANN202
        try:
            sm = request.app.state.session_manager
            _, graph, viz_path = load_plan_for_session(session_id, sm)
            laid_out = _laid_out_graph(viz_path, graph)
            focused_step = int(payload["step"])
            viewport_w = float(payload.get("viewport", {}).get("w") or 1600)
            viewport_h = float(payload.get("viewport", {}).get("h") or 900)
            positions = focus_layout_mod.compute(laid_out, focused_step)
            fit = focus_layout_mod.fit_to_cluster(
                laid_out, positions, focused_step, viewport_w, viewport_h
            )
            context = _build_render_context(laid_out, positions, focused_step=focused_step)
        except (KeyError, ValueError, TypeError) as exc:
            # Bad payload (missing/non-numeric step), stale page (step removed
            # from graph), or load failure. Surface to user as a toast — never
            # leave a half-rendered view from a partial yield.
            yield sse_toast(f"Could not focus step: {exc}", "err")
            return
        yield SSE.patch_signals(
            {
                "focusedStep": focused_step,
                "pan": {"x": fit["pan"]["x"], "y": fit["pan"]["y"]},
                "scale": fit["scale"],
            }
        )
        for step in context["steps"]:
            yield _patch_svg(
                render(
                    "viz/_card.html",
                    step=step,
                    session_id=session_id,
                    focused_step=focused_step,
                ),
                f"#card-{step['step']}",
            )
        yield _patch_svg(
            render(
                "viz/_xflow_layer.html",
                cross_flows=context["cross_flows"],
            ),
            "#xflow-layer",
        )

    return _stream()


@router.post("/command-center/{session_id}/plan/release")
@datastar_response
async def plan_release(session_id: str, payload: dict[str, Any], request: Request):  # noqa: ANN201
    """Exit focus mode — restore default-grid layout."""

    async def _stream():  # noqa: ANN202
        try:
            sm = request.app.state.session_manager
            _, graph, viz_path = load_plan_for_session(session_id, sm)
            laid_out = _laid_out_graph(viz_path, graph)
            positions = focus_layout_mod.default_grid(laid_out)
            context = _build_render_context(laid_out, positions, focused_step=None)
            # Compute the same camera the initial GET produced so release
            # returns to the first-paint view, not a different (0, 0) origin.
            viewport_w = float(payload.get("viewport", {}).get("w") or 1600)
            camera = focus_layout_mod.default_camera(viewport_w)
        except (KeyError, ValueError, TypeError) as exc:
            yield sse_toast(f"Could not release focus: {exc}", "err")
            return
        yield SSE.patch_signals(
            {
                "focusedStep": "",
                "pan": {"x": camera["pan"]["x"], "y": camera["pan"]["y"]},
                "scale": camera["scale"],
            }
        )
        for step in context["steps"]:
            yield _patch_svg(
                render(
                    "viz/_card.html",
                    step=step,
                    session_id=session_id,
                    focused_step=None,
                ),
                f"#card-{step['step']}",
            )
        yield _patch_svg(
            render(
                "viz/_xflow_layer.html",
                cross_flows=context["cross_flows"],
            ),
            "#xflow-layer",
        )

    return _stream()
