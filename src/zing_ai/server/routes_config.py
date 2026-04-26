"""Route handlers for the /config UI page."""

from __future__ import annotations

import sys
from collections import OrderedDict
from typing import Any

from datastar_py import ServerSentEventGenerator as SSE
from datastar_py.fastapi import datastar_response
from fastapi import APIRouter, Request
from fastapi.responses import HTMLResponse, JSONResponse
from filelock import Timeout
from pydantic import ValidationError

from zing_ai.config import ConfigError, default_config, load_config, save_config
from zing_ai.server.config_meta import FIELD_META
from zing_ai.server.sse_helpers import sse_toast as _sse_toast
from zing_ai.server.templates import render

_PLATFORM_FIELD_META: dict[str, dict[str, Any]] = {
    key: meta
    for key, meta in FIELD_META.items()
    if "platform" not in meta or meta["platform"] == sys.platform
}


def _group_repos(
    repos: list[str], excluded: set[str]
) -> OrderedDict[str, list[dict[str, str | bool]]]:
    """Group repos by owner. Groups sorted by count desc, repos sorted alphabetically."""
    raw: dict[str, list[dict[str, str | bool]]] = {}
    for repo in repos:
        owner = repo.partition("/")[0]
        raw.setdefault(owner, []).append(
            {"name": repo, "short": repo.partition("/")[2], "enabled": repo not in excluded}
        )
    for items in raw.values():
        items.sort(key=lambda r: str(r["short"]).lower())
    # Largest groups first, then alphabetical owner for ties.
    ordered = OrderedDict(sorted(raw.items(), key=lambda kv: (-len(kv[1]), kv[0].lower())))
    return ordered


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
def get_config_page(request: Request) -> HTMLResponse:
    """Return the configuration page HTML."""
    config_error: str | None = None
    try:
        cfg = load_config()
    except ConfigError as e:
        config_error = str(e)
        cfg = default_config()
    cache = request.app.state.external_cache  # type: ignore[attr-defined]
    excluded = set(cfg.command_center.github_excluded_repos)
    github_repo_groups = _group_repos(cache.github_repos, excluded)
    # Initial signal state for Datastar bindings on the repo checkboxes.
    # Signal names must be valid JS identifiers — replace "/" with "__".
    repo_groups_signals: dict[str, bool] = {
        owner: all(r["enabled"] for r in repos) for owner, repos in github_repo_groups.items()
    }
    repos_signals: dict[str, bool] = {
        str(r["name"]).replace("/", "__"): bool(r["enabled"])
        for repos in github_repo_groups.values()
        for r in repos
    }
    return HTMLResponse(
        render(
            "config.html",
            config=cfg,
            field_meta=_PLATFORM_FIELD_META,
            current_path="/config",
            config_error=config_error,
            github_repo_groups=github_repo_groups,
            repo_groups_signals=repo_groups_signals,
            repos_signals=repos_signals,
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


@router.get("/config/github-repos")
def get_github_repos(request: Request) -> JSONResponse:
    """Return the list of writable repos with their enabled/excluded status."""
    cache = request.app.state.external_cache  # type: ignore[attr-defined]
    try:
        cfg = load_config()
    except ConfigError:
        cfg = default_config()
    excluded = set(cfg.command_center.github_excluded_repos)
    repos = [{"name": repo, "enabled": repo not in excluded} for repo in cache.github_repos]
    return JSONResponse({"repos": repos})


@router.post("/config/github-repos/toggle")
@datastar_response
async def post_toggle_github_repo(payload: dict[str, Any]):  # noqa: ANN201
    """Toggle a repo's inclusion in polling. Returns SSE so Datastar can process it."""
    repo = payload.get("repo", "")
    enabled = payload.get("enabled", True)

    async def _stream():  # noqa: ANN202
        if not repo:
            yield _sse_toast("repo is required", "err")
            return
        try:
            cfg = load_config()
        except ConfigError as e:
            yield _sse_toast(str(e), "err")
            return
        excluded = set(cfg.command_center.github_excluded_repos)
        if enabled:
            excluded.discard(repo)
        else:
            excluded.add(repo)
        cfg.command_center.github_excluded_repos = sorted(excluded)
        try:
            save_config(cfg)
        except Timeout:
            yield _sse_toast("config is locked, try again", "err")
            return
        sig_name = repo.replace("/", "__")
        yield SSE.patch_signals({"repos": {sig_name: bool(enabled)}})

    return _stream()


@router.post("/config/github-repos/toggle-group")
@datastar_response
async def post_toggle_github_repo_group(  # noqa: ANN201
    request: Request, payload: dict[str, Any]
):
    """Toggle all repos under an owner prefix. Returns SSE so Datastar can process it."""
    owner = payload.get("owner", "")
    enabled = payload.get("enabled", True)

    async def _stream():  # noqa: ANN202
        if not owner:
            yield _sse_toast("owner is required", "err")
            return
        cache = request.app.state.external_cache  # type: ignore[attr-defined]
        group_repos = [r for r in cache.github_repos if r.partition("/")[0] == owner]
        if not group_repos:
            yield _sse_toast(f"no repos for owner: {owner}", "err")
            return
        try:
            cfg = load_config()
        except ConfigError as e:
            yield _sse_toast(str(e), "err")
            return
        excluded = set(cfg.command_center.github_excluded_repos)
        for repo in group_repos:
            if enabled:
                excluded.discard(repo)
            else:
                excluded.add(repo)
        cfg.command_center.github_excluded_repos = sorted(excluded)
        try:
            save_config(cfg)
        except Timeout:
            yield _sse_toast("config is locked, try again", "err")
            return
        repos_signals = {r.replace("/", "__"): bool(enabled) for r in group_repos}
        yield SSE.patch_signals({"repoGroups": {owner: bool(enabled)}, "repos": repos_signals})

    return _stream()
