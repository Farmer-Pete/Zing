"""Click command group for simulating MCP tool calls against a running Zing server."""

from __future__ import annotations

import asyncio
import json
from pathlib import Path

import click

_STATE_FILE = Path.home() / ".zing-ai-sim.json"


def _load_state() -> dict:
    """Read and parse the sim state file."""
    try:
        return json.loads(_STATE_FILE.read_text())
    except (FileNotFoundError, json.JSONDecodeError):
        raise click.ClickException(
            "No active sim session. Run 'zing-ai sim create' first."
        ) from None


def _save_state(data: dict) -> None:
    """Write state dict as JSON to the sim state file."""
    _STATE_FILE.write_text(json.dumps(data, indent=2) + "\n")


def _resolve_step(state: dict, step_name: str) -> str:
    """Look up a step name in state and return the step_id."""
    steps = state.get("steps", {})
    if step_name not in steps:
        available = ", ".join(steps)
        raise click.ClickException(f"Unknown step '{step_name}'. Available: {available}")
    return steps[step_name]


async def _call_mcp_async(url: str, tool_name: str, arguments: dict) -> dict:
    """Call an MCP tool via the streamable HTTP client."""
    from mcp import ClientSession
    from mcp.client.streamable_http import streamable_http_client
    from mcp.types import TextContent

    async with (
        streamable_http_client(url) as (read_stream, write_stream, _),
        ClientSession(read_stream, write_stream) as session,
    ):
        await session.initialize()
        result = await session.call_tool(tool_name, arguments=arguments)
        content = result.content[0]
        if not isinstance(content, TextContent):
            msg = f"Unexpected content type: {type(content)}"
            raise TypeError(msg)
        return json.loads(content.text)


def _call_mcp(url: str, tool_name: str, arguments: dict) -> dict:
    """Sync wrapper around the async MCP client."""
    try:
        return asyncio.run(_call_mcp_async(url, tool_name, arguments))
    except (ConnectionRefusedError, OSError) as exc:
        raise click.ClickException(
            f"Could not connect to Zing server at {url}. Is 'zing-ai mcp' running?"
        ) from exc


@click.group()
@click.option("--url", default=None, help="MCP server URL.")
@click.pass_context
def sim(ctx: click.Context, *, url: str | None) -> None:
    """Simulate MCP tool calls against a running Zing server."""
    ctx.ensure_object(dict)
    if url is None:
        try:
            state = _load_state()
            url = state.get("url", "http://localhost:9876/mcp")
        except click.ClickException:
            url = "http://localhost:9876/mcp"
    ctx.obj["url"] = url


@sim.command()
@click.argument("title")
@click.option("--steps", default=None, help="Comma-separated list of step names.")
@click.pass_context
def create(ctx: click.Context, title: str, steps: str | None) -> None:
    """Create a new session on the MCP server."""
    url = ctx.obj["url"]
    parsed_steps: list[str] | None = steps.split(",") if steps else None
    result = _call_mcp(url, "session_create", {"title": title, "steps": parsed_steps})
    _save_state({
        "session_id": result["session_id"],
        "steps": result["steps"],
        "url": url,
    })
    click.echo(json.dumps(result, indent=2))


@sim.command()
@click.option("--title", default=None, help="New session title.")
@click.option("--zing-file", default=None, help="Path to zing file.")
@click.pass_context
def update(ctx: click.Context, title: str | None, zing_file: str | None) -> None:
    """Update an existing session on the MCP server."""
    state = _load_state()
    args: dict = {"session_id": state["session_id"]}
    if title is not None:
        args["title"] = title
    if zing_file is not None:
        args["zing_file"] = zing_file
    result = _call_mcp(ctx.obj["url"], "session_update", args)
    click.echo(json.dumps(result, indent=2))


@sim.command()
@click.argument("step")
@click.pass_context
def start(ctx: click.Context, step: str) -> None:
    """Start a step on the MCP server."""
    state = _load_state()
    step_id = _resolve_step(state, step)
    result = _call_mcp(ctx.obj["url"], "step_start", {
        "session_id": state["session_id"],
        "step_id": step_id,
    })
    click.echo(json.dumps(result, indent=2))


@sim.command("agent-start")
@click.argument("step")
@click.argument("name")
@click.option("--desc", default="", help="Agent description.")
@click.pass_context
def agent_start_cmd(ctx: click.Context, step: str, name: str, desc: str) -> None:
    """Register a running agent for a step."""
    state = _load_state()
    step_id = _resolve_step(state, step)
    result = _call_mcp(ctx.obj["url"], "agent_start", {
        "session_id": state["session_id"],
        "step_id": step_id,
        "name": name,
        "description": desc,
    })
    click.echo(json.dumps(result, indent=2))


@sim.command("agent-stop")
@click.argument("step")
@click.argument("name")
@click.pass_context
def agent_stop_cmd(ctx: click.Context, step: str, name: str) -> None:
    """Mark an agent as completed."""
    state = _load_state()
    step_id = _resolve_step(state, step)
    result = _call_mcp(ctx.obj["url"], "agent_stop", {
        "session_id": state["session_id"],
        "step_id": step_id,
        "name": name,
    })
    click.echo(json.dumps(result, indent=2))


@sim.command()
@click.argument("step")
@click.argument("name")
@click.argument("message")
@click.pass_context
def log(ctx: click.Context, step: str, name: str, message: str) -> None:
    """Log a message from an agent."""
    state = _load_state()
    step_id = _resolve_step(state, step)
    result = _call_mcp(ctx.obj["url"], "step_log", {
        "session_id": state["session_id"],
        "step_id": step_id,
        "agent_name": name,
        "message": message,
    })
    click.echo(json.dumps(result, indent=2))
