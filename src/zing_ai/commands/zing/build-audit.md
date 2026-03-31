
<objective>
Perform a code review of the current branch like a senior developer would. Read the changes, think about what could go wrong, and discuss your concerns with the user. After discussing each finding, write the confirmed ones to a markdown file.
</objective>

<process>

<step name="load_review_reference">
Read the shared review reference file at `~/.claude/commands/zing/_shared/review-core.md` using the Read tool. This contains the tone guidelines, review categories, severity/confidence scales, and other shared review standards used throughout this process.
</step>

<step name="detect_branch_and_diff">
Determine the current branch and its base branch:

1. Run `git branch --show-current` to get the current branch name.
2. Determine the base branch by checking which of `main` or `master` exists: `git rev-parse --verify main 2>/dev/null` and `git rev-parse --verify master 2>/dev/null`. Use whichever exists. If both exist, prefer `main`. If neither exists, use the default remote HEAD via `git symbolic-ref refs/remotes/origin/HEAD 2>/dev/null`.
3. Check if the current branch has an associated PR: `gh pr view --json number --jq '.number' 2>/dev/null`. This works for any branch — whether you created the PR locally, or checked out a remote branch that has a PR (e.g., via `gh pr checkout` or `git checkout -b foo origin/foo`). If a PR is found, use `gh pr diff` for steps 5–6 (this gives the correct GitHub-perspective diff regardless of merge history). If no PR is found, fall back to `git diff` using a fork-point base.
4. **Without PR only:** Try to find the original fork point: `git merge-base --fork-point <base> HEAD 2>/dev/null`. If this succeeds, use the returned commit as `<fork>` in place of `<base>...HEAD` for steps 5–6. If it fails (e.g., reflog expired or fresh clone), fall back to `<base>...HEAD`.
5. Get the full diff:
   - **With PR:** `gh pr diff {number}`
   - **Without PR:** `git diff <fork>..HEAD` (or `git diff <base>...HEAD` if fork-point failed)
6. Get a summary of changed files:
   - **With PR:** `gh pr diff {number} --name-only`
   - **Without PR:** `git diff <fork>..HEAD --stat` (or `git diff <base>...HEAD --stat` if fork-point failed)
7. Run `git log <base>...HEAD --oneline` to get the commit history for this branch.

If the current branch IS the base branch (e.g., user is on `main`), say something like:
"Looks like you're on `{branch}` — there's no feature branch to diff against. Check out the branch you want reviewed and run this again."
Exit.

If there is no diff (no changes), say something like:
"This branch is identical to `{base}` — nothing to review yet."
Exit.

### Session setup

After detecting the branch, parse the zing doc's YAML frontmatter (if one was provided as an argument). Extract the `session` value (session ID) and the `steps` mapping (which maps step names like `plan`, `plan-audit`, `build`, `build-audit` to their step IDs).

If there is no zing doc, no frontmatter, or no `session` in the frontmatter, this is a standalone invocation. Call `session_create(title="Code Review — {branch_name}", steps=["code-review"])` to get a new session ID and step IDs. If a zing doc exists, update its frontmatter to include `session: {session_id}` and the `steps:` mapping, then save the file.

Once you have the session ID, if a zing doc exists, resolve the zing file path to an absolute path and call `session_update(session_id, zing_file=abs_path, title="Code Review — {branch_name}")` to associate the zing file with the session.

Then call `step_start(session_id, steps.build-audit)` where `steps.build-audit` is the build-audit step ID from the frontmatter (or the code-review step ID if this is a standalone invocation). This transitions the step from PENDING to STARTED.

The session ID and step ID will be passed to the shared review steps.
</step>

<step name="read_changed_files">
Follow the `read_changed_files` step from the shared review reference.
</step>

<step name="big_picture">
Follow the `big_picture` step from the shared review reference.
</step>

<step name="analyze_changes">

Follow the `diff_preparation` step from the shared review reference.

Follow the `agent_dispatch` step from the shared review reference. The diff stat summary comes from `git diff --stat`. No additional skill-specific context is needed for agents beyond what the shared reference specifies. Pass the **session ID** and **step ID** to each agent for agent lifecycle calls (`agent_start`/`agent_stop` only — agents must NOT call `finding_submit`).
</step>

<step name="present_summary">
Give a brief, natural overview of the branch before diving into findings. Something like:

```
Alright, I've looked through the {count} files changed on `{branch_name}`. Here's what I found — {total_count} things I want to flag:
```

If no issues were found, just say something like:
```
Looked through everything — nothing jumped out at me. Changes look solid.
```
Write an empty findings report and exit.
</step>

<step name="check_and_review">
Follow the `check_and_review` step from the shared review reference.

- **Accepted/downgraded/discuss findings**: Include in the report (see `write_report` step).
- **No findings after triage**: Say something like "Looked through everything — nothing survived triage. Changes look solid." Write an empty findings report and exit.
</step>

<step name="write_report">
Compile the triaged findings (accepted, downgraded, and discuss items) into a GitHub-flavored markdown file.

