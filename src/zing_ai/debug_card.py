"""Debug utility for Kanban card classification.

Fetches a live PR (via GitHub) and/or ticket (via Linear), runs the data
through the same :func:`aggregate` pipeline the Command Center uses,
constructs the canonical :class:`CardView` for the resulting card, and
prints a structured trace.

This module is a **dumb display**: it fetches data, calls production
functions, and prints what they return.  Display state comes from
``CardView`` via Pydantic introspection.  Diagnostic traces
(``_pr_needs_response``, ``_classify_card``) are emitted by the
production functions themselves via an optional ``trace`` parameter.
There is **no** classification, predicate, or display logic in this
file — by design.  See CLAUDE.md "Debugging Kanban classification".
"""

from __future__ import annotations

import asyncio
import re
from datetime import UTC, datetime
from typing import Any

import click
from pydantic import BaseModel

from zing_ai.config import load_config
from zing_ai.server.card_view import build_card_view
from zing_ai.server.command_center import (
    _DONE_WINDOW,
    _classify_card,
    _compute_signals,
    _pr_needs_response,
    aggregate,
)
from zing_ai.server.github_client import GitHubClient, _map_pr
from zing_ai.server.linear_client import LinearClient
from zing_ai.server.models_external import (
    GitHubPR,
    KanbanCard,
    KanbanColumn,
    KanbanView,
    LinearIssue,
)

_PR_URL_RE = re.compile(r"github\.com/([^/]+)/([^/]+)/pull/(\d+)")
_PR_HASH_RE = re.compile(r"^([^/]+/[^/#]+)#(\d+)$")
_PR_NUM_RE = re.compile(r"^#?(\d+)$")


_PR_QUERY = """
query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      number title state isDraft headRefName baseRefName body
      author { login }
      reviewDecision
      mergeable
      url
      updatedAt
      mergedAt
      reviewRequests(first: 10) {
        nodes { requestedReviewer { ... on User { login } } }
      }
      latestReviews(first: 10) {
        nodes { author { login } state }
      }
      commits(last: 1) {
        nodes {
          commit {
            statusCheckRollup {
              state
              contexts(first: 50) {
                nodes {
                  ... on CheckRun { name status conclusion detailsUrl }
                  ... on StatusContext { context state targetUrl }
                }
              }
            }
          }
        }
      }
    }
  }
}
"""


_ISSUE_QUERY = """
query($id: String!) {
  issue(id: $id) {
    id identifier title priority url updatedAt
    state { name type }
    assignee { name }
    team { name }
  }
}
"""


def _parse_pr_arg(arg: str, repo_default: str | None) -> tuple[str, int]:
    """Resolve ``--pr`` to an ``(owner/repo, number)`` tuple."""
    m = _PR_URL_RE.search(arg)
    if m:
        return f"{m.group(1)}/{m.group(2)}", int(m.group(3))
    m = _PR_HASH_RE.match(arg)
    if m:
        return m.group(1), int(m.group(2))
    m = _PR_NUM_RE.match(arg)
    if m:
        if not repo_default:
            raise click.ClickException(
                f"PR number {arg!r} requires --repo (e.g. --repo turngate/backend-v1)"
            )
        return repo_default, int(m.group(1))
    raise click.ClickException(f"Could not parse --pr {arg!r}")


async def _fetch_pr(github: GitHubClient, repo: str, number: int) -> GitHubPR:
    """Fetch a single PR by number using the same GraphQL shape as the poller."""
    owner, _, name = repo.partition("/")
    data = await github._graphql(  # noqa: SLF001 — debug-only utility
        _PR_QUERY, {"owner": owner, "repo": name, "number": number}
    )
    repo_data = data.get("repository") or {}
    pr_node = repo_data.get("pullRequest")
    if pr_node is None:
        raise click.ClickException(f"PR {repo}#{number} not found")
    return _map_pr(pr_node, repo=repo)


async def _fetch_ticket(linear: LinearClient, identifier: str) -> LinearIssue:
    """Fetch a single Linear issue by identifier (e.g. ``BAK-1179``)."""
    data = await linear._post(_ISSUE_QUERY, {"id": identifier})  # noqa: SLF001
    node = data.get("issue")
    if node is None:
        raise click.ClickException(f"Linear ticket {identifier!r} not found")
    raw = node["updatedAt"]
    if raw.endswith("Z"):
        raw = raw[:-1] + "+00:00"
    return LinearIssue(
        id=node["id"],
        identifier=node["identifier"],
        title=node["title"],
        state=node["state"]["name"],
        state_type=node["state"]["type"],
        priority=node.get("priority", 0),
        assignee=node["assignee"]["name"] if node.get("assignee") else None,
        team=node["team"]["name"] if node.get("team") else None,
        url=node["url"],
        updated_at=datetime.fromisoformat(raw),
    )


# ---------------------------------------------------------------------------
# Pydantic-introspection printer
# ---------------------------------------------------------------------------


def _format_value(value: Any, indent: int) -> list[str]:
    """Render *value* as one or more indented lines of the output report."""
    pad = "  " * indent
    if isinstance(value, BaseModel):
        return _format_model(value, indent)
    if isinstance(value, list):
        if not value:
            return [f"{pad}[]"]
        out: list[str] = []
        for i, item in enumerate(value):
            if isinstance(item, BaseModel):
                out.append(f"{pad}- [{i}]:")
                out.extend(_format_model(item, indent + 1))
            else:
                out.append(f"{pad}- [{i}]: {item!r}")
        return out
    if isinstance(value, dict):
        if not value:
            return [f"{pad}{{}}"]
        return [f"{pad}{k}: {v!r}" for k, v in value.items()]
    if isinstance(value, datetime):
        return [f"{pad}{value.isoformat()}"]
    return [f"{pad}{value!r}"]


