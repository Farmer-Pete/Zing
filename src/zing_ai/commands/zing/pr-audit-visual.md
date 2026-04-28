
<objective>
Visually validate a GitHub pull request by driving the user's real Chrome browser via the **Claude in Chrome** integration against a deployed preview environment. This is a behavior-level audit, not a code-level one: the goal is to confirm the PR's stated fix actually works end-to-end in the UI, then probe for regressions the change is most likely to introduce, then summarize findings and offer to create follow-up tickets and post a review.

This command is conversational and gated. Claude proposes plans and waits for the user to approve them before clicking, navigating, or doing anything beyond read-only inspection. Claude also warns the user before any non-read-only operation (mutating UI state in the app, posting reviews, creating tickets, etc.).
</objective>

<runtime_requirement>
This command must run inside the **Claude in Chrome** extension (https://claude.ai/chrome). Claude in Chrome drives the user's real Chrome browser, which means the audit inherits whatever sessions, cookies, and SSO state the user already has — no separate sandboxed profile, no cookie injection, no login bridging. If invoked from the CLI / a Zellij session / any non-Chrome surface, the very first step is to stop, tell the user this command is only useful inside Claude in Chrome, and ask them to re-run it from https://claude.ai/chrome with the same arguments.
</runtime_requirement>

<core_principles>
- **Plan, then wait.** Every phase that involves browser interaction begins with Claude writing a plan and ending with an explicit pause for user approval. Never execute a plan immediately after writing it.
- **Read-only by default.** Navigation, reading the page, screenshots, console reads, network reads, and JavaScript expressions that only read state are read-only and do not require a per-action warning (just the up-front plan approval covers them).
- **Warn before mutating.** Anything that changes server state, account state, app state, or external systems is a write action and requires a fresh, explicit confirmation from the user in the chat — even if the original plan mentioned it. This includes: clicking buttons that mutate data (status changes, archive, delete, save, send), submitting forms, posting GitHub reviews, creating Linear tickets, changing settings, accepting cookies/agreements. When in doubt, treat it as a write and ask.
- **Respect the existing safety rules.** The system-level prohibited actions and explicit-permission actions still apply on top of everything in this document. This command never weakens them.
- **Treat the live browser with respect.** Claude in Chrome operates in the user's real browser, with their real auth and real data — there is no sandbox to undo a misstep. Open audits in a fresh tab, never close or reload tabs the user is using, and never log them out.
- **One PR at a time.** Do not chain to fixing the issues, opening more PRs, or running other zing commands automatically. Finish the audit, summarize, offer next steps, stop.
</core_principles>

<browser_interaction_model>
Claude in Chrome exposes the user's live Chrome browser. Prefer reading the rendered page (DOM / accessibility tree) over screenshots — text reads are cheaper and more reliable for finding elements. Reserve screenshots for visual evidence in the report (layout glitches, visual regressions, confirming what something looks like), not for locating elements.

Available action categories (use whichever names Claude in Chrome surfaces — these are the *capabilities* you should exercise):

- Navigate to a URL.
- Open / switch / close tabs (do NOT close tabs you didn't open).
- Read the current page's structure and visible content.
- Take a screenshot of the current viewport (for the report only).
- Click an element identified from the page read.
- Type into an input, fill a form, select a dropdown option.
- Scroll the page / hover an element.
- Wait for specific text or a specific element to appear or disappear (cap waits at a few seconds — if it's still missing after ~{{ thresholds.browser_wait_timeout_seconds }} seconds, stop and tell the user something is wrong rather than retrying indefinitely).
- Inspect network requests (URL, method, status, body) for a specified pattern.
- Read console messages, especially errors.
- Evaluate read-only JavaScript expressions (e.g. `performance.getEntriesByType('resource')`).

If the surface doesn't expose one of these, call it out to the user instead of inventing a workaround.
</browser_interaction_model>

<process>

<step name="resolve_pr">
Determine which PR to audit:

1. If the user provided a PR number (`123`, `#123`), use it directly.
2. If they provided a full GitHub PR URL, extract the number.
3. Otherwise, automatically detect from the current branch — run `gh pr view --json number,headRefName,baseRefName,title,url` and use whatever it returns. Do NOT ask the user to confirm; if a PR exists for the branch, use it.
4. Only if step 3 fails (no PR for the current branch, command errors, or detached HEAD) — use AskUserQuestion to ask the user for a PR number or URL, then stop if they don't provide one.

Once you have the PR number, run:
```
gh pr view {number} --json number,headRefName,baseRefName,title,url,body,author
```

Read the title, body, and author. The PR body usually contains the bug description, repro steps, and the author's reasoning. This is the most important context for the rest of the audit.

Also run `git log {base}...{head} --oneline` (or `gh pr view {number} --json commits`) to see the commit history for additional context.

Briefly summarize back to the user: PR number, title, what the PR claims to fix, and the repro steps you extracted from the body. This sets shared context before any planning.
</step>

<step name="resolve_preview_url">
Find the preview/staging URL where this PR is deployed, in this order:

1. **Check PR comments and body for a preview URL.** Run:
   ```
   gh pr view {number} --json body,comments --jq '{body, comments: [.comments[].body]}'
   ```
   Look for URLs that look like preview environments (e.g. `pr-{number}.preview.*`, `*.vercel.app`, `*.netlify.app`, `deploy-preview-*`, "Preview:", "Deployed at:"). Common patterns: bot comments from Vercel, Netlify, GitHub Actions, or custom CI. If you find one, use it without asking.
2. **If no URL was found in comments**, use AskUserQuestion to ask the user for the preview/staging URL. Offer a guessed convention from the PR number (e.g. `https://pr-{number}.preview.example.com/`) as one option if you can infer the team's pattern from prior conversation context.
3. **Do not invent URLs.** If neither comments nor the user provide one, stop and tell the user this command needs a deployed environment.

Do not try to spin up a local dev server.
</step>

<step name="build_test_plan">
Write a numbered test plan that covers the PR's stated fix. Structure it as:

1. **Setup** — opening a fresh tab, navigating to the preview URL, confirming the user's existing auth state is in effect, picking the right entry point in the app.
2. **Repro path** — the exact clicks/navigations from the PR body's repro steps.
3. **Expected outcome** — what "the fix works" looks like in the UI, in concrete terms (which widgets should populate, what text should appear, what state should be visible).
4. **Read-only verification** — supporting checks that don't mutate state: network requests fired, console errors, page contents.

For each step, describe the action in plain terms ("navigate to X", "read the alerts list", "click the row labelled Y", "screenshot the empty state for the report"). Don't reference tool names — Claude in Chrome will map the action to its own controls.

If any step in the plan involves a click or interaction that could mutate state (status changes, archive, save, submit), call it out explicitly in the plan with a "⚠ write action — will re-confirm before doing this" marker. Most repro paths are read-only navigation, but check.

Also list things you'd need from the user before starting:
- Which environment (preview / staging / prod)
- Which account / role they're currently signed in as in Chrome (the audit will use that account)
- Any specific test data to use vs picking arbitrary records

End the plan with a clear pause: state explicitly that you will not execute anything until they approve the plan or ask for changes.
</step>

<step name="wait_for_plan_approval">
Stop. Do not touch the browser yet.

Use AskUserQuestion to ask the user to approve the plan, with options:
- **Approve** — proceed with the plan as written
- **Modify** — user wants to change something (they'll describe the change in their answer)
- **Reject** — abandon and rewrite

If they choose Modify, incorporate their changes and re-present the plan with another AskUserQuestion approval gate. If they Approve, restate the final plan in one or two sentences before proceeding so there is no ambiguity about what is about to run.

If the plan included any write actions, remind the user explicitly in your follow-up message: "I'll pause again before any write action and ask you to confirm in the chat."
</step>

<step name="execute_repro">
Execute the approved repro plan in Chrome. Standard sequence:

1. Open a **new tab** for the audit. Never reuse or close one of the user's existing tabs.
2. Navigate to the preview URL.
3. Read the page to confirm auth state. Because Claude in Chrome runs in the user's actual browser, they should already be signed in; the page should land on an authenticated route, not a `/login`. If you do hit a sign-in screen, that almost always means the user isn't signed in to the *preview* environment specifically — stop and ask: "I landed on a sign-in page at {url}. Want to sign in to {env} in this tab and tell me when to continue, or stop the audit?" Wait for an explicit "go" before re-reading the page. Never enter credentials yourself.
4. Walk through the repro steps. Read the page to locate elements, then click / type / fill / select to interact.
5. After each meaningful navigation, re-read the page to confirm the new state. Take a screenshot only when visual evidence is needed for the report.
6. Use the wait-for capability for slow loads (wait for specific text or an element to appear), capped at a few seconds. If the page is still loading after ~{{ thresholds.browser_wait_timeout_seconds }} seconds, stop and tell the user something is wrong rather than retrying indefinitely.

**Before any click that would mutate state**, use AskUserQuestion to confirm. The question should quote the specific action (e.g. "About to click 'Archive' on alert X. Proceed?") with options:
- **Yes** — proceed with this action
- **Skip** — skip this action and continue with the rest of the plan
- **Stop** — abort the audit

This applies even if the action was listed in the approved plan. Each write action is a separate AskUserQuestion call — do not batch multiple mutations into one prompt.

While executing, capture:
- Page reads at each meaningful state (primary method for reading content)
- Screenshots at key visual checkpoints for the audit report
- Network requests (filter by the relevant API path)
- Console errors
- Any unexpected loading states, empty states, or layout glitches

If a browser action fails or returns nothing, do not retry it more than 2-3 times. Stop and tell the user what went wrong.
</step>

<step name="report_repro_findings">
After the repro path is done, write a concise findings report for the user:

- **Verdict**: does the PR's stated fix work as advertised?
- **Evidence**: which widgets populated, which network requests fired, which page reads or screenshots show the expected state.
- **Notes / caveats**: anything that worked but seems off (transient empty states, slow loads, cosmetic glitches).

End the response with a single, direct question: **"Want me to think through what regressions this PR could introduce and propose a regression test plan?"** Then stop and wait for the user's answer. Do not offer ticket creation, PR review, or any other follow-up at this point — the only next step on offer is the regression plan.
</step>

<step name="build_regression_plan">
Think about what could regress as a result of this specific change. Read whichever files in the diff are most relevant if you haven't already (`gh pr diff {number}` or targeted reads). Build a list of regression risks specific to *this* PR, not a generic checklist. Examples of categories to consider, depending on the change:

- **Performance / latency** — does the fix add network calls on a critical path? Serial vs parallel?
- **Loading / skeleton states** — does removing a cache mean spinners or empty states now appear where they didn't before?
- **Sibling code paths** — does the same pattern this PR fixes exist in adjacent files? (Code search via Grep / find_referencing_symbols.)
- **State propagation across screens** — if the PR touches mutations or cache, do other screens still update correctly?
- **Pagination / infinite scroll** — does the change affect data fetched after the first page?
- **Direct deep links** — does navigating to the affected URL with no prior cache work correctly?
- **Filter / sort / view variants** — list-style screens often have multiple variants (active, archived, filtered).
- **Tab focus / refetch behavior** — React Query or similar libraries may refetch on focus.
- **Auth boundaries** — does the fix behave differently for users with different roles/permissions?
- **Test additions** — does the PR add a guard test? Worth running it locally? (That's a code-side check, not a browser check.)

For each risk, write one or two sentences explaining the risk AND a concrete way to validate it in the browser. Group "code-only" checks (like "grep for the same pattern elsewhere") separately and call out that they'll be done via Read/Grep, not browser actions.

Same flagging rule as before: any step that involves a write action gets a "⚠ write action — will re-confirm" marker.

End with the same explicit pause: you will not execute anything until the user approves.
</step>

<step name="wait_for_regression_plan_approval">
Stop. Use AskUserQuestion to ask the user to approve the regression plan, with options:
- **Approve** — proceed with the plan as written
- **Modify** — user wants to change something (they'll describe in their answer)
- **Reject** — abandon and skip regression testing

If they choose Modify, incorporate changes and re-present with another AskUserQuestion approval gate. If they Approve, restate the final plan in one or two sentences before proceeding.
</step>

<step name="execute_regression_tests">
Execute the approved regression plan. Same rules as the repro execution step:

- Read-only operations are fine without per-action confirmation.
- **Any write action requires a fresh AskUserQuestion confirmation**, with the same Yes / Skip / Stop options as in the repro execution step, even if it was in the approved plan.
- Capture page reads, screenshots (at key visual checkpoints), network requests, console output, and JS performance entries (e.g. `() => performance.getEntriesByType('resource')`) where relevant.
- Don't retry failing browser actions in a loop. Stop and report.
- For code-side checks, use Read / Grep / Glob, not browser actions.

After each test or coherent group of tests, give the user a brief intermediate result so they can interrupt if something looks wrong.
</step>

<step name="final_report">
Compile a complete findings report. Format:

```
## Test results

**Repro / fix validation**
- ✅ / ⚠ / ❌ {test name} — {one-line result}

**Regression coverage**
- ✅ / ⚠ / ❌ {test name} — {one-line result}

**Code checks**
- ✅ / ⚠ / ❌ {check name} — {one-line result}
```

Then a **Summary** section that calls out:
- Whether the PR is safe to merge
- Any genuine concerns (UX flashes, perf regressions, sibling code paths still buggy, etc.)
- Suggested follow-up tickets (with proposed titles, no creation yet)
- Suggested review action (approve / comment / request changes)

Do NOT create tickets or post the review at this point. Use AskUserQuestion to ask what to do next, with options:
- **Post PR review** — draft and submit a GitHub PR review
- **Create Linear ticket(s)** — file follow-up tickets for any concerns surfaced
- **Both** — do both of the above
- **Stop** — finish the audit, no further actions

Branch into `optional_follow_ups` based on the answer.
</step>

<step name="optional_follow_ups">
Based on the user's response, optionally do any of the following. **Each one is a write action — confirm before executing, even though they're a natural conclusion to the audit.**

**Creating Linear tickets**
- Use the Linear GraphQL API via `curl` per the global `~/.claude/CLAUDE.md` Linear section. Do NOT use the Linear MCP integration.
- Get the API key from `~/.config/lr/config.json` under `.workspaces[activeWorkspace].apiKey`.
- Use the team ID list from the global CLAUDE.md (FRO, BAK, etc).
- Before creating, show the user the title and a description summary, then use AskUserQuestion to confirm with options **Create / Edit / Skip**.
- After creation, paste the issue identifier and URL back to the user.

**Posting a PR review**

Build the review body using the template below. Every test executed during the audit (both repro/fix validation AND regression tests) must appear — do not summarize or omit passing tests.

```markdown
## Visual audit

### Fix validation

| # | Test | Steps | Result |
|---|------|-------|--------|
| 1 | {test name} | {numbered steps to reproduce, e.g. "1. Navigate to /alerts 2. Click row for alert X 3. Check detail panel"} | ✅ Pass / ⚠ Warning / ❌ Fail — {one-line explanation} |
| 2 | ... | ... | ... |

### Regression tests

| # | Test | Steps | Result |
|---|------|-------|--------|
| 1 | {test name} | {numbered steps to reproduce} | ✅ Pass / ⚠ Warning / ❌ Fail — {one-line explanation} |
| 2 | ... | ... | ... |

### Code checks

| # | Check | Result |
|---|-------|--------|
| 1 | {check name, e.g. "Grep for same pattern in sibling files"} | ✅ / ⚠ / ❌ — {one-line result} |

### Summary

{1-3 sentences: overall verdict, key concerns if any, and whether this is safe to merge.}

---

🤖 Created with [Zing](https://github.com/Farmer-Pete/Zing)
```

Key rules for the review body:
- **List every test.** Do not collapse passing tests into "all others passed." Each test is a row.
- **Steps must be concrete.** Write them so a human could reproduce the test manually — specific URLs, element names, sequences of clicks.
- **No Claude/AI/tool attribution** in the body — only the Zing footer above.
- **The Zing footer is mandatory** and must always be the last line, separated by `---`.

Show the user the drafted review body, then use AskUserQuestion to confirm the review event with options **Approve / Comment / Request changes / Edit body / Cancel**.
- Submit via `gh pr review {number} --approve|--comment|--request-changes --body "$(cat <<'EOF' ... EOF)"`.
- Verify the submission with `gh pr view {number} --json reviews --jq '.reviews[-1]'`.

**Filing a fix PR or running other commands**
- Don't. Stop here. The user can run `/zing:plan` or another command separately if they want fixes.
</step>

</process>

<safety_rules>
- This command is only useful inside Claude in Chrome. If invoked elsewhere, stop in the first step and ask the user to re-run from https://claude.ai/chrome — do not try to substitute another browser tool.
- Never click, type, navigate, or run JavaScript in the browser before the user has approved the plan that covers it.
- Never click a button that mutates state (status change, archive, delete, send, save, submit, accept) without a fresh in-chat confirmation, even if it's in an approved plan.
- Never create a Linear ticket, post a GitHub review, or push code without explicit user confirmation in the chat for that specific action.
- Never invent preview URLs. Always ask the user.
- Never bypass the system-level prohibited actions list (banking, credentials, downloads, permissions changes, etc.). This command does not grant any new authority.
- Never log in or authenticate on the user's behalf. If the preview shows a sign-in screen, stop, surface the URL to the user, and ask them to sign in themselves before continuing.
- Never accept cookie banners, terms agreements, or modal dialogs. Defer to the user.
- Never close, reload, or navigate tabs the user opened. Operate only on the new tab the audit created. When the audit ends, leave that tab in place unless the user asks you to close it.
- Never log the user out of any site, even if "logging in fresh" feels cleaner.
- If a page triggers a JavaScript alert/confirm/prompt dialog, stop and ask the user how to handle it. Those dialogs block further browser interaction.
- If browser actions fail repeatedly (more than 2-3 attempts), stop and report. Do not loop.
</safety_rules>

<anti_patterns>
- Do NOT auto-execute the plan you just wrote. Always pause for approval.
- Do NOT batch multiple write actions into one confirmation. Confirm each one separately, or at minimum quote each one explicitly in a single confirmation.
- Do NOT proceed to ticket creation or PR review automatically just because the audit is "done." Always ask.
- Do NOT generate generic regression checklists. Tailor regressions to the actual diff.
- Do NOT click around exploring the app outside the planned scope. Stay focused.
- Do NOT attempt to fix anything you find. This command audits, it doesn't repair.
- Do NOT post review text that includes Claude / AI / tool attribution — only the Zing footer is allowed.
- Do NOT create tickets without showing the title and summary to the user first.
- Do NOT fall back to Playwright, an MCP browser server, or any other browser surface — this command intentionally relies on Claude in Chrome and the user's real browser session. If Claude in Chrome can't do something, surface it to the user instead of routing around it.
- Do NOT reach for a screenshot to find an element on the page — read the page content/structure first; reserve screenshots for the report.
</anti_patterns>

<success_criteria>
- [ ] The audit ran inside Claude in Chrome (not Playwright, not another browser surface)
- [ ] PR was identified and its body/repro steps were summarized back to the user
- [ ] User provided the preview URL (not invented)
- [ ] A repro test plan was written and explicitly approved before any browser interaction
- [ ] The repro path was executed in a fresh tab, never reusing or closing the user's existing tabs
- [ ] If a sign-in screen appeared, Claude stopped and asked the user to sign in — no credentials were entered by Claude itself
- [ ] Every write action during execution was confirmed in chat at the moment of execution
- [ ] Repro findings were reported
- [ ] A regression plan tailored to the diff was written and explicitly approved before execution
- [ ] The regression plan was executed with the same write-action confirmation discipline
- [ ] A final consolidated report was produced (repro + regression + code checks)
- [ ] The user was offered ticket creation and PR review as separate, individually confirmed actions
- [ ] No tickets, reviews, or other write actions were taken without explicit chat confirmation
- [ ] The audit stopped cleanly without auto-chaining into fixing or other commands
</success_criteria>
</output>