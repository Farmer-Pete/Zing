"""Tests for zing_ai.launch — core launch logic."""

from __future__ import annotations

import json
import subprocess
from collections.abc import Callable
from pathlib import Path
from unittest import TestCase
from unittest.mock import MagicMock, patch

from zing_ai.launch import (
    LaunchError,
    build_claude_args,
    checkout_pr_branch,
    create_session_on_server,
    create_worktree,
    derive_branch_name,
    detect_action,
    extract_ticket_id,
    fetch_pr_data,
    move_ticket_in_progress,
    parse_pr_url,
    resolve_repo_root,
    rollback_worktree,
    run_init_script,
)

# ---------------------------------------------------------------------------
# resolve_repo_root
# ---------------------------------------------------------------------------


class TestResolveRepoRoot(TestCase):
    """Tests for resolve_repo_root."""

    def _make_run(self, toplevel: str, worktree_list: str) -> Callable[..., MagicMock]:
        """Return a mock for subprocess.run that yields the given outputs."""

        def fake_run(cmd, **kwargs):
            result = MagicMock()
            if "rev-parse" in cmd:
                result.stdout = toplevel + "\n"
            elif "worktree" in cmd and "list" in cmd:
                result.stdout = worktree_list
            return result

        return fake_run

    def test_returns_main_root_when_not_in_worktree(self, tmp_path: Path | None = None) -> None:
        """When the current root IS the main worktree, return it unchanged."""
        current = "/repo/main"
        wt_list = f"worktree {current}\nHEAD abc123\nbranch refs/heads/main\n\n"
        fake_run = self._make_run(current, wt_list)

        with patch("zing_ai.launch.subprocess.run", side_effect=fake_run):
            result = resolve_repo_root(Path("/repo/main"))

        self.assertEqual(result, Path(current))

    def test_returns_main_root_when_in_worktree(self) -> None:
        """When in a linked worktree, return the main worktree root."""
        main = "/repo/main"
        linked = "/repo/worktrees/feature"
        wt_list = (
            f"worktree {main}\nHEAD abc\nbranch refs/heads/main\n\n"
            f"worktree {linked}\nHEAD def\nbranch refs/heads/feature\n\n"
        )
        fake_run = self._make_run(linked, wt_list)

        with patch("zing_ai.launch.subprocess.run", side_effect=fake_run):
            result = resolve_repo_root(Path(linked))

        self.assertEqual(result, Path(main))

    def test_raises_launch_error_on_rev_parse_failure(self) -> None:
        """CalledProcessError from git rev-parse bubbles up as LaunchError."""
        with (
            patch(
                "zing_ai.launch.subprocess.run",
                side_effect=subprocess.CalledProcessError(128, "git", stderr="not a git repo"),
            ),
            self.assertRaises(LaunchError),
        ):
            resolve_repo_root(Path("/not/a/repo"))

    def test_raises_launch_error_on_worktree_list_failure(self) -> None:
        """CalledProcessError from git worktree list bubbles up as LaunchError."""
        call_count = 0

        def fake_run(cmd, **kwargs):
            nonlocal call_count
            call_count += 1
            if call_count == 1:
                result = MagicMock()
                result.stdout = "/repo/main\n"
                return result
            raise subprocess.CalledProcessError(128, "git", stderr="oops")

        with (
            patch("zing_ai.launch.subprocess.run", side_effect=fake_run),
            self.assertRaises(LaunchError),
        ):
            resolve_repo_root(Path("/repo/main"))


# ---------------------------------------------------------------------------
# derive_branch_name
# ---------------------------------------------------------------------------


class TestDeriveBranchName(TestCase):
    """Tests for derive_branch_name."""

    def _mock_urlopen(self, response_data: dict):
        """Context manager mock for urllib.request.urlopen."""
        resp_mock = MagicMock()
        resp_mock.read.return_value = json.dumps(response_data).encode()
        resp_mock.__enter__ = lambda s: s
        resp_mock.__exit__ = MagicMock(return_value=False)
        return patch("zing_ai.launch.urllib.request.urlopen", return_value=resp_mock)

    def test_returns_branch_name(self) -> None:
        data = {"data": {"issue": {"branchName": "zing/bak-123-do-the-thing"}}}
        with self._mock_urlopen(data):
            result = derive_branch_name("BAK-123", "api-key")
        self.assertEqual(result, "zing/bak-123-do-the-thing")

    def test_raises_on_missing_data(self) -> None:
        data = {"data": {"issue": None}}
        with self._mock_urlopen(data), self.assertRaises(LaunchError):
            derive_branch_name("BAK-999", "api-key")

    def test_raises_on_empty_branch_name(self) -> None:
        data = {"data": {"issue": {"branchName": ""}}}
        with self._mock_urlopen(data), self.assertRaises(LaunchError):
            derive_branch_name("BAK-123", "api-key")