def _format_model(model: BaseModel, indent: int = 0) -> list[str]:
    """Walk a Pydantic model's declared fields and render each one.

    Order matches the field-declaration order in the model — the same
    order ``model.model_fields`` returns.  Keeps output stable as
    fields are added.
    """
    pad = "  " * indent
    out: list[str] = []
    for name in type(model).model_fields:
        value = getattr(model, name)
        if isinstance(value, BaseModel):
            out.append(f"{pad}{name}:")
            out.extend(_format_model(value, indent + 1))
        elif isinstance(value, list) and value and isinstance(value[0], BaseModel):
            out.append(f"{pad}{name}:")
            for i, item in enumerate(value):
                out.append(f"{pad}  - [{i}]:")
                out.extend(_format_model(item, indent + 2))
        elif isinstance(value, (list, dict)):
            out.append(f"{pad}{name}: {value!r}")
        elif isinstance(value, datetime):
            out.append(f"{pad}{name}: {value.isoformat()}")
        else:
            out.append(f"{pad}{name}: {value!r}")
    return out


# ---------------------------------------------------------------------------
# Pipeline glue
# ---------------------------------------------------------------------------


def _find_card(view: KanbanView, key: str) -> tuple[KanbanCard, KanbanColumn] | None:
    """Locate the card by key in the aggregate result and return its column."""
    for column in ("todo", "in_progress", "needs_review", "done"):
        for card in getattr(view, column):
            if card.key == key:
                return card, column  # type: ignore[return-value]
    return None


async def debug_card(
    pr_arg: str | None,
    ticket_arg: str | None,
    repo_default: str | None,
    username_override: str | None,
) -> None:
    """Fetch live data, build the card, and print the trace."""
    cfg = load_config()
    cc = cfg.command_center

    if not pr_arg and not ticket_arg:
        raise click.ClickException("Provide at least --pr or --ticket")
    if pr_arg and not cc.github_token:
        raise click.ClickException(
            "command_center.github_token not set in ~/.config/zing-ai/config.toml"
        )
    if ticket_arg and not cc.linear_api_key:
        raise click.ClickException(
            "command_center.linear_api_key not set in ~/.config/zing-ai/config.toml"
        )

    github: GitHubClient | None = GitHubClient(token=cc.github_token) if cc.github_token else None
    linear: LinearClient | None = (
        LinearClient(api_key=cc.linear_api_key) if cc.linear_api_key else None
    )

    try:
        if username_override:
            username = username_override
        elif github is not None:
            username = await github.fetch_current_user()
        else:
            username = ""

        prs: list[GitHubPR] = []
        if pr_arg and github is not None:
            repo, number = _parse_pr_arg(pr_arg, repo_default)
            prs.append(await _fetch_pr(github, repo, number))

        ticket: LinearIssue | None = None
        if ticket_arg and linear is not None:
            ticket = await _fetch_ticket(linear, ticket_arg)

        # Run through the same aggregation pipeline the Command Center uses.
        view = aggregate(
            issues=[ticket] if ticket is not None else [],
            prs=prs,
            current_username=username,
        )

        # Locate the resulting card by its expected key.  ``aggregate`` keys
        # ticket cards by identifier and orphan PR cards as ``pr-{repo}-{n}``.
        if ticket is not None:
            expected_key = ticket.identifier
        else:
            pr = prs[0]
            expected_key = f"pr-{pr.repo}-{pr.number}" if pr.repo else f"pr-{pr.number}"

        located = _find_card(view, expected_key)
        if located is None:
            # Card was filtered out by aggregate (e.g. orphan PR with no user
            # involvement).  Build a bare card so the trace still has data.
            fallback = KanbanCard(key=expected_key, ticket=ticket, prs=prs)
            located = (fallback, "todo")
            excluded_by_aggregate = True
        else:
            excluded_by_aggregate = False

        card, column = located
        card_view = build_card_view(card, column, username)

        now = datetime.now(UTC)
        cutoff = now - _DONE_WINDOW

        out: list[str] = [
            "=== INPUT ===",
            f"current_username: {username!r}",
            f"now: {now.isoformat()}",
            f"done_window_cutoff: {cutoff.isoformat()}  # now - 7 days",
            f"card.key: {card.key!r}",
            f"excluded_by_aggregate: {excluded_by_aggregate}",
            "",
        ]

        out.append("=== CARD VIEW (canonical render model) ===")
        out.extend(_format_model(card_view, indent=1))
        out.append("")

        for pr in prs:
            out.append(f"=== PR_NEEDS_RESPONSE TRACE (#{pr.number}) ===")
            pr_trace: list[str] = []
            result = _pr_needs_response(card, pr, username, trace=pr_trace)
            out.extend(pr_trace)
            out.append(f"  RESULT: _pr_needs_response = {result}")
            out.append("")

        signals = _compute_signals(card, username, cutoff)
        out.append("=== CARD SIGNALS ===")
        for k, v in signals.__dict__.items():
            out.append(f"  {k}: {v}")
        out.append("")

        out.append("=== DECISION TABLE TRACE (first match wins) ===")
        classify_trace: list[str] = []
        _classify_card(card, username, now, trace=classify_trace)
        out.extend(classify_trace)
        out.append("")

        click.echo("\n".join(out))

    finally:
        if github is not None:
            await github.aclose()
        if linear is not None:
            await linear.aclose()


def run(
    pr_arg: str | None,
    ticket_arg: str | None,
    repo_default: str | None,
    username_override: str | None,
) -> None:
    """Sync entry point used by the CLI."""
    asyncio.run(debug_card(pr_arg, ticket_arg, repo_default, username_override))
