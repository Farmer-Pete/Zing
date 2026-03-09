
<objective>
Review a GitHub pull request like a senior developer would. Check out the PR branch, read the changes, think about what could go wrong, and talk through your concerns with the user. After discussing each finding, submit a GitHub PR review with line-level comments using `gh api`.
</objective>

<process>

<step name="load_review_reference">
Read the shared review reference file at `~/.claude/commands/zing/_shared/review-core.md` using the Read tool. This contains the tone guidelines, review categories, severity/confidence scales, and other shared review standards used throughout this process.
</step>

<step name="resolve_pr">
Determine which PR to review:

1. If the user provided a PR number (e.g., `123`, `#123`), use that directly.
2. If the user provided a full GitHub PR URL (e.g., `https://github.com/owner/repo/pull/123`), extract the PR number.
3. If the user didn't specify a PR, run `gh pr view --json number,headRefName,baseRefName,title,url` to see if the current branch has an open PR. If not, tell the user: "No open PR found for the current branch. Provide a PR number or URL and try again." Exit.

Once you have the PR number, run:
```
gh pr view {number} --json number,headRefName,baseRefName,title,url,body
```

Store the PR number, head branch, base branch, title, and URL for later use.

### Session setup

After resolving the PR, check if the zing doc (if one was provided as an argument) has `session` in its YAML frontmatter. If a `session` value is present, use that as the session ID. If there is no zing doc, no frontmatter, or no `session` in the frontmatter, call `create_review()` to get a new session ID. If a zing doc exists, update its frontmatter to include `session: {session_id}` and save the file.

The `create_review()` call requires:
- `session_id`: a unique identifier (e.g., `"pr-audit-{number}-{timestamp}"`)
- `title`: e.g., `"PR Review — #{number} {title}"`
- `zing_file`: absolute path to the zing doc if one exists (resolve to absolute first), otherwise omit the parameter

After creating the session, call `start_step(session_id, "code-review", 6)` to initialize the workflow step for the 6 review agents. This returns a `step_id` — a unique identifier for this step.

This session ID, step ID (from `start_step`), and the server port will be passed to the shared review steps.
</step>

<step name="checkout_pr">
Check out the PR branch locally:

```
gh pr checkout {number} --branch pr-review-{number}
```

If this fails (e.g., due to uncommitted changes), tell the user and exit.
</step>

<step name="get_diff">
Get the full diff and context:

1. Run `gh pr diff {number}` to get the full diff.
2. Run `gh pr diff {number} --stat` to get a summary of changed files. If `--stat` is not supported, run `git diff {base}...HEAD --stat` instead.
3. Run `git log {base}...HEAD --oneline` to get the commit history.

From the diff output, note which lines in each file appear in diff hunks — you can only place line-level comments on lines that appear in the diff (`+` lines or unchanged context lines). Lines that are only in the old version (`-` lines) cannot receive comments.
</step>

<step name="read_changed_files">
Follow the `read_changed_files` step from the shared review reference.
</step>

<step name="big_picture">
Follow the `big_picture` step from the shared review reference.
</step>

<step name="analyze_changes">

Follow the `diff_preparation` step from the shared review reference.

Follow the `agent_dispatch` step from the shared review reference. The diff stat summary comes from `gh pr diff --stat`. Pass the **session ID** and **server port** to each agent so they can POST findings to the review server. In addition to the shared agent context, each agent also receives:
- A note of which lines in each assigned file appear in the diff (so agents know which lines can receive line-level comments)
- PR-specific context: PR number `{number}`, head branch `{headRefName}`, base branch `{baseRefName}`
</step>

<step name="present_summary">
Give a brief, natural overview of the PR before diving into findings. Something like:

```
Alright, I've looked through the {count} files changed on PR #{number} (`{branch_name}`). Here's what I found — {total_count} things I want to flag:
```

Follow the `present_summary` step from the shared review reference for the table format and confidence mapping.

If no issues were found, just say something like:
```
Looked through everything — nothing jumped out at me. Changes look solid.
```
Submit an approving review and exit.
</step>

<step name="check_and_review">
After all 6 agents return, check each agent's output for a `FATAL:` prefix. If any agent returned a fatal error, report the error to the user and abort.

