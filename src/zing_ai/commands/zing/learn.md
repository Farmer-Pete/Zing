
<objective>
Mine a repository's merged PR and closed issue history to identify recurring classes of missed production issues, then write repo-specific detection rules to `.zing-learned-rules.md`. These rules are loaded by audit commands during code reviews, so future reviews catch the same classes of issues before they reach production.
</objective>

<process>

<step name="resolve_repo">
Detect the current repository and set up the session.

1. Run `git remote get-url origin` to get the remote URL.
2. Parse the `owner/repo` from the URL. Handle both HTTPS (`https://github.com/owner/repo.git`) and SSH (`git@github.com:owner/repo.git`) formats.
3. Confirm with the user:
   ```
   Repository: {owner}/{repo}
   Lookback: last 50 merged PRs and last 100 closed bug tickets (default)
   Proceed? [Y/n]
   ```
   Allow the user to override the PR lookback count (default: 50) or date range (default: 90 days).
4. Create a session:
   ```
   session_create(title="Learn: {owner}/{repo}", steps=["learn-collect", "learn-analyze"])
   ```
   Store the returned `session_id` and step IDs.
5. Start the collection step:
   ```
   step_start(session_id, learn-collect_step_id)
   ```
</step>

<step name="collect_seer_comments">
Scan merged PRs for Sentry Seer bot comments and classify them by developer response.

1. Fetch the list of merged PRs:
   ```bash
   gh pr list --state merged --json number,title,mergedAt --limit {N}
   ```

2. For each PR, fetch all comments via two separate paginated calls:
   ```bash
   gh api --paginate repos/{owner}/{repo}/pulls/{number}/comments
   gh api --paginate repos/{owner}/{repo}/issues/{number}/comments
   ```

3. Filter for Seer/Sentry bot comments by checking the `user.login` field for `sentry-io[bot]` or `getsentry`, OR by checking the comment body for Sentry issue link markers (e.g., URLs matching `sentry.io/organizations/*/issues/*`).

4. For each Seer comment found, read the reply thread to determine developer acceptance:
   - Collect all subsequent comments in the same thread (replies to the same review comment, or later comments by non-bot authors in the general thread).
   - Classify based solely on the reply text:
     - If the developer acknowledged the issue and indicated they fixed it → `"legit"`
     - If the developer dismissed, disputed, or said the alert was a false positive → skip (do not include)
     - If there are no replies from the developer → `"unconfirmed"` (include; let the analysis agent decide)

5. Collect structured data for each kept comment:
   ```json
   {
     "seer_comment_body": "...",
     "reply_thread_text": "...",
     "acceptance_status": "legit|unconfirmed",
     "file_path": "src/...",
     "pr_number": 123,
     "pr_title": "..."
   }
   ```

6. Use `step_log` to report progress after each batch of PRs:
   ```
   step_log(session_id, learn-collect_step_id, "learn", "Scanned 15/50 PRs, found 3 accepted Seer comments")
   ```

7. **Empty-dataset guard:** If zero Seer comments were found across all PRs, log a warning:
   ```
   step_log(session_id, learn-collect_step_id, "learn",
     "No Seer/Sentry bot comments found in the last {N} merged PRs. The repo may not have Sentry Seer integration enabled.")
   ```
   Continue to the next collection step — do not abort yet.
</step>

<step name="collect_bug_fixes">
Scan closed bug tickets and their linked PRs to extract root causes and fix patterns.

1. Fetch closed bug issues with common bug labels:
   ```bash
   gh issue list --state closed --label bug,bugfix,bug-fix,hotfix,fix \
     --json number,title,body,closedAt --limit 100
   ```

2. For each bug ticket, find the linked PR using two strategies:
   - **Timeline events:** Fetch the issue timeline and look for `cross-referenced` events that reference a pull request:
     ```bash
     gh api --paginate repos/{owner}/{repo}/issues/{number}/timeline
     ```
     Look for events where `event == "cross-referenced"` and `source.type == "issue"` and `source.issue.pull_request` is present.
   - **Body pattern:** Search the issue body for `Fixes #N`, `Closes #N`, `Resolves #N`, or `Fix #N` patterns and extract the PR number.