First, ensure the `.zing` directory exists in the current working directory (create it if it doesn't). Write the file to `.zing/code-review-{branch_name}-{datetime}.md` where `{datetime}` is the current date and time in YYYY-MM-DD-HHmm format (e.g. `2025-06-15-1423`) and `{branch_name}` has slashes replaced with dashes.

Use this structure:

```markdown
# Code Review — `{branch_name}`

Reviewed on {YYYY-MM-DD} against `{base_branch}`. {count} files changed across {commit_count} commits. {valid_count} issues worth flagging out of {total_count} things I looked at.

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

After writing, tell the user:
```
Wrote {valid_count} findings to {file_path}
```

If zero valid findings, write a short file noting "Nothing to flag — changes looked good." and tell the user.
</step>

<step name="create_pr">
End your review summary with: "Zing! Review complete."

### Determine the recommended option

After writing the review report, examine the complexity of all accepted/downgraded findings from the `review_wait()` response. For each finding, use `response.complexity or finding.complexity` (user override first, then agent classification).

Count the findings by complexity and determine the default:
- If **all** accepted/downgraded findings have `complexity == "simple"`: recommend **"Auto-apply all fixes"**
- Otherwise (any standard or complex findings): recommend **"Fix with chat"**

### Present the "What next?" question

Before asking the user, send a browser notification so they know input is needed:
Call `notification_send(session_id, title="Build audit complete", body="Review findings are ready. Choose how to proceed.")` where `session_id` is the session ID from the zing file frontmatter.

Use AskUserQuestion to ask: "What next?" with these options. Append a recommendation note to the description of the recommended option explaining why (e.g., "Recommended — all 5 findings are simple fixes" or "Recommended — 3 findings are complex and need a detailed plan").

- "Auto-apply all fixes" (description: "Fastest — applies fixes without asking. Less control.")
- "Fix with chat" (description: "Walk through each finding interactively. More control, still fast.")
- "Create a PR" (description: "Create a GitHub PR from the current branch")
- "I'm done" (description: "Stop here")

### Handling each choice

If "Auto-apply all fixes": proceed to the `auto_apply` step.

If "Fix with chat": proceed to the `discuss_findings` step.

If "Create a PR":
1. Run `gh pr create --draft --fill` via Bash to create a draft PR (use --fill to auto-populate from commits)
2. If `gh pr create` fails, show the error message. Before asking the user, send a browser notification so they know input is needed:
   Call `notification_send(session_id, title="PR creation failed", body="The pull request could not be created. Manual intervention needed.")` where `session_id` is the session ID from the zing file frontmatter.
   Use AskUserQuestion:
   - "Try again" (description: "Retry gh pr create --draft --fill")
   - "Try without --fill" (description: "Run gh pr create --draft without --fill, letting gh prompt for title/body")
   - "Skip PR creation" (description: "Continue without creating a PR")
3. Show the user the PR URL

Follow the `attribution_rule` from the shared review reference.

If "I'm done" at the initial question, exit normally.
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
   Fix code review findings: {brief list of finding titles}

   Applied {applied_count} fixes from code review ({report_file}).
   Findings addressed:
   - #{n}: {finding title}
   - #{n}: {finding title}
   ...
   ```

5. After committing, return to the `create_pr` step's AskUserQuestion — offer "Create a PR" and "I'm done" (without the "Auto-apply" or "Fix with chat" options again).
</step>

<step name="discuss_findings">
Read the report markdown file written in the `write_report` step. Parse each numbered finding from the "Details" section.

Before presenting findings, send a browser notification so they know input is needed:
Call `notification_send(session_id, title="Chat fix mode", body="Ready for interactive fix discussion.")` where `session_id` is the session ID from the zing file frontmatter.

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

After the walkthrough is complete, return to the `create_pr` step's AskUserQuestion — offer "Create a PR" and "I'm done" (without the "Fix with chat" option again).
</step>

</process>

<anti_patterns>
Follow the anti-patterns from the shared review reference, plus:
- Do NOT ask about the report format or file path — just write it
</anti_patterns>

<success_criteria>
Review is complete when:

- [ ] Shared review reference was loaded
- [ ] Current branch and base branch were detected
- [ ] Full diff was obtained and all changed files were read
- [ ] Big-picture assessment shared (sizing, context, relevance)
- [ ] Changes were analyzed against the full review checklist (implementation, logic/bugs, error handling, naming, dependencies, security, performance, usability, testing, production readiness, readability, language-specific, experts)
- [ ] Each finding has a severity and confidence rating
- [ ] Agent findings collected via JSONL return, deduplicated, and submitted via `finding_submit()`
- [ ] Review UI was opened for batch triage via `review_wait()`
- [ ] User triage decisions (accept, drop, downgrade, discuss) were applied
- [ ] Triaged findings were written to a markdown file in `.zing/` in GFM format
- [ ] File path was shown to the user
- [ ] If user chose "Discuss findings", each finding was walked through with opportunity for deeper discussion
</success_criteria>
