
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

After resolving the PR, call `session_create(title="PR Review — #{number} {title}", steps=["code-review"])` to get a new session ID and step IDs.

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

### Determine the recommended option

After submitting the review, examine the complexity of all accepted/downgraded findings from the `review_wait()` response. For each finding, use `response.complexity or finding.complexity` (user override first, then agent classification).

Count the findings by complexity and determine the default:
- If **all** accepted/downgraded findings have `complexity == "simple"`: recommend **"Auto-apply all fixes"**
- If accepted/downgraded findings are a mix of simple and standard (but **no** complex): recommend **"Fix with chat"**
- If **any** accepted/downgraded finding has `complexity == "complex"`: recommend **"Build a plan to fix"**

### Present the "What next?" question

Use AskUserQuestion to ask: "What next?" with these options. Append a recommendation note to the description of the recommended option explaining why (e.g., "Recommended — all 5 findings are simple fixes" or "Recommended — 3 findings are complex and need a detailed plan").

- "Auto-apply all fixes" (description: "Fastest — applies fixes without asking. Less control.")
- "Fix with chat" (description: "Walk through each finding interactively. More control, still fast.")
- "Build a plan to fix" (description: "Full plan → audit → build pipeline. Most rigorous, slowest.")
- "I'm done" (description: "Stop here")

### Handling each choice

If "Auto-apply all fixes": proceed to the `auto_apply` step.

If "Fix with chat": proceed to the `discuss_findings` step.

If "Build a plan to fix": invoke the `Skill` tool with skill name `zing` and args set to the report file path (e.g. `.zing/pr-review-123-2025-06-15-1423.md`). Do NOT embed the current session token in the new zing file — it gets its own session when `/zing:plan` picks it up.

If "I'm done": exit normally.

</step>

<step name="auto_apply">
Automatically apply fixes for all accepted/downgraded findings without interactive prompts. This step is self-contained — no separate skill or server-side code is needed.

### Process

1. **Iterate through each accepted/downgraded finding** in the order they appear in the report.

2. **For each finding:**
   a. Read the finding's body (the detailed explanation from the report) and the selected approach option (from `response.selected` / `response.other_text`).
   b. Read the relevant source file(s) referenced in the finding.
   c. Apply the fix directly using Edit/Write tools. For simple findings the fix should be obvious from the finding description and selected approach. For standard findings, use the approach option to guide the implementation.
   d. After applying the fix, check if there are test files related to the changed code (look for `test_*.py`, `*_test.py`, `*.test.ts`, `*.spec.ts`, etc. in the same directory or a `tests/` directory). If test files exist, run the relevant tests via Bash to verify the fix didn't break anything.
   e. If a fix **fails** (tests break, the change is ambiguous, or the code context makes the fix unclear), **fall back to interactive "fix with chat" mode for that finding only**: show the finding to the user, explain what was attempted and why it failed, and ask for guidance. After resolving interactively, continue auto-applying the remaining findings.

3. **After all findings are processed**, show a summary:
   ```
   Auto-apply complete:
   - {applied_count} fixes applied successfully
   - {fallback_count} required interactive resolution
   - {skipped_count} skipped
   ```

4. **Stage and commit all changes** with a descriptive commit message listing the findings addressed:
   ```
   Fix PR review findings: {brief list of finding titles}

   Applied {applied_count} fixes from PR review ({report_file}).
   Findings addressed:
   - #{n}: {finding title}
   - #{n}: {finding title}
   ...
   ```

5. After committing, return to the `submit_review` step's AskUserQuestion — offer "Build a plan to fix" and "I'm done" (without the "Auto-apply" or "Fix with chat" options again).
</step>

<step name="discuss_findings">
Read the report markdown file written in the `write_report` step. Parse each numbered finding from the "Details" section.

Present the first finding — show its number, description, file/line, severity, and the explanation from the report. Include the code snippet. Then say something like:

"What would you like to do with this one? You can ask me to fix it, suggest a different approach, or just say **next** to move on."

Then enter a conversational loop. The user drives the interaction using natural language — there are no menus or structured prompts. Respond naturally to whatever they say:

- **"fix this" / "fix it"** — Write a fix for the current finding. Show what you're changing and why, then apply it with the Edit tool. After applying, confirm what changed and present the next finding.
- **"try X instead" / "what about X?" / "I'd rather do Y"** — The user is steering toward a different solution. Discuss the approach, and if it makes sense, apply it. If it has trade-offs worth knowing, explain them before applying.
- **"next" / "skip"** — Move to the next finding without changes.
- **"done" / "that's enough"** — End the walkthrough early.
- **"why?" / "explain" / "tell me more"** — Go deeper into why this matters — failure modes, examples, what could go wrong in production. Then wait for the user's next input on the same finding.
- **"show me more context"** — Read more of the surrounding code and show it. Then wait for the user's next input.
- **Any other input** — The user might ask a question, disagree, suggest something, or want to explore a tangent. Engage naturally, then continue with the current finding until they say next/fix/done.

After each finding is resolved (fixed, skipped, or discussed), present the next one. Continue until all findings have been addressed or the user says done.

After the walkthrough is complete, return to the `submit_review` step's AskUserQuestion — offer "Build a plan to fix" and "I'm done" (without the "Fix with chat" option again).
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