3. For each linked PR, fetch the diff and description:
   ```bash
   gh api repos/{owner}/{repo}/pulls/{number} \
     -H "Accept: application/vnd.github.v3.diff"
   ```
   Also fetch the PR description and commit messages:
   ```bash
   gh api repos/{owner}/{repo}/pulls/{number} --jq '{title,body}'
   gh api --paginate repos/{owner}/{repo}/pulls/{number}/commits --jq '.[].commit.message'
   ```

4. Collect structured data for each bug/PR pair:
   ```json
   {
     "bug_description": "...",
     "root_cause_from_pr": "...",
     "fix_diff": "...",
     "affected_files": ["src/..."],
     "pr_number": 456,
     "issue_number": 789,
     "issue_title": "..."
   }
   ```

5. Use `step_log` to report progress:
   ```
   step_log(session_id, learn-collect_step_id, "learn", "Processed 12/30 bug tickets, linked PRs found for 8")
   ```

6. **Empty-dataset guard:** If zero bug tickets were found (the `gh issue list` returned no results), log a warning:
   ```
   step_log(session_id, learn-collect_step_id, "learn",
     "No closed issues found with labels bug/bugfix/bug-fix/hotfix/fix. Check that your repo uses these labels for bug tracking, or that `gh` is authenticated with sufficient permissions.")
   ```

7. **Combined empty-dataset guard:** After both collection passes complete, check if BOTH returned zero items. If so, abort with a clear message to the user:
   ```
   No data collected from either Seer comments or bug tickets. Cannot generate learned rules.

   Check:
   (1) Is `gh` authenticated? Run `gh auth status`.
   (2) Does this repo use Sentry Seer for code review?
   (3) Does this repo use bug/bugfix/hotfix labels on GitHub Issues?
   ```
   Do NOT proceed to `analyze_patterns` — exit the command.
</step>

<step name="analyze_patterns">
Cluster the collected data into recurring issue classes using a subagent, then stop the collection step and start the analysis step.

1. Start the analysis step:
   ```
   step_start(session_id, learn-analyze_step_id)
   ```

2. Register the analysis agent:
   ```
   agent_start(session_id, learn-analyze_step_id, name="analysis", description="Clustering issues into rule classes")
   ```

3. Launch a single Task subagent with `subagent_type: "general-purpose"`. Do NOT specify a `model:` key — let the subagent inherit the parent's model so the best available model is used for clustering quality.

   Construct a self-contained prompt that includes:
   - All collected Seer comments (serialized as JSON)
   - All collected bug fix data (serialized as JSON)
   - Instructions for the subagent:

   ```
   You are analyzing a repository's history of missed production issues to identify
   recurring patterns. Your input is two datasets:

   1. Seer/Sentry bot comments from merged PRs where developers acknowledged real issues
   2. Bug tickets with linked PR fixes showing root causes

   Your job:
   a) Cluster issues into recurring classes — filter out one-offs (issues that appear
      only once and share no pattern with others).
   b) For each cluster of 2+ similar issues, generate a structured rule object.
   c) Anonymize file paths and author references — do not include raw usernames,
      committer emails, or personal information in rule descriptions or examples.

   Return ONLY a delimiter line followed by JSON rule objects, one per line:

   ---RULES---
   {"pattern_name": "...", "description": "...", "detection_criteria": ["...", "..."], "examples": [{"bad": "...", "good": "...", "lang": "python", "bad_comment": "...", "good_comment": "..."}], "severity": "critical|high|medium|low", "source_type": "seer-comment|bug-fix|both", "occurrence_count": 3}
   {"pattern_name": "...", ...}

   Rules:
   - Every rule must be grounded in 2+ actual data points from the input
   - Do not invent or fabricate rules
   - occurrence_count must reflect the actual count from the data
   - severity must use the scale: critical, high, medium, low
   - source_type must be: seer-comment, bug-fix, or both
   - examples.bad/good should be short representative code snippets (10-20 lines max)
   - examples.bad_comment explains what is wrong; examples.good_comment explains the fix
   - Omit examples if no representative code snippet is available in the data
   ```