# ---------------------------------------------------------------------------
# create_worktree
# ---------------------------------------------------------------------------


class TestCreateWorktree(TestCase):
    """Tests for create_worktree."""

    def test_creates_worktree_and_returns_path(self, tmp_path: Path | None = None) -> None:
        repo_root = Path("/repo/main")
        branch_name = "bak-123-my-feature"
        template = "../{repo}-{branch}"
        prefix = "zing/"

        with patch("zing_ai.launch.subprocess.run") as mock_run:
            mock_run.return_value = MagicMock()
            result = create_worktree(repo_root, branch_name, template, prefix)

        expected_path = (repo_root / "../main-bak-123-my-feature").resolve()
        self.assertEqual(result, expected_path)

        mock_run.assert_called_once()
        call_args = mock_run.call_args[0][0]
        self.assertEqual(call_args[0], "git")
        self.assertEqual(call_args[1], "worktree")
        self.assertEqual(call_args[2], "add")
        self.assertEqual(call_args[3], "-b")
        self.assertEqual(call_args[4], "zing/bak-123-my-feature")
        self.assertEqual(call_args[5], str(expected_path))

    def test_raises_on_git_failure(self) -> None:
        with (
            patch(
                "zing_ai.launch.subprocess.run",
                side_effect=subprocess.CalledProcessError(1, "git", stderr="already exists"),
            ),
            self.assertRaises(LaunchError),
        ):
            create_worktree(Path("/repo"), "feat", "../{repo}-{branch}", "zing/")

    def test_path_formatting_with_slash_in_branch(self) -> None:
        """Template substitution uses the full branch_name including slashes."""
        repo_root = Path("/repo/myproject")
        with patch("zing_ai.launch.subprocess.run") as mock_run:
            mock_run.return_value = MagicMock()
            result = create_worktree(repo_root, "bak-42-fix", "../{repo}-{branch}", "")
        expected = (repo_root / "../myproject-bak-42-fix").resolve()
        self.assertEqual(result, expected)


# ---------------------------------------------------------------------------
# checkout_pr_branch
# ---------------------------------------------------------------------------


class TestCheckoutPrBranch(TestCase):
    """Tests for checkout_pr_branch."""

    def test_creates_worktree_on_existing_branch(self) -> None:
        repo_root = Path("/repo/main")
        branch = "origin/pr-branch"
        template = "../{repo}-{branch}"

        with patch("zing_ai.launch.subprocess.run") as mock_run:
            mock_run.return_value = MagicMock()
            result = checkout_pr_branch(repo_root, branch, template)

        expected_path = (repo_root / f"../main-{branch}").resolve()
        self.assertEqual(result, expected_path)

        call_args = mock_run.call_args[0][0]
        # Should NOT have '-b' flag
        self.assertNotIn("-b", call_args)
        self.assertIn("add", call_args)
        self.assertIn(branch, call_args)

    def test_raises_on_git_failure(self) -> None:
        with (
            patch(
                "zing_ai.launch.subprocess.run",
                side_effect=subprocess.CalledProcessError(1, "git", stderr="branch not found"),
            ),
            self.assertRaises(LaunchError),
        ):
            checkout_pr_branch(Path("/repo"), "missing-branch", "../{repo}-{branch}")


# ---------------------------------------------------------------------------
# rollback_worktree
# ---------------------------------------------------------------------------


class TestRollbackWorktree(TestCase):
    """Tests for rollback_worktree."""

    def test_runs_correct_command(self) -> None:
        worktree_path = Path("/repo/worktrees/feature")
        with patch("zing_ai.launch.subprocess.run") as mock_run:
            mock_run.return_value = MagicMock()
            rollback_worktree(worktree_path)

        call_args = mock_run.call_args[0][0]
        self.assertEqual(call_args, ["git", "worktree", "remove", "--force", str(worktree_path)])

    def test_raises_on_git_failure(self) -> None:
        with (
            patch(
                "zing_ai.launch.subprocess.run",
                side_effect=subprocess.CalledProcessError(1, "git", stderr="not a worktree"),
            ),
            self.assertRaises(LaunchError),
        ):
            rollback_worktree(Path("/repo/worktrees/gone"))


