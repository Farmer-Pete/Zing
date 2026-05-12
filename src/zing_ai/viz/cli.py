"""CLI for the viz module: zing-ai viz {validate, layout}."""

from __future__ import annotations

import json
import sys
from pathlib import Path

import click

from zing_ai.viz import layout, validate


def _slug_to_paths(slug: str) -> tuple[Path, Path]:
    """Resolve a plan slug to (markdown_path, viz_path) under .zing/."""
    zing_dir = Path.cwd() / ".zing"
    return (zing_dir / f"{slug}.md", zing_dir / f"{slug}.viz.json")


@click.group("viz")
def viz() -> None:
    """Plan visualization commands."""


@viz.command("validate")
@click.argument("slug")
def viz_validate(slug: str) -> None:
    """Validate a plan's viz JSON against the schema and cross-references."""
    _, viz_path = _slug_to_paths(slug)
    if not viz_path.exists():
        click.echo(f"error: {viz_path} does not exist", err=True)
        sys.exit(2)
    graph = json.loads(viz_path.read_text())
    errors = validate.validate(graph)
    if errors:
        for err in errors:
            click.echo(err.format(str(viz_path)), err=True)
        sys.exit(1)
    click.echo(
        f"ok · {len(graph['steps'])} steps · {len(graph.get('cross_flows', []))} cross-flows"
    )


@viz.command("layout")
@click.argument("slug")
@click.option(
    "--output",
    "-o",
    default="-",
    help="Output path; '-' for stdout (default).",
)
def viz_layout(slug: str, output: str) -> None:
    """Run Graphviz layout on a plan's viz JSON. Prints to stdout by default."""
    _, viz_path = _slug_to_paths(slug)
    if not viz_path.exists():
        click.echo(f"error: {viz_path} does not exist", err=True)
        sys.exit(2)
    graph = json.loads(viz_path.read_text())
    errors = validate.validate(graph)
    if errors:
        click.echo(f"error: {viz_path} has validation errors:", err=True)
        for err in errors:
            click.echo("  " + err.format(str(viz_path)), err=True)
        sys.exit(1)
    laid_out = layout.layout_graph(graph)
    text = json.dumps(laid_out, indent=2)
    if output == "-":
        click.echo(text)
    else:
        Path(output).write_text(text)
        click.echo(f"wrote {output}")
