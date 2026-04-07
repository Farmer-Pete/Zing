"""Route handlers for the /config UI page."""

from __future__ import annotations

from typing import Any

from fastapi import APIRouter
from fastapi.responses import HTMLResponse, JSONResponse
from filelock import Timeout
from pydantic import ValidationError

from zing_ai.config import load_config, save_config
from zing_ai.server.config_meta import FIELD_META
from zing_ai.server.templates import render

router = APIRouter()


@router.get("/config")
def get_config_page() -> HTMLResponse:
    """Return the configuration page HTML."""
    cfg = load_config()
    return HTMLResponse(
        render("config.html", config=cfg, field_meta=FIELD_META, current_path="/config")
    )


@router.post("/config/save/{category}")
def post_save_config(category: str, payload: dict[str, Any]) -> JSONResponse:
    """Save a config section by category name."""
    valid = {"thresholds", "models", "git", "agents", "report"}
    if category not in valid:
        return JSONResponse({"error": f"unknown category: {category}"}, status_code=400)
    cfg = load_config()
    section = getattr(cfg, category)
    try:
        new_section = section.model_copy(update=payload)
        # Force re-validation by re-instantiating the model from dump
        new_section = type(section).model_validate(new_section.model_dump())
    except ValidationError as e:
        return JSONResponse({"error": e.errors()[0]}, status_code=422)
    setattr(cfg, category, new_section)
    try:
        save_config(cfg)
    except Timeout:
        return JSONResponse({"error": "config is locked, try again"}, status_code=503)
    return JSONResponse({"status": "ok"})