# ---------------------------------------------------------------------------
# run_init_script
# ---------------------------------------------------------------------------


class TestRunInitScript(TestCase):
    """Tests for run_init_script."""

    def test_skips_when_script_does_not_exist(self, tmp_path: Path | None = None) -> None:
        """No subprocess call when the init script is absent."""
        import tempfile

        with tempfile.TemporaryDirectory() as repo_dir:
            repo_root = Path(repo_dir)
            with patch("zing_ai.launch.subprocess.run") as mock_run:
                run_init_script(repo_root, ".zing-init.sh", Path("/worktree"), "my-branch")
            mock_run.assert_not_called()

    def test_runs_script_with_correct_env_vars(self) -> None:
        """Script is invoked from repo root with the correct env vars."""
        import tempfile

        with tempfile.TemporaryDirectory() as repo_dir:
            repo_root = Path(repo_dir)
            script = repo_root / ".zing-init.sh"
            script.write_text("#!/bin/sh\necho hi\n")

            worktree_path = Path("/tmp/worktree")
            branch = "zing/bak-123"

            with patch("zing_ai.launch.subprocess.run") as mock_run:
                mock_run.return_value = MagicMock()
                run_init_script(repo_root, ".zing-init.sh", worktree_path, branch)

            mock_run.assert_called_once()
            kwargs = mock_run.call_args[1]
            env = kwargs["env"]
            self.assertEqual(env["ZING_BRANCH"], branch)
            self.assertEqual(env["ZING_WORKTREE_PATH"], str(worktree_path))
            self.assertEqual(env["ZING_SPEC_FILE"], "")
            self.assertEqual(env["ZING_SESSION_ID"], "")
            self.assertEqual(kwargs["cwd"], repo_root)

    def test_raises_on_script_failure(self) -> None:
        import tempfile

        with tempfile.TemporaryDirectory() as repo_dir:
            repo_root = Path(repo_dir)
            script = repo_root / ".zing-init.sh"
            script.write_text("#!/bin/sh\nexit 1\n")

            with (
                patch(
                    "zing_ai.launch.subprocess.run",
                    side_effect=subprocess.CalledProcessError(1, ".zing-init.sh", stderr="failed"),
                ),
                self.assertRaises(LaunchError),
            ):
                run_init_script(repo_root, ".zing-init.sh", Path("/wt"), "branch")


# ---------------------------------------------------------------------------
# move_ticket_in_progress
# ---------------------------------------------------------------------------


class TestMoveTicketInProgress(TestCase):
    """Tests for move_ticket_in_progress."""

    def _make_urlopen_sequence(self, responses: list[dict]):
        """Return a side_effect list for sequential urllib.request.urlopen calls."""
        mocks = []
        for data in responses:
            m = MagicMock()
            m.read.return_value = json.dumps(data).encode()
            m.__enter__ = lambda s: s
            m.__exit__ = MagicMock(return_value=False)
            mocks.append(m)
        return mocks

    def test_moves_ticket_in_progress(self) -> None:
        responses = [
            # Step 1: fetch team
            {"data": {"issue": {"id": "issue-uuid", "team": {"id": "team-uuid"}}}},
            # Step 2: fetch workflow state
            {"data": {"workflowStates": {"nodes": [{"id": "state-uuid"}]}}},
            # Step 3: update issue
            {"data": {"issueUpdate": {"success": True}}},
        ]
        mocks = self._make_urlopen_sequence(responses)
        with patch("zing_ai.launch.urllib.request.urlopen", side_effect=mocks):
            # Should not raise
            move_ticket_in_progress("BAK-123", "api-key")

    def test_raises_when_team_not_found(self) -> None:
        responses = [
            {"data": {"issue": None}},
        ]
        mocks = self._make_urlopen_sequence(responses)
        with (
            patch("zing_ai.launch.urllib.request.urlopen", side_effect=mocks),
            self.assertRaises(LaunchError),
        ):
            move_ticket_in_progress("BAK-999", "api-key")

    def test_raises_when_no_in_progress_state(self) -> None:
        responses = [
            {"data": {"issue": {"id": "issue-uuid", "team": {"id": "team-uuid"}}}},
            {"data": {"workflowStates": {"nodes": []}}},
        ]
        mocks = self._make_urlopen_sequence(responses)
        with (
            patch("zing_ai.launch.urllib.request.urlopen", side_effect=mocks),
            self.assertRaises(LaunchError),
        ):
            move_ticket_in_progress("BAK-123", "api-key")

    def test_raises_when_update_fails(self) -> None:
        responses = [
            {"data": {"issue": {"id": "issue-uuid", "team": {"id": "team-uuid"}}}},
            {"data": {"workflowStates": {"nodes": [{"id": "state-uuid"}]}}},
            {"data": {"issueUpdate": {"success": False}}},
        ]
        mocks = self._make_urlopen_sequence(responses)
        with (
            patch("zing_ai.launch.urllib.request.urlopen", side_effect=mocks),
            self.assertRaises(LaunchError),
        ):
            move_ticket_in_progress("BAK-123", "api-key")


