"""Unit tests for command_center aggregation logic."""

from __future__ import annotations

import unittest

from zing_ai.server.command_center import AUDIT_STEP_NAMES, aggregate


class TestAggregateEmptyInputs(unittest.TestCase):
    """The aggregate() entry point returns empty collections for empty inputs."""

    def test_aggregate_empty_inputs(self) -> None:
        inbox, hubs = aggregate(
            issues=[],
            prs=[],
            sessions=[],
            current_username="octocat",
        )
        self.assertEqual(inbox, [])
        self.assertEqual(hubs, [])


class TestAuditStepNames(unittest.TestCase):
    """AUDIT_STEP_NAMES covers the four audit types."""

    def test_audit_step_names_contents(self) -> None:
        self.assertEqual(
            AUDIT_STEP_NAMES,
            frozenset({"plan-audit", "build-audit", "pr-audit", "custom-audit"}),
        )


if __name__ == "__main__":
    unittest.main()
