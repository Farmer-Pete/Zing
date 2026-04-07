"""Route handlers for the /install UI page and install runner."""

from __future__ import annotations

import asyncio
from pathlib import Path

from datastar_py import ServerSentEventGenerator as SSE
from datastar_py.fastapi import datastar_response
from fastapi import APIRouter
from fastapi.responses import HTMLResponse

from zing_ai.config import load_config
from zing_ai.installer import InstallError, install_claude, install_opencode, is_install_stale
from zing_ai.manifest import load_manifest
from zing_ai.server.templates import render

router = APIRouter()


def _install_target_for(runtime: str) -> Path:
    """Return the default install target directory for the given runtime."""
    if runtime == "claude":
        return Path.home() / ".claude" / "commands"
    if runtime == "opencode":
        return Path.home() / ".config" / "opencode" / "commands"
    raise ValueError(f"unknown runtime: {runtime}")


@router.post("/install/run")
@datastar_response
async def post_run_install(payload: dict[str, str]):  # noqa: ANN201
    """Run the installer for the given runtime and return a status fragment."""
    runtime = payload.get("runtime", "")

    async def _generate():  # noqa: ANN202
        if runtime not in ("claude", "opencode"):
            yield SSE.patch_elements(
                f"<div class='error'>unknown runtime: {runtime}</div>",
                selector="#install-status-error",
            )
            return
        cfg = load_config()
        target_dir = _install_target_for(runtime)
        install_fn = install_claude if runtime == "claude" else install_opencode
        try:
            await asyncio.to_thread(install_fn, target_dir=target_dir, config=cfg)
        except InstallError as e:
            manifest = load_manifest(target_dir)
            status = {
                "runtime": runtime,
                "stale": True,
                "installed_at": manifest.get("installed_at") if manifest else None,
                "target_dir": str(target_dir),
                "error": str(e),
            }
            yield SSE.patch_elements(
                render("fragments/install_status.html", status=status),
                selector=f"#install-status-{runtime}",
            )
            return
        # Success — re-render with fresh status
        manifest = load_manifest(target_dir)
        status = {
            "runtime": runtime,
            "stale": is_install_stale(target_dir, runtime, cfg),
            "installed_at": manifest.get("installed_at") if manifest else None,
            "target_dir": str(target_dir),
        }
        yield SSE.patch_elements(
            render("fragments/install_status.html", status=status),
            selector=f"#install-status-{runtime}",
        )

    return _generate()


@router.get("/install")
def get_install_page() -> HTMLResponse:
    """Return the install page HTML."""
    cfg = load_config()
    statuses = []
    for runtime in ("claude", "opencode"):
        target_dir = _install_target_for(runtime)
        manifest = load_manifest(target_dir)
        statuses.append(
            {
                "runtime": runtime,
                "stale": is_install_stale(target_dir, runtime, cfg),
                "installed_at": manifest.get("installed_at") if manifest else None,
                "target_dir": str(target_dir),
            }
        )
    return HTMLResponse(render("install.html", statuses=statuses, current_path="/install"))