# ---------------------------------------------------------------------------
# create_session_on_server
# ---------------------------------------------------------------------------


class TestCreateSessionOnServer(TestCase):
    """Tests for create_session_on_server."""

    def test_posts_correct_payload(self) -> None:
        resp_mock = MagicMock()
        resp_mock.read.return_value = json.dumps(
            {"status": "created", "session_id": "sess-abc"}
        ).encode()
        resp_mock.__enter__ = lambda s: s
        resp_mock.__exit__ = MagicMock(return_value=False)

        with patch("zing_ai.launch.urllib.request.urlopen", return_value=resp_mock) as mock_open:
            create_session_on_server(
                server_url="http://localhost:9876",
                session_id="sess-abc",
                title="My session",
                ticket_id="BAK-42",
                worktree_path="/tmp/wt",
                skill="new",
            )

        # Inspect the Request object passed to urlopen
        req = mock_open.call_args[0][0]
        self.assertIn("/api/sessions/claude-code", req.full_url)
        body = json.loads(req.data.decode())
        self.assertEqual(body["session_id"], "sess-abc")
        self.assertEqual(body["ticket_id"], "BAK-42")
        self.assertEqual(body["worktree_path"], "/tmp/wt")
        self.assertEqual(body["skill"], "new")

    def test_includes_none_fields_in_payload(self) -> None:
        resp_mock = MagicMock()
        resp_mock.read.return_value = json.dumps(
            {"status": "created", "session_id": "sess-xyz"}
        ).encode()
        resp_mock.__enter__ = lambda s: s
        resp_mock.__exit__ = MagicMock(return_value=False)

        with patch("zing_ai.launch.urllib.request.urlopen", return_value=resp_mock) as mock_open:
            create_session_on_server(
                server_url="http://localhost:9876",
                session_id="sess-xyz",
                title="Minimal",
                ticket_id=None,
                worktree_path=None,
                skill=None,
            )

        req = mock_open.call_args[0][0]
        body = json.loads(req.data.decode())
        self.assertIsNone(body["worktree_path"])
        self.assertIsNone(body["skill"])


# ---------------------------------------------------------------------------
# detect_action
# ---------------------------------------------------------------------------


class TestDetectAction(TestCase):
    """Tests for detect_action."""

    def _mock_urlopen(self, response_data):
        resp_mock = MagicMock()
        resp_mock.read.return_value = json.dumps(response_data).encode()
        resp_mock.__enter__ = lambda s: s
        resp_mock.__exit__ = MagicMock(return_value=False)
        return patch("zing_ai.launch.urllib.request.urlopen", return_value=resp_mock)

    def test_returns_resume_when_session_found(self) -> None:
        # GET /api/sessions?ticket_id=BAK-123 returns a plain list
        sessions = [
            {"session_type": "claude_code", "ticket_id": "BAK-123", "session_id": "sess-abc"},
            {"session_type": "zing", "ticket_id": "BAK-123", "session_id": "sess-xyz"},
        ]
        with self._mock_urlopen(sessions):
            action, sid = detect_action("BAK-123", "http://localhost:9876")
        self.assertEqual(action, "resume")
        self.assertEqual(sid, "sess-abc")

    def test_returns_new_when_no_matching_session(self) -> None:
        sessions: list = []
        with self._mock_urlopen(sessions):
            action, sid = detect_action("BAK-123", "http://localhost:9876")
        self.assertEqual(action, "new")
        self.assertIsNone(sid)

    def test_returns_new_when_session_list_empty(self) -> None:
        with self._mock_urlopen([]):
            action, sid = detect_action("BAK-123", "http://localhost:9876")
        self.assertEqual(action, "new")
        self.assertIsNone(sid)

    def test_returns_new_when_only_non_claude_code_sessions(self) -> None:
        sessions = [
            {"session_type": "zing", "ticket_id": "BAK-5", "session_id": "sess-zing"},
        ]
        with self._mock_urlopen(sessions):
            action, sid = detect_action("BAK-5", "http://localhost:9876")
        self.assertEqual(action, "new")
        self.assertIsNone(sid)


