"""Core launch logic for `zing-ai launch` — worktree creation, init scripts, Linear updates.

All functions are pure (no Click dependency) so they can be unit-tested with mocked subprocess
calls.  The CLI command in cli.py imports and orchestrates these functions.

Workflow mode notes
-------------------
- ``"worktree"``: call :func:`create_worktree`.
- ``"branch"``: run ``git checkout -b <prefix><branch>`` in the current repo, return ``cwd``.
- ``"none"``: return ``cwd`` unchanged.
- ``"ask"``: the **caller** (CLI command) must prompt the user and then dispatch to one of the
  modes above.  This module does not import Click and will not prompt.
"""

from __future__ import annotations

import contextlib
import json
import os
import re
import shlex
import shutil
import subprocess
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Literal


class LaunchError(Exception):
    """Raised when any launch step fails.  The CLI command catches this and exits non-zero."""


# ---------------------------------------------------------------------------
# Ticket ID pattern — canonical source. Other modules import this.
# ---------------------------------------------------------------------------

TICKET_ID_PATTERN = r"[A-Z]{2,}-\d+"

_TICKET_RE = re.compile(rf"\b{TICKET_ID_PATTERN}\b")

# ---------------------------------------------------------------------------
# PR URL helpers
# ---------------------------------------------------------------------------


def parse_pr_url(url: str) -> tuple[str, str, int]:
    """Parse a GitHub pull-request URL and return (owner, repo, number).

    Accepts:
    - ``https://github.com/{owner}/{repo}/pull/{number}``
    - ``https://github.com/{owner}/{repo}/pull/{number}/...`` (with trailing segments)

    Args:
        url: GitHub PR URL string.

    Returns:
        A ``(owner, repo, number)`` tuple.

    Raises:
        LaunchError: If the URL is not a recognised GitHub PR URL.
    """
    parsed = urllib.parse.urlparse(url)
    match = re.match(r"^/([^/]+)/([^/]+)/pull/(\d+)", parsed.path)
    if not match:
        raise LaunchError(f"Not a valid GitHub PR URL: {url!r}")
    owner, repo, number_str = match.group(1), match.group(2), match.group(3)
    return owner, repo, int(number_str)


def fetch_pr_data(owner: str, repo: str, number: int) -> dict:
    """Fetch PR metadata from GitHub using the ``gh`` CLI.

    Runs ``gh pr view <number> --repo <owner>/<repo> --json headRefName,title,body``
    and returns the parsed JSON dict.

    Args:
        owner: Repository owner (user or org).
        repo: Repository name.
        number: Pull-request number.

    Returns:
        Parsed JSON dict with keys ``headRefName``, ``title``, and ``body``.

    Raises:
        LaunchError: If ``gh`` is not on PATH or the command fails.
    """
    if shutil.which("gh") is None:
        raise LaunchError(
            "GitHub CLI (gh) is required for PR-based launches."
            " Install it from https://cli.github.com/"
        )
    try:
        result = subprocess.run(
            [
                "gh",
                "pr",
                "view",
                str(number),
                "--repo",
                f"{owner}/{repo}",
                "--json",
                "headRefName,title,body",
            ],
            check=True,
            capture_output=True,
            text=True,
        )
    except subprocess.CalledProcessError as exc:
        raise LaunchError(
            f"gh pr view failed for {owner}/{repo}#{number}: {exc.stderr.strip()}"
        ) from exc
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise LaunchError(f"gh pr view returned non-JSON output: {exc}") from exc


def extract_ticket_id(branch: str, title: str, body: str) -> str | None:
    """Search *branch*, *title*, and *body* for a Linear ticket ID.

    Uses the ``_TICKET_RE`` pattern (``r"\\b[A-Z]{2,}-\\d+\\b"``) and returns
    the first match found, checked in the order: branch → title → body.

    Args:
        branch: PR head branch name.
        title: PR title.
        body: PR body / description.

    Returns:
        Uppercased ticket ID string (e.g. ``"BAK-123"``), or ``None`` if not found.
    """
    for text in (branch, title, body):
        if text:
            match = _TICKET_RE.search(text)
            if match:
                return match.group(0).upper()
    return None


# ---------------------------------------------------------------------------
# Git helpers
# ---------------------------------------------------------------------------


