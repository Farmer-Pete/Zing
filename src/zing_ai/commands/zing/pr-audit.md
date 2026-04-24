
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

Store the PR number, head branch, base branch, title, body/description, and URL for later use. Read the PR body carefully — it contains the author's intent, context, and any testing notes that inform the review.

### Session setup

After resolving the PR, call `session_create(title="PR Review — #{number} {title}", steps=["code-review"])` to get a new session ID and step IDs.

{% if git.workflow_mode == "worktree" or git.workflow_mode == "ask" -%}
If the zing file's frontmatter contains a `worktree_path:` entry, `cd` to that path before running any subsequent `git` or `gh` commands.
{%- endif %}

Then call `step_start(session_id, step_id)` where `step_id` is the code-review step ID returned by `session_create`. This transitions the step from PENDING to STARTED.

The session ID and step ID will be passed to the shared review steps.
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

<step name="fetch_pr_context">
Fetch all existing comments and review history for full context:

1. **PR comments and review threads:**
```
gh api repos/{owner}/{repo}/pulls/{number}/comments --paginate
gh api repos/{owner}/{repo}/issues/{number}/comments --paginate
gh api repos/{owner}/{repo}/pulls/{number}/reviews --paginate
```

2. **Check for prior reviews by the current user.** From the reviews list, check if the current user (`gh api /user --jq '.login'`) has previously submitted a review. If so:
   - Note the date of the last review.
   - Get the diff since the last review: `git log --since="{last_review_date}" --oneline {base}...HEAD` to identify which commits are new.
   - The review should **primarily focus on changes since the last review**, while still noting any unresolved issues from prior reviews. Mention this scope at the start of the review: "This is a re-review focusing on changes since my last review on {date}."

3. **Read all comment threads** to understand ongoing discussions, resolved issues, and any context the author or other reviewers have provided. Do not re-raise issues that have already been resolved.
</step>

<step name="read_changed_files">
Follow the `read_changed_files` step from the shared review reference.
</step>

<step name="big_picture">
Follow the `big_picture` step from the shared review reference.
</step>

<step name="analyze_changes">

Follow the `diff_preparation` step from the shared review reference.

Follow the `agent_dispatch` step from the shared review reference. The diff stat summary comes from `gh pr diff --stat`. Pass the **session ID** and **step ID** to each agent for agent lifecycle calls (`agent_start`/`agent_stop` only — agents must NOT call `finding_submit`). In addition to the shared agent context, each agent also receives:
- A note of which lines in each assigned file appear in the diff (so agents know which lines can receive line-level comments)
- PR-specific context: PR number `{number}`, head branch `{headRefName}`, base branch `{baseRefName}`
- If this is a re-review: which commits are new since the last review

### Pre-existing issues

If you notice issues in the code that are **outside the scope of this PR** or **predate the changes** (i.e., the code was already broken/problematic before this PR touched it), raise them as findings with type `"pre_existing"` and severity `low`. These findings:

- **Cannot trigger `REQUEST_CHANGES`** — they are informational only
- Should include an option to **create a Linear ticket** to track the issue separately. Add an option with label "File a ticket" and description "Create a Linear ticket to track this separately — no fix needed in this PR."
- If the user selects "File a ticket" during triage, create the ticket using the Linear GraphQL API. Before creating any tickets, ensure a "zing" label exists: query `issueLabels(filter: { name: { eq: "zing" } })` — if no results, create it with `issueLabelCreate(input: { name: "zing", teamId: "<team_id>" })`. Then create the ticket:
```bash
curl -s -X POST https://api.linear.app/graphql \
  -H "Content-Type: application/json" \
  -H "Authorization: $API_KEY" \
  -d '{"query": "mutation { issueCreate(input: { teamId: \"<team_id>\", title: \"...\", description: \"...\", labelIds: [\"<zing_label_id>\"], priority: <1-4>, estimate: <story_points> }) { success issue { identifier url } } }"}'
```
   Use the team ID that matches the repository (see the team IDs in CLAUDE.md). Read the Linear API key from `~/.config/lr/config.json`.

   **Priority**: Set based on the pre-existing issue's impact — `1` (urgent), `2` (high), `3` (medium), `4` (low). Most pre-existing issues will be `3` or `4`.

   **Estimate (story points)**: Estimate the effort to fix the issue — use the team's point scale (typically 1, 2, 3, 5, 8). Consider complexity, scope of changes needed, and testing effort.
- When included in the PR review, place the comment on the relevant diff line if possible. Prefix with "**Pre-existing:** " and include the Linear ticket link if one was created (e.g., "**Pre-existing:** {description}. Tracked in {TICKET-ID} — no fix needed in this PR.").

### Stylistic preferences

