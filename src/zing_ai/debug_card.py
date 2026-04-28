"""Debug utility for Kanban card classification.

Fetches a live PR (via GitHub) and/or ticket (via Linear), runs the data
through the same :func:`aggregate` pipeline the Command Center uses,
constructs the canonical :class:`CardView` for the resulting card, and
prints a structured trace with two diagnostic blocks attached:

* a line-by-line walk through ``_pr_needs_response`` for each PR, and
* the first-match decision-table evaluation in ``_classify_card``.

The display state itself is not duplicated here — every field a renderer
would draw comes from ``CardView`` via Pydantic introspection, so adding
a field there automatically surfaces it in the output.
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
    _compute_signals,
    _is_human_reviewer,
    _should_include_card,
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
# Diagnostic traces (kept as duplication-with-tests for explanatory value)
# ---------------------------------------------------------------------------


def _trace_pr_needs_response(
    card: KanbanCard, pr: GitHubPR, current_username: str
) -> tuple[list[str], bool]:
    """Walk the live ``_pr_needs_response`` logic step-by-step and return the result."""
    lines: list[str] = [
        f"--- TRACE: _pr_needs_response(card, pr=#{pr.number}, "
        f"current_username={current_username!r}) ---",
    ]

    state_ok = pr.state == "open"
    author_ok = pr.author == current_username
    not_approved = pr.review_decision != "APPROVED"
    lines.append(f"  guard pr.state == 'open': {state_ok}")
    lines.append(f"  guard pr.author == current_username: {author_ok}")
    lines.append(f"  guard pr.review_decision != 'APPROVED': {not_approved}")
    if not (state_ok and author_ok and not_approved):
        lines.append("  → guard fails → return False")
        return lines, False

    if pr.review_decision == "CHANGES_REQUESTED":
        lines.append("  branch: pr.review_decision == 'CHANGES_REQUESTED' → True")
        explicit_cr = {
            login for login, state in pr.reviewer_states.items() if state == "CHANGES_REQUESTED"
        }
        lines.append(f"    explicit_cr: {sorted(explicit_cr)}")
        pending = explicit_cr - set(pr.requested_reviewers)
        lines.append(f"    pending = explicit_cr - requested_reviewers: {sorted(pending)}")
        if not explicit_cr:
            lines.append(
                "    sub-branch: 'no explicit CR' (states overwritten OR all "
                "reviewers re-requested) → return True"
            )
            return lines, True
        if pending:
            lines.append("    sub-branch: 'pending CR' → return True")
            return lines, True
        lines.append("    sub-branch: all explicit CR-ers re-requested → fall through")

    not_rerequested = set(pr.reviewers) - set(pr.requested_reviewers)
    lines.append(
        f"  fallback: not_rerequested = reviewers - requested_reviewers: {sorted(not_rerequested)}"
    )
    human_results = {r: _is_human_reviewer(card, r) for r in sorted(not_rerequested)}
    lines.append(f"  fallback: _is_human_reviewer per reviewer: {human_results}")
    result = any(human_results.values())
    lines.append(f"  → return {result}")
    return lines, result


def _trace_classification(card: KanbanCard, current_username: str, now: datetime) -> list[str]:
    """Print the decision-table evaluation in ``_classify_card`` order."""
    cutoff = now - _DONE_WINDOW
    s = _compute_signals(card, current_username, cutoff)
    included = _should_include_card(card, current_username, cutoff)

    rules: list[tuple[str, bool, str]] = [
        ("(filter) _should_include_card == False", not included, "EXCLUDED"),
        ("has_unaddressed_feedback", s.has_unaddressed_feedback, "in_progress"),
        ("has_pr_awaiting_review", s.has_pr_awaiting_review, "needs_review"),
        ("has_approved_open_pr", s.has_approved_open_pr, "done"),
        ("is_recently_done", s.is_recently_done, "done"),
        (
            "owned AND (has_active_session OR has_open_pr_no_reviewers)",
            s.owned and (s.has_active_session or s.has_open_pr_no_reviewers),
            "in_progress",
        ),
        ("owned AND ticket_started", s.owned and s.ticket_started, "in_progress"),
        ("(default fallthrough)", True, "todo"),
    ]

    lines = ["=== DECISION TABLE TRACE (first match wins) ==="]
    fired = False
    for desc, val, target in rules:
        if val and not fired:
            marker = "[FIRE]"
            fired = True
        elif val:
            marker = "[ ok ]"
        else:
            marker = "[skip]"
        lines.append(f"  {marker} {desc}  ⇒ {target}")
    lines.append("")
    return lines


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
            trace_lines, result = _trace_pr_needs_response(card, pr, username)
            out.extend(trace_lines)
            out.append(f"  RESULT: _pr_needs_response = {result}")
            out.append("")

        signals = _compute_signals(card, username, cutoff)
        out.append("=== CARD SIGNALS ===")
        for k, v in signals.__dict__.items():
            out.append(f"  {k}: {v}")
        out.append("")

        out.extend(_trace_classification(card, username, now))

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