Otherwise, call `wait_for_review(session_id, "code-review")`. This opens the review UI in the browser where the user can see all findings posted by the 6 review agents. The user triages each finding — accepting, dropping, downgrading severity, or marking for discussion — and submits all decisions at once.

When `wait_for_review` returns, it provides a list of `ReviewItem` objects. Each item contains the original finding data and the user's triage decision:
- **Accepted findings**: Include in the report and submit as PR line-level comments.
- **Dropped findings**: Exclude from the report and PR review entirely.
- **Downgraded findings**: Include in the report and PR review with their adjusted severity.
- **Discuss findings**: Walk through each one conversationally with the user (following the `walk_through_findings` guidelines from the shared review reference for discuss items only), then include in the report and PR review with a note that they were flagged for discussion.

If no findings remain after triage (all dropped), say something like:
```
Looked through everything — nothing survived triage. Changes look solid.
```
Submit an approving review and exit.
</step>

<step name="write_report">
Compile the triaged findings (accepted, downgraded, and discuss items) into a GitHub-flavored markdown file as a local record.

First, ensure the `.zing` directory exists in the current working directory (create it if it doesn't). Write the file to `.zing/pr-review-{number}-{datetime}.md` where `{number}` is the PR number and `{datetime}` is the current date and time in YYYY-MM-DD-HHmm format (e.g. `2025-06-15-1423`).

Use this structure:

```markdown
# PR Review — #{number} `{title}`

Reviewed on {YYYY-MM-DD} against `{baseRefName}`. {count} files changed across {commit_count} commits. {valid_count} issues worth flagging out of {total_count} things I looked at.

PR: {pr_url}

## At a Glance

| # | What | Where | Severity | Confidence |
|---|------|-------|----------|------------|
| 1 | Short natural description | file:line | critical/high/medium/low | high/medium/low |

## Details

### 1. {Natural short description}

`{file_path}:{line_number}` — **{severity}**, {confidence} confidence

{Write the explanation conversationally, with specific references to the code. Include a code snippet showing the problematic lines. For downgraded findings, use the adjusted severity. For discuss findings, include a note: "(Flagged for discussion)".}

```{language}
{relevant code snippet}
`` `

---

### 2. ...
```

If zero valid findings, write a short file noting "Nothing to flag — changes looked good."

This file serves as a local record and can be passed to `/zing:plan` to plan fixes.
</step>

<step name="submit_review">
After the report has been written, submit a GitHub PR review using the API. Use the triaged `ReviewItem` list from the `check_and_review` step to build the review payload.

**Determine your suggested review event** based on the triaged findings (not dropped ones):
- If any accepted or downgraded finding has severity `critical` or `high`: suggest `REQUEST_CHANGES`
- If all remaining findings are `medium` or `low`: suggest `COMMENT`
- If zero findings remain after triage: suggest `APPROVE`

**Ask the user what action to take.** Use AskUserQuestion to present your suggestion and let the user decide. Show your reasoning (e.g. "Found 2 high-severity issues after triage, so I'd suggest requesting changes") and offer all three options — APPROVE, COMMENT, REQUEST_CHANGES — with your suggestion marked as recommended. Use the event the user selects for the submission.

**Get the latest commit SHA** for the PR (needed by the API):
```
gh pr view {number} --json headRefOid --jq '.headRefOid'
```

**Build the review body.** This is the top-level review comment. Write it naturally — a short summary paragraph, like:

"Reviewed {count} files across {commit_count} commits. Found {valid_count} things worth flagging. {One sentence summary of the most important concern, if any.}"

If there are any findings that are NOT tied to a specific diff line (big-picture concerns, or findings on lines outside the diff that can't receive line-level comments), include them in the review body. Format each one clearly with the file path and line number, severity emoji, and explanation — just like you would for a line comment, but written inline in the body.

**Build the line-level comments array from triaged findings.** Process the `ReviewItem` list:
- **Accepted findings**: Create a line-level comment with the original severity.
- **Dropped findings**: Skip entirely — do not include in the review.
- **Downgraded findings**: Create a line-level comment with the adjusted severity.
- **Discuss findings**: Create a line-level comment with a note that this was flagged for discussion (e.g., prefix with "💬 Flagged for discussion: ").

For each included finding that IS tied to a specific diff line, create a comment object:

```json
{
  "path": "relative/path/to/file",
  "line": <the actual file line number from the new version of the file>,
  "body": "<the finding explanation, written naturally like a PR comment>"
}
```

For multi-line findings, use `start_line` and `line` to specify the range:
```json
{
  "path": "relative/path/to/file",
  "start_line": <start line number>,
  "line": <end line number>,
  "body": "<the finding explanation>"
}
```

The `line` value must be a line number in the **new version** of the file that falls within a diff hunk (a `+` line or unchanged context line shown in the diff). Lines that are only in the old version (`-` lines) cannot receive comments — in that case, comment on the nearest `+` or context line.

**Format each comment body** to be a good PR comment:
- Prefix with severity as an emoji: `🔴` critical, `🟠` high, `🟡` medium, `⚪` low (use the adjusted severity for downgraded findings)
- Write the explanation naturally, as you would in a real review
- Include a short code suggestion if the fix is obvious (using GitHub's suggestion syntax if applicable):
  ````
  ```suggestion
  corrected code here
  ```
  ````
- Keep it concise — this is a PR comment, not an essay

**Submit the review** using `gh api`:

```bash
gh api repos/{owner}/{repo}/pulls/{number}/reviews \
  -X POST \
  -f commit_id='{commit_sha}' \
  -f event='{APPROVE|COMMENT|REQUEST_CHANGES}' \
  -f body='{review_body}' \
  --input /tmp/pr-review-payload.json
```

To handle the complex JSON payload with the comments array, write the full JSON body to a temporary file first, then use `--input`:

```bash
cat > /tmp/pr-review-payload.json << 'PAYLOAD'
{
  "commit_id": "{commit_sha}",
  "event": "{event}",
  "body": "{review_body}",
  "comments": [
    {
      "path": "src/foo.ts",
      "line": 42,
      "body": "🟠 This could be null here..."
    }
  ]
}
PAYLOAD

gh api repos/{owner}/{repo}/pulls/{number}/reviews --input /tmp/pr-review-payload.json
```

Get the `{owner}/{repo}` from:
```
gh repo view --json nameWithOwner --jq '.nameWithOwner'
```

Follow the `attribution_rule` from the shared review reference.

After submitting, tell the user:
```
Submitted review on PR #{number} ({event}) with {comment_count} line comments.
{pr_url}

Wrote {valid_count} findings to {report_file_path}
To start working on fixes, run: /zing:plan {report_file_path}
```

If the API call fails (e.g., a line comment targets a line the API rejects), retry by moving the rejected line-level comments into the review body instead. Format them the same way as non-diff findings: file path, line number, severity emoji, and explanation. The review should still be submitted — just with fewer line-level comments and more content in the body. If the entire API call fails, fall back to `gh pr review {number} --body '{body}'` with all findings formatted in the body.

End your review summary with: "Zing! Review complete."

</step>

</process>

<anti_patterns>
Follow the anti-patterns from the shared review reference, plus:
- Do NOT place comments on lines that don't appear in the diff — the GitHub API will reject them
- Do NOT submit a review without the user triaging findings in the batch review UI first
</anti_patterns>

<success_criteria>
Review is complete when:

- [ ] Shared review reference was loaded
- [ ] PR was identified (from argument, URL, or current branch)
- [ ] PR branch was checked out locally
- [ ] Full diff was obtained and all changed files were read
- [ ] Lines eligible for line-level comments were identified from the diff
- [ ] Big-picture assessment shared (sizing, context, relevance)
- [ ] Changes were analyzed against the full review checklist (implementation, logic/bugs, error handling, naming, dependencies, security, performance, usability, testing, production readiness, readability, language-specific, experts)
- [ ] Each finding has a severity and confidence rating
- [ ] Findings were posted to the review server by subagents
- [ ] Review UI was opened for batch triage via `wait_for_review()`
- [ ] User triage decisions (accept, drop, downgrade, discuss) were applied
- [ ] Triaged findings were written to a markdown file in `.zing/` in GFM format
- [ ] File path was shown to the user with instruction to run `/zing:plan` on it
- [ ] PR review was submitted via GitHub API with line-level comments
- [ ] Review body and comments do not mention Claude/Codex/OpenCode — only Zing attribution if any
- [ ] PR URL was shown to the user
- [ ] "Zing! Review complete." signoff was displayed
</success_criteria>