def resolve_repo_root(cwd: Path) -> Path:
    """Return the main git repository root, resolving through worktrees.

    Runs ``git rev-parse --show-toplevel`` to get the current root, then
    ``git worktree list --porcelain`` to check whether that root is a linked
    worktree.  If so, returns the main worktree's root instead.

    Args:
        cwd: Directory to run git commands from.

    Returns:
        Absolute path to the main worktree root.

    Raises:
        LaunchError: If any git command fails.
    """
    try:
        result = subprocess.run(
            ["git", "rev-parse", "--show-toplevel"],
            check=True,
            capture_output=True,
            text=True,
            cwd=cwd,
        )
        current_root = Path(result.stdout.strip())
    except subprocess.CalledProcessError as exc:
        raise LaunchError(f"git rev-parse failed: {exc.stderr.strip()}") from exc

    # Parse worktree list to find the main worktree root.
    try:
        wt_result = subprocess.run(
            ["git", "worktree", "list", "--porcelain"],
            check=True,
            capture_output=True,
            text=True,
            cwd=cwd,
        )
    except subprocess.CalledProcessError as exc:
        raise LaunchError(f"git worktree list failed: {exc.stderr.strip()}") from exc

    # The first worktree entry (no "worktree" key mismatch with "bare" or branch) is the main one.
    # Porcelain format: blocks separated by blank lines, first field is "worktree <path>".
    main_root: Path | None = None
    for block in wt_result.stdout.split("\n\n"):
        lines = [ln for ln in block.strip().splitlines() if ln]
        if not lines:
            continue
        path_line = lines[0]
        if not path_line.startswith("worktree "):
            continue
        wt_path = Path(path_line[len("worktree ") :].strip())
        # The main worktree never has an "isbare" marker but also won't have a "branch" line
        # containing "refs/heads/..." pointing elsewhere — simplest heuristic: first entry is main.
        if main_root is None:
            main_root = wt_path
        # If current_root matches a linked worktree (not the first one), use main_root.
        if wt_path == current_root and main_root != current_root:
            return main_root

    # Either not in a worktree, or is already the main worktree.
    return current_root


def create_worktree(
    repo_root: Path,
    branch_name: str,
    worktree_root_template: str,
    branch_prefix: str,
) -> Path:
    """Create a new git worktree on a new branch.

    The worktree path is derived by formatting *worktree_root_template* with:

    - ``{repo}`` — basename of *repo_root*
    - ``{branch}`` — *branch_name*

    Runs ``git worktree add -b <prefix><branch> <path>`` from *repo_root*.

    Args:
        repo_root: Absolute path to the main repository root.
        branch_name: Branch slug (without prefix).
        worktree_root_template: Template string, e.g. ``"../{repo}-{branch}"``.
        branch_prefix: Prefix prepended to the branch name, e.g. ``"zing/"``.

    Returns:
        Absolute path to the newly-created worktree directory.

    Raises:
        LaunchError: If ``git worktree add`` fails.
    """
    repo_name = repo_root.name
    relative = worktree_root_template.format(repo=repo_name, branch=branch_name)
    worktree_path = (repo_root / relative).resolve()
    full_branch = f"{branch_prefix}{branch_name}"

    try:
        subprocess.run(
            ["git", "worktree", "add", "-b", full_branch, str(worktree_path)],
            check=True,
            capture_output=True,
            text=True,
            cwd=repo_root,
        )
    except subprocess.CalledProcessError as exc:
        raise LaunchError(f"git worktree add failed: {exc.stderr.strip()}") from exc

    return worktree_path


def checkout_pr_branch(
    repo_root: Path,
    branch_name: str,
    worktree_root_template: str,
) -> Path:
    """Create a new git worktree on an existing (remote) branch.

    Runs ``git worktree add <path> <branch>`` — no ``-b`` flag, so the branch
    must already exist (e.g. a PR branch fetched from origin).

    Args:
        repo_root: Absolute path to the main repository root.
        branch_name: Existing branch name.
        worktree_root_template: Template string, e.g. ``"../{repo}-{branch}"``.

    Returns:
        Absolute path to the newly-created worktree directory.

    Raises:
        LaunchError: If ``git worktree add`` fails.
    """
    repo_name = repo_root.name
    relative = worktree_root_template.format(repo=repo_name, branch=branch_name)
    worktree_path = (repo_root / relative).resolve()

    # Best-effort fetch so the branch exists locally for worktree add
    with contextlib.suppress(subprocess.CalledProcessError):
        subprocess.run(
            ["git", "fetch", "origin", branch_name],
            capture_output=True,
            text=True,
            cwd=repo_root,
        )

    # Reuse existing worktree if it already exists and is a valid git checkout
    if worktree_path.is_dir():
        try:
            subprocess.run(
                ["git", "rev-parse", "--is-inside-work-tree"],
                check=True,
                capture_output=True,
                text=True,
                cwd=worktree_path,
            )
            return worktree_path
        except subprocess.CalledProcessError:
            pass  # directory exists but isn't a valid worktree — fall through to create

    try:
        subprocess.run(
            ["git", "worktree", "add", str(worktree_path), branch_name],
            check=True,
            capture_output=True,
            text=True,
            cwd=repo_root,
        )
    except subprocess.CalledProcessError as exc:
        raise LaunchError(f"git worktree add failed: {exc.stderr.strip()}") from exc

    return worktree_path


