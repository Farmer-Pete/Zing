"""Serve the viz JSON Schema so slash commands can fetch it at runtime."""

from __future__ import annotations

import json

from fastapi import APIRouter
from fastapi.responses import JSONResponse

from zing_ai.viz.validate import SCHEMA_PATH

router = APIRouter()


@router.get("/viz/schema.json")
async def viz_schema() -> JSONResponse:
    """Return the viz graph JSON Schema verbatim. Unauthenticated, stable."""
    return JSONResponse(json.loads(SCHEMA_PATH.read_text()))
