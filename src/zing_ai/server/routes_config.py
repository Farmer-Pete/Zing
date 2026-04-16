"""Route handlers for the /config UI page."""

from __future__ import annotations

from typing import Any

from fastapi import APIRouter
from fastapi.responses import HTMLResponse, JSONResponse
from filelock import Timeout
from pydantic import ValidationError

from zing_ai.config import ConfigError, default_config, load_config, save_config
from zing_ai.server.config_meta import FIELD_META
from zing_ai.server.templates import render

router = APIRouter()


def _serialize_validation_error(e: ValidationError) -> dict[str, Any]:
    """Return a JSON-safe representation of the first ValidationError entry."""
    errors = e.errors()
    if not errors:
        return {"msg": "validation failed", "loc": [], "type": "value_error"}
    err = errors[0]
    return {
        "msg": err.get("msg", "invalid value"),
        "loc": list(err.get("loc", ())),
        "type": err.get("type", "value_error"),
    }


@router.get("/config")
def get_config_page() -> HTMLResponse:
    """Return the configuration page HTML."""
    config_error: str | None = None
    try:
        cfg = load_config()
    except ConfigError as e:
        config_error = str(e)
        cfg = default_config()
    return HTMLResponse(
        render(
            "config.html",
            config=cfg,
            field_meta=FIELD_META,
            current_path="/config",
            config_error=config_error,
        )
    )


@router.post("/config/save/{category}")
def post_save_config(category: str, payload: dict[str, Any]) -> JSONResponse:
    """Save a config section by category name."""
    valid = {"thresholds", "models", "git", "agents", "report", "command_center"}
    if category not in valid:
        return JSONResponse({"error": f"unknown category: {category}"}, status_code=400)
    try:
        cfg = load_config()
    except ConfigError as e:
        return JSONResponse({"error": str(e)}, status_code=422)
    section = getattr(cfg, category)
    section_cls = type(section)
    # Reject unknown payload keys so typo'd field names surface as 422.
    unknown = set(payload) - set(section_cls.model_fields)
    if unknown:
        return JSONResponse({"error": f"unknown fields: {sorted(unknown)}"}, status_code=422)
    try:
        # Validate by merging payload into the dumped section so type coercion runs.
        new_section = section_cls.model_validate({**section.model_dump(), **payload})
    except ValidationError as e:
        return JSONResponse({"error": _serialize_validation_error(e)}, status_code=422)
    setattr(cfg, category, new_section)
    try:
        save_config(cfg)
    except Timeout:
        return JSONResponse({"error": "config is locked, try again"}, status_code=503)
    return JSONResponse({"status": "ok"})