def rollback_worktree(worktree_path: Path) -> None:
    """Remove a worktree forcefully.  Called on downstream failure after creation.

    Runs ``git worktree remove --force <path>``.

    Args:
        worktree_path: Absolute path to the worktree to remove.

    Raises:
        LaunchError: If ``git worktree remove`` fails.
    """
    try:
        subprocess.run(
            ["git", "worktree", "remove", "--force", str(worktree_path)],
            check=True,
            capture_output=True,
            text=True,
        )
    except subprocess.CalledProcessError as exc:
        raise LaunchError(f"git worktree remove failed: {exc.stderr.strip()}") from exc


# ---------------------------------------------------------------------------
# Init script
# ---------------------------------------------------------------------------


def run_init_script(
    repo_root: Path,
    script_name: str,
    worktree_path: Path,
    branch: str,
) -> None:
    """Run the repository's init script from the repo root, if present.

    Looks for ``<repo_root>/<script_name>``.  If the file exists, runs it as a
    subprocess from *repo_root* with the following environment variables:

    - ``ZING_BRANCH`` — *branch*
    - ``ZING_WORKTREE_PATH`` — absolute string of *worktree_path*
    - ``ZING_SPEC_FILE`` — empty string
    - ``ZING_SESSION_ID`` — empty string

    Args:
        repo_root: Absolute path to the main repository root (where the script lives).
        script_name: Filename of the init script, e.g. ``".zing-init.sh"``.
        worktree_path: Directory to run the script from.
        branch: Branch name passed as ``ZING_BRANCH``.

    Raises:
        LaunchError: If the script exits with a non-zero status.
    """
    script_path = repo_root / script_name
    if not script_path.exists():
        return

    env = {**os.environ}
    env["ZING_BRANCH"] = branch
    env["ZING_WORKTREE_PATH"] = str(worktree_path)
    env["ZING_SPEC_FILE"] = ""
    env["ZING_SESSION_ID"] = ""

    try:
        subprocess.run(
            [str(script_path)],
            check=True,
            capture_output=True,
            text=True,
            cwd=repo_root,
            env=env,
        )
    except subprocess.CalledProcessError as exc:
        raise LaunchError(
            f"Init script {script_name} exited with code {exc.returncode}: {exc.stderr.strip()}"
        ) from exc


# ---------------------------------------------------------------------------
# Linear helpers
# ---------------------------------------------------------------------------