4. After the subagent returns, parse the `---RULES---` section from its output to extract the rule objects.

5. Stop the analysis agent:
   ```
   agent_stop(session_id, learn-analyze_step_id, name="analysis")
   ```
</step>

<step name="write_rules">
Format the rule objects from the analysis agent and write them to `.zing-learned-rules.md`.

Always overwrite the file if it already exists — this command is designed to be re-run as the codebase evolves.

Use the following exact template:

```markdown
# Learned Review Rules

> Auto-generated by `/zing:learn` on {YYYY-MM-DD}.
> Source: {N} merged PRs, {M} closed bug tickets from {owner/repo}.
> These rules encode patterns of issues that have historically slipped through
> code review in this codebase. Review agents load this file during audits.

## Rules

### 1. {Pattern Name}

**Severity:** {critical|high|medium|low}
**Source:** {seer-comment | bug-fix | both} ({count} occurrences)

**Description:**
{What the class of issue is and why it's easy to miss}

**Detection criteria:**
- {Specific thing to look for}
- {Another specific thing to look for}

**Examples:**

```{lang}
// BAD — {why this is wrong}
{code snippet from actual issue}
```

```{lang}
// GOOD — {why this is correct}
{code snippet from actual fix}
```

---

### 2. {Pattern Name}
...
```

Formatting rules:
- Each rule is numbered sequentially starting at 1.
- The `**Severity:**` field uses the same scale as `review-core.md`: `critical`, `high`, `medium`, `low`.
- Rules are separated by `---` horizontal rules.
- If a rule has no code examples (the analysis agent found no representative snippet), omit the `**Examples:**` block for that rule.
- All review agents receive all rules — do not filter by category. Each agent applies the rules relevant to its domain using its own judgment.
- Write to `.zing-learned-rules.md` in the repository root (the current working directory).

Log completion:
```
step_log(session_id, learn-analyze_step_id, "learn",
  "Wrote {rule_count} rules to .zing-learned-rules.md")
```
</step>

<step name="present_summary">
Display a summary to the user and show the path to the generated file.

```
Learn complete.

Repository:       {owner}/{repo}
PRs scanned:      {N}
Seer comments:    {seer_count} found, {legit_count} accepted by developers
Bug tickets:      {bug_count} analyzed, {linked_count} with linked PRs
Rule classes:     {rule_count} generated

Rules written to: .zing-learned-rules.md

These rules will be loaded automatically by /zing:pr-audit and /zing:custom-audit
during future code reviews.

To commit the rules file:
  git add .zing-learned-rules.md
  git commit -m "Add learned review rules from {owner}/{repo} history"
```

Do NOT commit `.zing-learned-rules.md` automatically — the user decides whether and when to commit it.
</step>

</process>

<anti_patterns>
- Do NOT commit .zing-learned-rules.md — the user decides whether and when to commit it
- Do NOT overwrite any file other than .zing-learned-rules.md
- Do NOT launch the analysis agent if both collection passes returned zero data points
- Do NOT include raw personal data (committer emails, full names, usernames) in rule descriptions or examples — anonymize file paths and author references
- Do NOT invent or fabricate rules — every rule must be grounded in actual collected data from Seer comments or bug tickets
- Do NOT skip the empty-dataset guard — always check and report when no data is found
- Do NOT use the ---JSONL--- delimiter — use ---RULES--- (---JSONL--- is reserved for finding objects in the review pipeline)
- Do NOT filter rules by agent type when loading — all agents receive all rules and apply relevant ones using their own judgment
</anti_patterns>