# ---------------------------------------------------------------------------
# build_claude_args
# ---------------------------------------------------------------------------


class TestBuildClaudeArgs(TestCase):
    """Tests for build_claude_args."""

    def test_new_ticket_session(self) -> None:
        args = build_claude_args("new", "sess-abc", "my session", target="BAK-123")
        self.assertEqual(
            args,
            ["claude", "/zing:new BAK-123", "--session-id", "sess-abc", "--name", "my session"],
        )

    def test_pr_audit_session(self) -> None:
        pr_url = "https://github.com/acme/repo/pull/42"
        args = build_claude_args("pr-audit", "sess-xyz", "pr session", target=pr_url)
        self.assertEqual(
            args,
            [
                "claude",
                f"/zing:pr-audit {pr_url}",
                "--session-id",
                "sess-xyz",
                "--name",
                "pr session",
            ],
        )

    def test_pr_audit_visual_session(self) -> None:
        """Any pr-* skill produces /zing:<skill> <target>."""
        pr_url = "https://github.com/acme/repo/pull/99"
        args = build_claude_args("pr-audit-visual", "sess-vis", "visual review", target=pr_url)
        self.assertEqual(
            args,
            [
                "claude",
                f"/zing:pr-audit-visual {pr_url}",
                "--session-id",
                "sess-vis",
                "--name",
                "visual review",
            ],
        )

    def test_resume_session(self) -> None:
        args = build_claude_args("resume", "sess-old", "old session")
        self.assertEqual(args, ["claude", "--resume", "sess-old"])

    def test_new_without_target(self) -> None:
        """When target is None, omit it from the slash command."""
        args = build_claude_args("new", "sess-no-ticket", "unticketed")
        self.assertEqual(
            args,
            [
                "claude",
                "/zing:new",
                "--session-id",
                "sess-no-ticket",
                "--name",
                "unticketed",
            ],
        )

    def test_custom_skill(self) -> None:
        """Any skill produces /zing:<skill> <target>."""
        args = build_claude_args("build", "sess-build", "build session", target="BAK-7")
        self.assertEqual(
            args,
            [
                "claude",
                "/zing:build BAK-7",
                "--session-id",
                "sess-build",
                "--name",
                "build session",
            ],
        )


# ---------------------------------------------------------------------------
# parse_pr_url
# ---------------------------------------------------------------------------


class TestParsePrUrl(TestCase):
    """Tests for parse_pr_url."""

    def test_parses_simple_url(self) -> None:
        """Standard PR URL returns correct (owner, repo, number)."""
        owner, repo, number = parse_pr_url("https://github.com/acme/myrepo/pull/42")
        self.assertEqual(owner, "acme")
        self.assertEqual(repo, "myrepo")
        self.assertEqual(number, 42)

    def test_parses_url_with_trailing_path(self) -> None:
        """PR URL with trailing path segments still extracts core fields."""
        owner, repo, number = parse_pr_url("https://github.com/acme/myrepo/pull/42/files")
        self.assertEqual(owner, "acme")
        self.assertEqual(repo, "myrepo")
        self.assertEqual(number, 42)

    def test_parses_url_with_multiple_trailing_segments(self) -> None:
        """PR URL with multiple trailing path segments parses correctly."""
        owner, repo, number = parse_pr_url("https://github.com/my-org/cool-repo/pull/1234/commits")
        self.assertEqual(owner, "my-org")
        self.assertEqual(repo, "cool-repo")
        self.assertEqual(number, 1234)

    def test_raises_on_non_pr_url(self) -> None:
        """A GitHub URL that is not a PR URL raises LaunchError."""
        with self.assertRaises(LaunchError):
            parse_pr_url("https://github.com/acme/myrepo/issues/42")

    def test_raises_on_invalid_url(self) -> None:
        """A non-URL string raises LaunchError."""
        with self.assertRaises(LaunchError):
            parse_pr_url("not-a-url-at-all")

    def test_raises_on_missing_number(self) -> None:
        """A PR path without a number raises LaunchError."""
        with self.assertRaises(LaunchError):
            parse_pr_url("https://github.com/acme/myrepo/pull/")