def _linear_request(api_key: str, query: str, variables: dict | None = None) -> dict:
    """Execute a Linear GraphQL request and return the parsed JSON response.

    Args:
        api_key: Linear API key.
        query: GraphQL query or mutation string.
        variables: Optional variables dict.

    Returns:
        Parsed JSON response dict (the full response, not just ``data``).

    Raises:
        LaunchError: On HTTP errors or non-JSON responses.
    """
    payload: dict = {"query": query}
    if variables:
        payload["variables"] = variables
    body = json.dumps(payload).encode()
    req = urllib.request.Request(
        "https://api.linear.app/graphql",
        data=body,
        headers={
            "Content-Type": "application/json",
            "Authorization": api_key,
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(req) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        raise LaunchError(f"Linear API HTTP {exc.code}: {exc.reason}") from exc
    except urllib.error.URLError as exc:
        raise LaunchError(f"Linear API request failed: {exc.reason}") from exc
    except json.JSONDecodeError as exc:
        raise LaunchError(f"Linear API returned non-JSON response: {exc}") from exc


def derive_branch_name(ticket_id: str, api_key: str) -> str:
    """Fetch the suggested branch name for a Linear issue.

    Calls ``{ issue(id: $id) { branchName } }`` via the Linear GraphQL API
    and returns the ``branchName`` string.

    Args:
        ticket_id: Linear issue identifier, e.g. ``"BAK-123"``.
        api_key: Linear API key.

    Returns:
        The ``branchName`` string from Linear.

    Raises:
        LaunchError: If the API call fails or the issue is not found.
    """
    query = "query($id: String!) { issue(id: $id) { branchName } }"
    resp = _linear_request(api_key, query, variables={"id": ticket_id})
    try:
        branch_name = resp["data"]["issue"]["branchName"]
    except (KeyError, TypeError) as exc:
        raise LaunchError(f"Could not retrieve branchName for {ticket_id}: {resp}") from exc
    if not branch_name:
        raise LaunchError(f"Linear returned empty branchName for {ticket_id}")
    return branch_name


def move_ticket_in_progress(ticket_id: str, api_key: str) -> None:
    """Move a Linear ticket to the "In Progress" workflow state.

    Makes three GraphQL calls:

    1. Fetch the team ID for the issue.
    2. Fetch the "In Progress" workflow state ID for that team.
    3. Update the issue's ``stateId``.

    Args:
        ticket_id: Linear issue identifier, e.g. ``"BAK-123"``.
        api_key: Linear API key.

    Raises:
        LaunchError: If any API call fails or required data is missing.
    """
    # Step 1: fetch team id
    team_query = "query($id: String!) { issue(id: $id) { id team { id } } }"
    team_resp = _linear_request(api_key, team_query, variables={"id": ticket_id})
    try:
        issue_data = team_resp["data"]["issue"]
        issue_id = issue_data["id"]
        team_id = issue_data["team"]["id"]
    except (KeyError, TypeError) as exc:
        raise LaunchError(f"Could not retrieve team for {ticket_id}: {team_resp}") from exc

    # Step 2: fetch "In Progress" state id
    state_query = """
    query($teamId: ID!, $name: String!) {
      workflowStates(filter: {
        team: { id: { eq: $teamId } },
        name: { eq: $name }
      }) {
        nodes { id }
      }
    }
    """
    state_resp = _linear_request(
        api_key,
        state_query,
        variables={"teamId": team_id, "name": "In Progress"},
    )
    try:
        nodes = state_resp["data"]["workflowStates"]["nodes"]
        state_id = nodes[0]["id"]
    except (KeyError, TypeError, IndexError) as exc:
        raise LaunchError(
            f"Could not find 'In Progress' state for team {team_id}: {state_resp}"
        ) from exc

    # Step 3: update issue
    mutation = """
    mutation($issueId: String!, $stateId: String!) {
      issueUpdate(id: $issueId, input: { stateId: $stateId }) {
        success
      }
    }
    """
    update_resp = _linear_request(
        api_key,
        mutation,
        variables={"issueId": issue_id, "stateId": state_id},
    )
    try:
        success = update_resp["data"]["issueUpdate"]["success"]
    except (KeyError, TypeError) as exc:
        raise LaunchError(f"issueUpdate returned unexpected response: {update_resp}") from exc
    if not success:
        raise LaunchError(f"issueUpdate returned success=false for {ticket_id}")


# ---------------------------------------------------------------------------
# MCP session helpers
# ---------------------------------------------------------------------------


def create_session_on_server(
    server_url: str,
    session_id: str,
    title: str,
    ticket_id: str | None,
    worktree_path: str | None,
    skill: str | None,
    pr_number: int | None = None,
    pr_repo: str | None = None,
    tmux_session: str | None = None,
) -> None:
    """Create a ``ClaudeCodeSession`` on the Zing server via REST.

    Makes a plain ``POST /api/sessions/claude-code`` with a JSON body — no
    JSON-RPC envelope.

    Args:
        server_url: Base URL of the Zing server, e.g. ``"http://127.0.0.1:9876"``.
        session_id: Unique session identifier.
        title: Human-readable session title.
        ticket_id: Linear ticket ID, or ``None``.
        worktree_path: Absolute worktree path, or ``None``.
        skill: Skill/command name, or ``None``.
        pr_number: GitHub PR number, or ``None``.
        pr_repo: GitHub repo as ``"owner/repo"``, or ``None``.
        tmux_session: tmux session name for detached launches, or ``None``.

    Raises:
        LaunchError: If the HTTP call fails.
    """
    payload: dict = {
        "session_id": session_id,
        "title": title,
        "ticket_id": ticket_id,
        "worktree_path": worktree_path,
        "skill": skill,
        "pr_number": pr_number,
        "pr_repo": pr_repo,
        "tmux_session": tmux_session,
    }
    body = json.dumps(payload).encode()
    req = urllib.request.Request(
        f"{server_url.rstrip('/')}/api/sessions/claude-code",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req) as resp:
            resp.read()
    except urllib.error.HTTPError as exc:
        raise LaunchError(f"Zing server HTTP {exc.code}: {exc.reason}") from exc
    except (urllib.error.URLError, ConnectionRefusedError, OSError) as exc:
        raise LaunchError(
            f"Could not connect to Zing server at {server_url}. Is 'zing-ai mcp' running?"
        ) from exc


def detect_action(
    ticket_id: str,
    server_url: str,
) -> tuple[Literal["resume", "new"], str | None]:
    """Check whether an existing Claude Code session exists for *ticket_id*.

    Calls ``GET /api/sessions?ticket_id=<id>`` and finds any session with
    ``session_type == "claude_code"``.

    Args:
        ticket_id: Linear ticket ID to look for.
        server_url: Base URL of the Zing server.

    Returns:
        ``("resume", session_id)`` if a matching session is found, otherwise
        ``("new", None)``.

    Raises:
        LaunchError: If the HTTP call fails.
    """
    url = f"{server_url.rstrip('/')}/api/sessions?ticket_id={ticket_id}"
    req = urllib.request.Request(url, method="GET")
    try:
        with urllib.request.urlopen(req) as resp:
            sessions: list[dict] = json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        raise LaunchError(f"Zing server HTTP {exc.code}: {exc.reason}") from exc
    except (urllib.error.URLError, ConnectionRefusedError, OSError) as exc:
        raise LaunchError(
            f"Could not connect to Zing server at {server_url}. Is 'zing-ai mcp' running?"
        ) from exc
    except json.JSONDecodeError as exc:
        raise LaunchError(f"Zing server returned non-JSON response: {exc}") from exc

    for session in sessions:
        if session.get("session_type") == "claude_code":
            return ("resume", session["session_id"])

    return ("new", None)


# ---------------------------------------------------------------------------
# Claude argument builder
# ---------------------------------------------------------------------------


def build_claude_args(
    skill: str,
    session_id: str,
    name: str,
    target: str | None = None,
) -> list[str]:
    """Build the argv list to pass to ``os.execvp("claude", ...)`` .

    Modes:

    - ``skill == "resume"``: ``["claude", "--resume", session_id]``
    - anything else: ``["claude", "/zing:<skill> <target>", "--session-id",
      session_id, "--name", name]``

    Args:
        skill: Skill name (e.g. ``"resume"``, ``"new"``, ``"pr-audit"``,
            ``"pr-audit-visual"``).
        session_id: Claude session identifier.
        name: Session display name.
        target: Argument passed to the slash command (ticket ID for new-ticket
            flows, PR URL for PR flows).  Omitted from the command when ``None``.

    Returns:
        List of strings suitable for ``os.execvp``.
    """
    if skill == "resume":
        return ["claude", "--resume", session_id]

    slash_cmd = f"/zing:{skill} {target}" if target else f"/zing:{skill}"
    return ["claude", slash_cmd, "--session-id", session_id, "--name", name]


# ---------------------------------------------------------------------------
# tmux helpers
# ---------------------------------------------------------------------------

_TMUX_UNSAFE_RE = re.compile(r"[^a-zA-Z0-9_\-]")


def _sanitize_tmux_name(name: str) -> str:
    """Replace characters unsafe for tmux session names with underscores."""
    return _TMUX_UNSAFE_RE.sub("_", name)


def build_tmux_session_name(target: str, pr_number: int | None = None) -> str:
    """Build a tmux session name for a launch target.

    Args:
        target: Ticket ID, branch name, or other identifier for the session.
        pr_number: If provided, overrides ``target`` and produces a PR-based name.

    Returns:
        A tmux-safe session name string.
    """
    if pr_number is not None:
        return f"zing-pr-{pr_number}"
    if re.fullmatch(TICKET_ID_PATTERN, target):
        return f"zing-{target.lower()}"
    return f"zing-{_sanitize_tmux_name(target)}"


def require_tmux() -> None:
    """Check that tmux is available on PATH.

    Raises:
        LaunchError: If tmux is not found.
    """
    if shutil.which("tmux") is None:
        raise LaunchError("tmux is required for --detach mode but was not found on PATH")


def exec_or_detach(args: list[str], work_dir: Path, tmux_session: str | None = None) -> None:
    """Execute a command in the foreground or in a detached tmux session.

    When ``tmux_session`` is ``None``, the current process is replaced via
    ``os.execvp`` (foreground mode).  When set, a new detached tmux session
    is created.

    Args:
        args: Command and arguments, e.g. ``["claude", "/zing:new BAK-1", ...]``.
        work_dir: Working directory for the process.
        tmux_session: tmux session name.  ``None`` means foreground (exec).

    Raises:
        LaunchError: If the tmux session name already exists.
    """
    if tmux_session is None:
        os.execvp(args[0], args)
        return  # unreachable — satisfies type checkers

    # Check for name collision
    result = subprocess.run(
        ["tmux", "has-session", "-t", tmux_session],
        capture_output=True,
    )
    if result.returncode == 0:
        raise LaunchError(f"tmux session '{tmux_session}' already exists")

    subprocess.run(
        [
            "tmux",
            "new-session",
            "-d",
            "-s",
            tmux_session,
            "-c",
            str(work_dir),
            shlex.join(args),
        ],
        check=True,
    )


# ---------------------------------------------------------------------------
# Repo discovery
# ---------------------------------------------------------------------------


def find_repo_path(
    code_dir: str,
    repo_full_name: str,
    cache: dict[str, Path] | None = None,
) -> Path | None:
    """Scan *code_dir* for a local checkout of *repo_full_name* (``"owner/repo"``).

    Searches immediate children (depth 1) of *code_dir*.  For each subdirectory
    the function:

    1. Confirms it is a git repository via ``git rev-parse --is-inside-work-tree``.
    2. Skips linked worktrees (only the main checkout is matched).
    3. Resolves the origin remote URL and normalises it to ``"owner/repo"``.
    4. Stores the mapping in *cache* for future calls.

    Args:
        code_dir: Root directory that contains local repository checkouts.
        repo_full_name: GitHub repository in ``"owner/repo"`` format.
        cache: Optional dict used to memoise results.  Modified in-place.

    Returns:
        :class:`~pathlib.Path` to the matching directory, or ``None`` if not found.

    Raises:
        LaunchError: If *code_dir* is empty or does not exist.
    """
    if not code_dir:
        raise LaunchError(
            "code_dir is not configured. Set [git] code_dir in ~/.config/zing-ai/config.toml"
        )

    code_path = Path(code_dir)
    if not code_path.exists():
        raise LaunchError(f"code_dir '{code_dir}' does not exist")

    if cache is not None and repo_full_name in cache:
        return cache[repo_full_name]

    # Use a local dict to accumulate results; merge into caller's cache at the end.
    local: dict[str, Path] = {}

    # Scan immediate children only (depth 1).
    for child in sorted(code_path.iterdir()):
        if not child.is_dir():
            continue

        # 1. Check it is a git repo.
        is_git = subprocess.run(
            ["git", "-C", str(child), "rev-parse", "--is-inside-work-tree"],
            capture_output=True,
        )
        if is_git.returncode != 0:
            continue

        # 2. Skip linked worktrees — only match the main checkout.
        wt_result = subprocess.run(
            ["git", "-C", str(child), "worktree", "list", "--porcelain"],
            capture_output=True,
            text=True,
        )
        if wt_result.returncode != 0:
            continue

        # The first "worktree <path>" line names the main checkout.
        main_wt_path: Path | None = None
        for line in wt_result.stdout.splitlines():
            if line.startswith("worktree "):
                main_wt_path = Path(line[len("worktree ") :].strip())
                break

        if main_wt_path is not None and child.resolve() != main_wt_path.resolve():
            # This directory is a linked worktree — skip it.
            continue

        # 3. Get origin remote URL.
        remote_result = subprocess.run(
            ["git", "-C", str(child), "remote", "get-url", "origin"],
            capture_output=True,
            text=True,
        )
        if remote_result.returncode != 0:
            continue

        remote_url = remote_result.stdout.strip()

        # Parse owner/repo from HTTPS or SSH remote URL.
        # HTTPS: https://github.com/owner/repo.git
        # SSH:   git@github.com:owner/repo.git
        https_match = re.search(r"github\.com/([^/]+/[^/]+?)(?:\.git)?$", remote_url)
        ssh_match = re.search(r"github\.com:([^/]+/[^/]+?)(?:\.git)?$", remote_url)
        m = https_match or ssh_match
        if not m:
            continue

        full_name = m.group(1)
        local[full_name] = child

    # Merge results into caller's cache.
    if cache is not None:
        cache.update(local)
        return cache.get(repo_full_name)

    return local.get(repo_full_name)