Mark findings that are purely stylistic preferences (naming conventions, formatting choices, code organization preferences that don't affect correctness or readability) as **nits**. Prefix the comment body with "**Nit:** " and use severity `low`. Do not request changes for nit-only reviews.
</step>

<step name="present_summary">
Give a brief, natural overview of the PR before diving into findings. Something like:

```
Alright, I've looked through the {count} files changed on PR #{number} (`{branch_name}`). Here's what I found — {total_count} things I want to flag:
```

If no issues were found, just say something like:
```
Looked through everything — nothing jumped out at me. Changes look solid.
```
Submit an approving review and exit.
</step>

<step name="check_and_review">
Follow the `check_and_review` step from the shared review reference.

- **Accepted/downgraded/discuss findings**: Include in the report AND submit as PR line-level comments (see `write_report` and `submit_review` steps).
- **No findings after triage**: Say something like "Looked through everything — nothing survived triage. Changes look solid." Submit an approving review and exit.
</step>

<step name="write_report">
Compile the triaged findings (accepted, downgraded, and discuss items) into a GitHub-flavored markdown file as a local record.

First, ensure the `.zing` directory exists in the current working directory (create it if it doesn't). Write the file to `.zing/pr-review-{number}-{datetime}.md` where `{number}` is the PR number and `{datetime}` is the current date and time in {{ report.datetime_format }} format (e.g. `2025-06-15-1423`).

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

**Approach:** {the user's selected approach — see instructions below}

---

### 2. ...
```

**Including the selected approach:** Each `ReviewItem` returned by `review_wait()` contains a `response` with `selected` and `other_text` fields. For each finding in the report:
- If `response.selected` is set and is NOT `"__other__"`: use the `selected` value as the approach text (it will match one of the finding's `options[].label` values — include the matching option's `description` too, e.g. "**Approach:** Add guard clause — Check for None and return a 404, simple minimal change").
- If `response.selected` is `"__other__"` and `response.other_text` is set: use the `other_text` as the approach (e.g. "**Approach:** Wrap in a try/except and log the error instead").
- If `response.selected` is not set (no approach was chosen): omit the **Approach:** line entirely for that finding.

If zero valid findings, write a short file noting "Nothing to flag — changes looked good."

This file serves as a local record and can be passed to `/zing:plan` to plan fixes.
</step>

<step name="submit_review">
After the report has been written, submit a GitHub PR review using the API. Use the triaged `ReviewItem` list from the `check_and_review` step to build the review payload.

**Determine your suggested review event** based on the triaged findings (not dropped ones):
- If any accepted or downgraded finding has severity `critical` or `high`: suggest `REQUEST_CHANGES`
- If all remaining findings are `medium` or `low`: suggest `COMMENT`
- If zero findings remain after triage: suggest `APPROVE`

Before asking the user, send a browser notification so they know input is needed:
Call `notification_send(session_id, title="PR review complete", body="Review findings are ready. Approve, comment, or request changes.")` where `session_id` is the session ID from the zing file frontmatter.

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
- Include the user's selected approach at the end of the comment. Use the same logic as the `write_report` step: if `response.selected` is set and is NOT `"__other__"`, append `\n\n**Approach:** {selected} — {matching option description}`. If `response.selected` is `"__other__"` and `response.other_text` is set, append `\n\n**Approach:** {other_text}`. If no approach was chosen, omit it.
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
- Do NOT request changes for pre-existing issues — they are informational and tracked via Linear tickets
- Do NOT re-raise issues that have already been resolved in comment threads
- Do NOT request changes for nit-only reviews
</anti_patterns>

<success_criteria>
Review is complete when:

- [ ] Shared review reference was loaded
- [ ] PR was identified (from argument, URL, or current branch)
- [ ] PR body/description was read for context
- [ ] PR branch was checked out locally
- [ ] All existing PR comments and review threads were fetched
- [ ] Prior review by current user detected (if any) and review scoped accordingly
- [ ] Full diff was obtained and all changed files were read
- [ ] Lines eligible for line-level comments were identified from the diff
- [ ] Big-picture assessment shared (sizing, context, relevance)
- [ ] Changes were analyzed against the full review checklist (implementation, logic/bugs, error handling, naming, dependencies, security, performance, usability, testing, production readiness, readability, language-specific, experts)
- [ ] Each finding has a severity and confidence rating
- [ ] Pre-existing issues raised as informational findings with option to file Linear tickets
- [ ] Stylistic preferences marked as nits with low severity
- [ ] Agent findings collected via JSONL return, deduplicated, and submitted via `finding_submit()`
- [ ] Review UI was opened for batch triage via `review_wait()`
- [ ] User triage decisions (accept, drop, downgrade, discuss) were applied
- [ ] Triaged findings were written to a markdown file in `.zing/` in GFM format
- [ ] File path was shown to the user with instruction to run `/zing:plan` on it
- [ ] PR review was submitted via GitHub API with line-level comments
- [ ] Review body, comments, and any generated content do not mention Claude/Codex/OpenCode — only Zing attribution if any
- [ ] PR URL was shown to the user
- [ ] "Zing! Review complete." signoff was displayed
</success_criteria>
