"""Field metadata for the /config UI: labels, types, descriptions, groups."""

from __future__ import annotations

FIELD_META: dict[str, dict] = {
    # category: thresholds
    "thresholds.large_file_lines": {
        "label": "Large file line cutoff",
        "field_type": "number",
        "description": {
            "short": "Files longer than this are read in chunks instead of all at once.",
            "long": "Lower it on memory-tight machines; higher means agents grab more context per `Read` call.",  # noqa: E501
        },
        "group": "File reading",
    },
    "thresholds.audit_always_read_lines": {
        "label": "Audit always-read cutoff",
        "field_type": "number",
        "description": {
            "short": "Files smaller than this are always read in full during audits.",
            "long": "Prevents agents from skipping small-but-important files like configs and helpers.",  # noqa: E501
        },
        "group": "File reading",
    },
    "thresholds.branch_name_max_length": {
        "label": "Branch name max length",
        "field_type": "number",
        "description": {
            "short": "Max length of auto-generated git branch names.",
            "long": "Long slugs get truncated; some hosts reject very long branch names.",
        },
        "group": "Naming limits",
    },
    "thresholds.scope_slug_max_length": {
        "label": "Scope slug max length",
        "field_type": "number",
        "description": {
            "short": "Max length of slugs Zing generates from custom-audit scopes.",
            "long": "Used in filenames and report paths, so shorter keeps things readable.",
        },
        "group": "Naming limits",
    },
    "thresholds.simple_spec_max_words": {
        "label": "Simple spec max words",
        "field_type": "number",
        "description": {
            "short": "Specs shorter than this skip the planning phase entirely.",
            "long": 'Tiny tweaks ("rename X to Y") don\'t need a multi-step plan — Zing jumps straight to build.',  # noqa: E501
        },
        "group": "Planning",
    },
    "thresholds.plan_small_step_count": {
        "label": "Plan small step count",
        "field_type": "number",
        "description": {
            "short": "Plans with this many or fewer steps skip plan-audit.",
            "long": "Small plans don't justify the cost of running evaluation agents over them.",
        },
        "group": "Planning",
    },
    "thresholds.step_merge_min_words": {
        "label": "Step merge min words",
        "field_type": "number",
        "description": {
            "short": "Plan steps shorter than this are flagged as merge candidates.",
            "long": "Tiny steps usually mean over-fragmented planning — Zing suggests merging adjacent ones.",  # noqa: E501
        },
        "group": "Planning",
    },
    "thresholds.step_merge_max_words": {
        "label": "Step merge max words",
        "field_type": "number",
        "description": {
            "short": "Plan steps longer than this are protected from being merged.",
            "long": "Big steps would balloon further if combined with neighbors.",
        },
        "group": "Planning",
    },
    "thresholds.small_diff_max_files": {
        "label": "Small diff max files",
        "field_type": "number",
        "description": {
            "short": "Diffs touching fewer files than this use the lightweight review path.",
            "long": "Small diffs run a quicker audit with fewer agents to save time and tokens.",
        },
        "group": "Diff & audit sizing",
    },
    "thresholds.small_diff_max_lines": {
        "label": "Small diff max lines",
        "field_type": "number",
        "description": {
            "short": "Diffs with fewer changed lines than this also use the lightweight review path.",  # noqa: E501
            "long": "Combined with the file-count threshold, decides whether a PR gets a full or fast review.",  # noqa: E501
        },
        "group": "Diff & audit sizing",
    },
    "thresholds.audit_scope_small_lines": {
        "label": "Audit scope small tier",
        "field_type": "number",
        "description": {
            "short": "Codebases smaller than this are read in full during audits.",
            "long": "Small projects fit in context — no need for on-demand exploration.",
        },
        "group": "Diff & audit sizing",
    },
    "thresholds.audit_scope_medium_lines": {
        "label": "Audit scope medium tier",
        "field_type": "number",
        "description": {
            "short": "Codebases under this size use on-demand exploration during audits.",
            "long": "Larger projects rely on grep and symbol search before any full reads.",
        },
        "group": "Diff & audit sizing",
    },
    "thresholds.scope_max_files": {
        "label": "Scope max files",
        "field_type": "number",
        "description": {
            "short": "Custom audits matching more files than this get auto-narrowed.",
            "long": "Without narrowing, audits over hundreds of files lose focus and waste tokens.",
        },
        "group": "Scope",
    },
    "thresholds.scope_narrow_target": {
        "label": "Scope narrow target",
        "field_type": "number",
        "description": {
            "short": "When narrowing a too-wide scope, Zing trims it to about this many files.",
            "long": "Picks the files most relevant to the audit objective — override if the trim missed something.",  # noqa: E501
        },
        "group": "Scope",
    },
    "thresholds.comment_truncation_chars": {
        "label": "Comment truncation chars",
        "field_type": "number",
        "description": {
            "short": "Max chars shown per PR comment in the pr-respond summary view.",
            "long": "Cosmetic only — does **not** affect comments posted back to GitHub.",
        },
        "group": "Misc",
    },
    "thresholds.browser_wait_timeout_seconds": {
        "label": "Browser wait timeout (s)",
        "field_type": "number",
        "description": {
            "short": "How long visual audits wait for pages to load before giving up.",
            "long": "Increase if your dev server is slow to start; decrease to fail fast on broken pages.",  # noqa: E501
        },
        "group": "Misc",
    },
    # category: models
    "models.plan_exploration": {
        "label": "Plan exploration model",
        "field_type": "text",
        "description": {
            "short": "Model used by exploration subagents during planning.",
            "long": "Cheaper models work fine — exploration is breadth-first lookup.",
        },
    },
    "models.plan_audit": {
        "label": "Plan audit model",
        "field_type": "text",
        "description": {
            "short": "Model used by plan-audit evaluation agents.",
            "long": "A higher-capability model is recommended — these agents critique your plan before build starts.",  # noqa: E501
        },
    },
    "models.build_step": {
        "label": "Build step model",
        "field_type": "text",
        "description": {
            "short": "Model used to execute each build step.",
            "long": "This is the workhorse; quality here directly shapes the code output.",
        },
    },
    "models.review_agents_1_3": {
        "label": "Review agents 1-3 model",
        "field_type": "text",
        "description": {
            "short": "Model for the Architecture, Correctness, and Security review agents.",
            "long": "Leave empty to inherit `build_step`. Override for a sharper model on these critical reviewers.",  # noqa: E501
        },
    },
    "models.review_agents_4_6": {
        "label": "Review agents 4-6 model",
        "field_type": "text",
        "description": {
            "short": "Model for the UI, Performance, and Testing review agents.",
            "long": "Generally fine on a cheaper model than agents 1–3.",
        },
    },
    # category: git
    "git.workflow_mode": {
        "label": "Workflow mode",
        "field_type": "select",
        "description": {
            "short": "How Zing isolates new work: in a branch, a worktree, in place, or ask each time.",  # noqa: E501
            "long": "Use `branch` for solo work; `worktree` lets you keep multiple zings in flight in parallel.",  # noqa: E501
        },
        "options": ["branch", "worktree", "none", "ask"],
    },
    "git.branch_prefix": {
        "label": "Branch prefix",
        "field_type": "text",
        "description": {
            "short": "Prefix added to every auto-generated branch name.",
            "long": "Makes Zing branches easy to spot in `git branch` and PR lists.",
        },
        "show_when": "$git_workflow_mode === 'branch' || $git_workflow_mode === 'ask'",
    },
    "git.worktree_root": {
        "label": "Worktree root",
        "field_type": "text",
        "description": {
            "short": "Path template for where new worktrees are created.",
            "long": "`{repo}` and `{branch}` are substituted; default puts each worktree as a sibling of the main repo.",  # noqa: E501
        },
        "show_when": "$git_workflow_mode === 'worktree' || $git_workflow_mode === 'ask'",
    },
    "git.zing_init_script": {
        "label": "Worktree init script",
        "field_type": "text",
        "description": {
            "short": "Shell script run after Zing creates a new worktree — typically copies untracked files (`.env`, secrets, build caches) into it.",  # noqa: E501
            "long": "Zing sets `ZING_BRANCH`, `ZING_WORKTREE_PATH`, `ZING_SPEC_FILE`, `ZING_SESSION_ID`. Skipped silently if the file does not exist.",  # noqa: E501
        },
        "show_when": "$git_workflow_mode === 'worktree' || $git_workflow_mode === 'ask'",
    },
    "git.code_dir": {
        "label": "Code directory",
        "field_type": "text",
        "description": {
            "short": "Path to the directory containing your git repositories (e.g. ~/Code).",
            "long": "Path to the directory containing your git repositories (e.g. ~/Code).",
        },
    },
    # category: agents
    "agents.plan_exploration_count": {
        "label": "Plan exploration agent count",
        "field_type": "number",
        "description": {
            "short": "How many exploration subagents run in parallel during planning.",
            "long": "More agents = wider/faster discovery, but higher token cost.",
        },
    },
    "agents.plan_audit_count": {
        "label": "Plan audit agent count",
        "field_type": "number",
        "description": {
            "short": "How many evaluation agents critique a plan during plan-audit.",
            "long": "Each agent reviews from a different angle; more agents catch more issues.",
        },
    },
    "agents.review_small_diff_count": {
        "label": "Review small-diff agent count",
        "field_type": "number",
        "description": {
            "short": "Number of review agents launched for small-diff PRs.",
            "long": "Small PRs only need a couple of reviewers; this caps the cost.",
        },
    },
    "agents.review_large_diff_count": {
        "label": "Review large-diff agent count",
        "field_type": "number",
        "description": {
            "short": "Number of review agents launched for large or full diffs.",
            "long": "Large reviews fan out across more concerns: arch, security, perf, UI, tests.",
        },
    },
    # category: report
    "report.datetime_format": {
        "label": "Report datetime format",
        "field_type": "text",
        "description": {
            "short": "`strftime` format used in report filenames.",
            "long": "Default `%Y-%m-%d-%H%M` sorts chronologically by filename.",
        },
    },
    # category: command_center
    "command_center.linear_api_key": {
        "label": "Linear API key",
        "field_type": "password",
        "description": {
            "short": "Personal API key for Linear.",
            "long": (
                "Used by the Command Center to poll your open issues."
                " Generate one at Linear > Settings > API."
            ),
        },
        "group": "Command Center",
    },
    "command_center.github_token": {
        "label": "GitHub token",
        "field_type": "password",
        "description": {
            "short": "Personal access token for GitHub.",
            "long": "Used by the Command Center to poll open PRs. Needs `repo` scope.",
        },
        "group": "Command Center",
    },
    "command_center.poll_seconds": {
        "label": "Poll interval (s)",
        "field_type": "number",
        "description": {
            "short": "How often the Command Center polls Linear and GitHub for updates.",
            "long": "Lower values give fresher data but increase API request volume.",
        },
        "group": "Command Center",
    },
    "command_center.claude_flags": {
        "label": "Claude flags",
        "field_type": "text",
        "description": {
            "short": "Extra flags passed to `claude` on launch.",
            "long": "Extra flags passed to `claude` on launch (e.g. `--model sonnet`).",
        },
        "group": "Command Center",
    },
    "command_center.iterm2_integration": {
        "label": "iTerm2 integration",
        "field_type": "checkbox",
        "description": {
            "short": "Attach to tmux sessions via iTerm2 control mode.",
            "long": (
                "When enabled, the 'Attach to Session' button opens the tmux session "
                "in a native iTerm2 window using control mode (tmux -CC). macOS only.\n\n"
                "**Setup:** In iTerm2, go to Settings > General > tmux and enable "
                '"Automatically hide the tmux client session after connecting" '
                "to prevent a second control window from appearing."
            ),
        },
        "group": "Command Center",
        "platform": "darwin",
    },
}