# ---------------------------------------------------------------------------
# fetch_pr_data
# ---------------------------------------------------------------------------


class TestFetchPrData(TestCase):
    """Tests for fetch_pr_data."""

    def test_returns_parsed_json(self) -> None:
        """When gh succeeds, returns parsed JSON dict."""
        pr_json = {"headRefName": "bak-123-my-feature", "title": "My PR", "body": "Fixes BAK-123"}
        mock_result = MagicMock()
        mock_result.stdout = json.dumps(pr_json)

        with (
            patch("zing_ai.launch.shutil.which", return_value="/usr/bin/gh"),
            patch("zing_ai.launch.subprocess.run", return_value=mock_result),
        ):
            data = fetch_pr_data("acme", "myrepo", 42)

        self.assertEqual(data["headRefName"], "bak-123-my-feature")
        self.assertEqual(data["title"], "My PR")

    def test_raises_when_gh_not_found(self) -> None:
        """When gh is not on PATH, raises LaunchError with install URL."""
        with (
            patch("zing_ai.launch.shutil.which", return_value=None),
            self.assertRaises(LaunchError) as ctx,
        ):
            fetch_pr_data("acme", "myrepo", 42)
        self.assertIn("https://cli.github.com/", str(ctx.exception))

    def test_raises_on_gh_command_failure(self) -> None:
        """CalledProcessError from gh bubbles up as LaunchError."""
        with (
            patch("zing_ai.launch.shutil.which", return_value="/usr/bin/gh"),
            patch(
                "zing_ai.launch.subprocess.run",
                side_effect=subprocess.CalledProcessError(1, "gh", stderr="not found"),
            ),
            self.assertRaises(LaunchError),
        ):
            fetch_pr_data("acme", "myrepo", 99)

    def test_calls_gh_with_correct_args(self) -> None:
        """subprocess.run is called with expected gh arguments."""
        pr_json = {"headRefName": "main", "title": "T", "body": ""}
        mock_result = MagicMock()
        mock_result.stdout = json.dumps(pr_json)

        with (
            patch("zing_ai.launch.shutil.which", return_value="/usr/bin/gh"),
            patch("zing_ai.launch.subprocess.run", return_value=mock_result) as mock_run,
        ):
            fetch_pr_data("org", "repo", 7)

        call_args = mock_run.call_args[0][0]
        self.assertEqual(call_args[0], "gh")
        self.assertIn("7", call_args)
        self.assertIn("org/repo", call_args)
        self.assertIn("headRefName,title,body", call_args)


# ---------------------------------------------------------------------------
# extract_ticket_id
# ---------------------------------------------------------------------------


class TestExtractTicketId(TestCase):
    """Tests for extract_ticket_id."""

    def test_extracts_from_branch(self) -> None:
        """Ticket ID in branch name is returned first."""
        result = extract_ticket_id("BAK-123-my-feature", "", "")
        self.assertEqual(result, "BAK-123")

    def test_extracts_from_title(self) -> None:
        """Ticket ID in title is found when branch has none."""
        result = extract_ticket_id("feature-branch", "Fix FRO-42 bug", "")
        self.assertEqual(result, "FRO-42")

    def test_extracts_from_body(self) -> None:
        """Ticket ID in body is found when branch and title have none."""
        result = extract_ticket_id("feature-branch", "Some PR", "Closes ENG-99")
        self.assertEqual(result, "ENG-99")

    def test_branch_takes_priority_over_title(self) -> None:
        """When both branch and title have tickets, branch wins."""
        result = extract_ticket_id("BAK-10-thing", "Fixes FRO-20", "")
        self.assertEqual(result, "BAK-10")

    def test_returns_none_when_no_ticket(self) -> None:
        """Returns None when no ticket ID pattern is found anywhere."""
        result = extract_ticket_id("feature-branch", "Some PR title", "No ticket here")
        self.assertIsNone(result)

    def test_uppercases_result(self) -> None:
        """Returned ticket ID is always uppercased — input already uppercase."""
        result = extract_ticket_id("BAK-55-feature", "", "")
        self.assertEqual(result, "BAK-55")

    def test_ignores_short_prefixes(self) -> None:
        """Single-letter prefixes like 'A-1' are not matched (need 2+ letter prefix)."""
        result = extract_ticket_id("A-1", "B-42 fix", "X-99")
        self.assertIsNone(result)
