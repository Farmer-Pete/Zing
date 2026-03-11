
<objective>
Perform a code review of existing code like a senior developer would. The user describes what code to audit — either by naming files/directories or describing a code area — and you resolve it to concrete files, read the code, think about what could go wrong, and discuss your concerns with the user. After discussing each finding, write the confirmed ones to a markdown file.
</objective>

<process>

<step name="load_review_reference">
Read the shared review reference file at `~/.claude/commands/zing/_shared/review-core.md` using the Read tool. This contains the tone guidelines, review categories, severity/confidence scales, and other shared review standards used throughout this process.
</step>

<step name="resolve_scope">
Convert the user's description into a concrete set of files to audit.

1. **If no arguments provided**, use AskUserQuestion to ask: "What code should be audited? You can provide file/directory paths (e.g. `src/auth/`), a description (e.g. 'the authentication module'), or a mix of both."

2. **Parse for explicit paths.** Check each whitespace-separated or comma-separated token to see if it resolves to an existing file or directory (via `test -e` or `ls`). Collect hits as "explicit paths." For directories, recursively include all code files within them.

3. **Search for described code.** For any free-text portions that are not explicit paths, use multiple search strategies in parallel:
   - `mcp__serena__find_file` with wildcard patterns derived from key terms (e.g., "authentication" -> `*auth*`, `*login*`, `*session*`)
   - `mcp__serena__search_for_pattern` for key terms in file contents
   - `Glob` for directory structure exploration (e.g., "database layer" -> `**/db/**`, `**/database/**`, `**/models/**`)

4. **Merge and filter.** Deduplicate results. Apply the same exclusion rules as `diff_preparation` from the shared review reference:
   - Lock files, auto-generated code, minified/bundled files, vendored dependencies, binary files, media assets
   Sort files into two groups:
   - **High confidence**: Files from explicit paths, or files whose name/path directly matches the description
   - **Lower confidence**: Files found via content search that are tangentially related

5. **Handle edge cases**:
   - No files found: "Couldn't find any files matching '{description}'. Try a more specific path or description." Exit.
   - Too many files (over 50): Narrow to the top 20-30 most relevant. Warn: "This matches a lot of files ({count}). Narrowing to the most relevant ones — you can adjust the scope if needed."

6. **Confirm with the user.** Present the resolved file list grouped by directory, showing file count and approximate line count. Example:

   ```
   Based on "the authentication module", here's what I'd review:

   src/auth/
     login.ts (142 lines)
     session.ts (89 lines)
     middleware.ts (67 lines)
     types.ts (34 lines)
   src/utils/
     jwt.ts (55 lines)

   5 files, ~387 lines total
   ```

   Use AskUserQuestion:
   - "Looks good, start the audit" (description: "Review these {count} files")
   - "Add or remove files" (description: "Adjust the scope before starting")
   - "Cancel" (description: "Exit without reviewing")

   If "Add or remove files": Ask the user what to add/remove, re-resolve, and confirm again.
   If "Cancel": Exit.

### Session setup

After confirming scope with the user, call `session_create(title="Code Audit — {user_description}", steps=["code-review"])` to get a new session ID and step IDs.

Then call `step_start(session_id, step_id)` where `step_id` is the code-review step ID returned by `session_create`. This transitions the step from PENDING to STARTED.

The session ID and step ID will be passed to the review agents.
</step>

<step name="read_files">
Read the files in scope. Use a tiered strategy based on total scope size:

1. **Calculate total scope size** by summing the line counts of all resolved files.

2. **Tier selection**:
   - **Small scope (under ~2000 total lines)**: Read all files in full using the Read tool (in parallel where possible). Agents receive full file contents.
   - **Medium scope (~2000-5000 total lines)**: Read all files in full. Agents receive full file contents but are instructed to use Serena on-demand for cross-file context.
   - **Large scope (over ~5000 total lines)**: Use `mcp__aid__distill_file` for each file to produce compact API summaries. Read full contents only for files under ~200 lines or files the user explicitly named. Agents receive distilled summaries and use Serena on-demand for deep dives.

3. **Build a file manifest** for agents: For each file in scope, record:
   - File path
   - Line count
   - Language (inferred from extension)
   - Whether full content or distilled summary was prepared

Skip binary files, images, or non-code files that slipped through filtering with a note.
</step>

<step name="big_picture">
Before diving into line-by-line details, step back and share your initial impressions of this area of the codebase in a few sentences — like how you'd describe it to a colleague. Cover:

**Scope & Structure**
- What does this area of the codebase do? Summarize it in your own words.
- How is it organized? Is the structure logical and navigable?
- How large/complex is this area? Is it a tight module or a sprawling subsystem?

**First Impressions**
- Does anything jump out immediately — good or bad?
- Does this code look maintained, or does it show signs of tech debt / neglect?
- Are there obvious patterns or conventions being followed (or violated)?

**Dependencies & Surface Area**
- What does this code depend on? What depends on it?
- How exposed is this code — is it internal plumbing or a public-facing surface?
- Are there parts that look particularly risky or fragile?

Present this as a short, natural paragraph or two — not a checklist. Something like:

"This is the auth module — handles login, session management, and JWT validation. It's about 400 lines across 5 files, reasonably well-organized. The code looks fairly recent and follows the codebase conventions. One thing that stands out right away is {observation}. Let me dig into the details."

If anything at the big-picture level is worth flagging as a finding (e.g., "this module has no tests at all" or "the session logic is duplicated across three files"), add it to your findings list.
</step>

<step name="analyze_code">

**Content preparation:**

For each file in scope, prepare a file context block:

```
=== FILE: src/auth/login.ts (142 lines) ===
{full file content OR distilled summary, depending on tier from read_files step}
```

Apply the same filtering rules from `diff_preparation` in the shared review reference if any files slipped through (lock files, generated code, minified, vendored, binary). Since scope was already filtered at resolution time, this should be minimal.

**Agent dispatch:**

Launch 6 parallel Task agents to review the code. Each agent receives:
- The MCP-only code reading mandate: "Use Serena for code exploration, aid for analysis, CodeGraphContext for architecture. Do not use built-in Read/Grep/Glob for code files."
- The **file manifest** (list of all files in scope with line counts and languages)
- The **file context blocks** (full content or distilled summaries for all files — every agent sees every file)
- Its assigned review categories from the shared review reference (copy the checklist items verbatim into each agent's prompt, with the adaptations listed below)
- The severity/confidence scales from the shared review reference
- The tone guidelines from the shared review reference
- Instructions to use Serena on-demand to explore code beyond the provided context (callers, callees, related modules, test coverage)
- The **session ID** and **step ID** for agent lifecycle calls (`agent_start`/`agent_stop` only — agents must NOT call `finding_submit`)
- Instructions for agent lifecycle and JSONL return format (see below)

**Adapted framing for all agents:**

Include this framing in every agent's prompt:

> "You are reviewing existing code as part of a focused audit. The user asked for a review of: '{user_description}'. Unlike a diff review, you are reviewing the code as it stands today — not changes. Focus on issues that exist in the current code. Flag any real issues you find in the files within scope, regardless of when they were written. Your job is to find real problems, not suggest rewrites."

**Adapted checklist items:**

Some review checklist items from the shared reference are diff-oriented. Reframe them for existing code review:
- "Does this code change accomplish what it is supposed to do?" → "Does this code accomplish what it is supposed to do? Is it fit for purpose?"
- "Is the change necessary, or does it include unnecessary code?" → "Is there dead code, unused branches, or unnecessary complexity?"
- "Does similar functionality already exist in the codebase?" → "Is there duplicated functionality elsewhere in the codebase that could be consolidated?"
- "Have automated tests been added or updated to cover the change?" → "Does this code have adequate test coverage?"
- "Are there tests that prevent regression?" → "Are there tests that would catch regressions in this code?"
- "Were updates to documentation made as required by this change?" → "Is the documentation for this code accurate and up to date?"
- "Are there potential impacts on backward compatibility?" → "Are there compatibility concerns with how this code is used by consumers?"

**Aid tool usage:**

Agents 2, 3, and 5 should use their respective aid tools (`aid_hunt_bugs`, `aid_analyze_security`, `aid_performance_analysis`) more liberally than in diff reviews, since there is no diff to narrow focus. The instruction should say:

> "Use your aid analysis tools on files that are central to the audit scope or that look like they could harbor issues based on their complexity or role. You don't need to run these on every file, but be more liberal than you would in a diff review."

**Agent assignments** (same as shared reference):
- Agent 1 (Architecture & Design): Design, Implementation
- Agent 2 (Correctness & State): Logic Errors (incl. all sub-items), Error Handling
- Agent 3 (Security & API Surface): Security and Data Privacy, Dependencies and Compatibility, API Contract Integrity
- Agent 4 (UI & Readability): Naming, Readability, Language-Specific, Usability and Accessibility, UI Layout Robustness
- Agent 5 (Performance & Data Integrity): Performance (incl. all sub-items)
- Agent 6 (Testing & Observability): Testing and Testability (incl. Test Determinism), Production Readiness, Experts' Opinion

**Agent lifecycle and returning results:**

Each agent must call `agent_start(session_id, step_id, name="{agent-name}", description="{agent description}")` at the very start of its task, and `agent_stop(session_id, step_id, name="{agent-name}")` at the very end (after all analysis is done).

If `agent_start` or `agent_stop` returns an error:
- `KeyError` = abort with FATAL error (wrong session/step ID)
- `ValueError` = fix and retry

**NEVER call `mcp__zing-ai__finding_submit`** — this is forbidden for agents. The parent process collects all agent findings, deduplicates them, and submits them. Agents must only return findings as text. Format each finding as a JSON line and return them all at the end of the task output using this exact format. The `body` field supports GitHub-flavored markdown — follow the `finding_body_format` guidelines from the shared review reference for writing rich, self-contained bodies with embedded code snippets and optional mermaid diagrams:

```
---JSONL---
{"type":"triage","title":"Unchecked null return from get_user()","body":"The handler calls `get_user()` and immediately accesses `.email` without checking for `None`. If the user ID doesn't exist in the database, this will raise an `AttributeError` in production.\n\nHere's the handler:\n\n```python\ndef handle_request(user_id: str):\n    user = get_user(user_id)\n    send_email(user.email, \"Welcome!\")  # user can be None here\n    return {\"status\": \"ok\"}\n```\n\nThe problem is that `get_user()` returns `None` when the ID is not found (see `db.py:47`), but this code path assumes it always succeeds.","category":"correctness","severity":"high","confidence":"high","location":{"file":"src/handlers.py","line":42},"options":[{"label":"Add guard clause","description":"Check for None and return a 404 — simple, minimal change"},{"label":"Return early with error response","description":"Raise a typed UserNotFoundError so the error handler produces a consistent API response"}]}
```

Each line after `---JSONL---` must be a single valid JSON object — one line per finding. If the agent has no findings, omit the `---JSONL---` marker entirely.

Launch all 6 agents in parallel using 6 `Task` tool calls in a single message with `subagent_type: "general-purpose"`. Each agent's prompt must include these mandates verbatim:
1. "Use Serena for code exploration, aid for analysis, CodeGraphContext for architecture. Do not use built-in Read/Grep/Glob for code files."
2. "Do NOT call mcp__zing-ai__finding_submit — return findings as JSONL text only. The parent process handles submission."

**After all 6 agents return**, the parent:
1. Checks each agent's output for a `FATAL:` prefix. If any agent returned a fatal error, report the error to the user and abort.
2. Otherwise, proceeds to the `check_and_review` step.
</step>

<step name="present_summary">
Give a brief, natural overview of the audit scope before diving into findings. Something like:

```
Alright, I've gone through the {count} files in the audit scope ({user_description}). Here's what I found — {total_count} things I want to flag:
```

If no issues were found, just say something like:
```
Went through everything — nothing jumped out. This code looks solid.
```
Write an empty findings report and exit.
</step>

<step name="check_and_review">
Follow the `check_and_review` step from the shared review reference.

- **Accepted/downgraded/discuss findings**: Include in the report (see `write_report` step).
- **No findings after triage**: Say something like "Went through everything — nothing survived triage. This code looks solid." Write an empty findings report and exit.
</step>

<step name="walk_through_findings">
Follow the `walk_through_findings` guidelines from the shared review reference. Code snippets come from the actual file content at the relevant line numbers (not from diffs).
</step>

<step name="write_report">
Compile the triaged findings (accepted, downgraded, and discuss items) into a GitHub-flavored markdown file.

First, ensure the `.zing` directory exists in the current working directory (create it if it doesn't). Write the file to `.zing/code-audit-{scope_slug}-{datetime}.md` where `{scope_slug}` is a sanitized short version of the user's description (max ~30 chars, lowercase, spaces/slashes replaced with dashes) and `{datetime}` is the current date and time in YYYY-MM-DD-HHmm format (e.g. `2025-06-15-1423`).

Use this structure:

```markdown
# Code Audit — {user_description}

Audited on {YYYY-MM-DD}. {file_count} files reviewed ({total_lines} lines). {valid_count} issues worth flagging out of {total_count} things I looked at.

## Scope

- `src/auth/login.ts` (142 lines)
- `src/auth/session.ts` (89 lines)
- ...

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

After writing, tell the user:
```
Wrote {valid_count} findings to {file_path}

To start working on these fixes, run: /zing:plan {file_path}
```

If zero valid findings, write a short file noting "Nothing to flag — code looked good." and tell the user.
</step>

<step name="post_review">
End your review summary with: "Zing! Review complete."

After writing the review report, use AskUserQuestion to ask: "What next?"
- Options:
  - "Fix with chat" (description: "Walk through each finding interactively — faster, fix as you go")
  - "Build a plan to fix" (description: "Systematically plan and build a fix for each finding — slower but more thorough")
  - "I'm done" (description: "Stop here")

If "Fix with chat": proceed to the `discuss_findings` step.

If "Build a plan to fix": invoke the `Skill` tool with skill name `zing` and args set to the report file path (e.g. `.zing/code-audit-auth-module-2025-06-15-1423.md`).

Follow the `attribution_rule` from the shared review reference.

If "I'm done", exit normally.
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

After the walkthrough is complete, return to the `post_review` step's AskUserQuestion — offer "Build a plan to fix" and "I'm done" (without the "Fix with chat" option again).
</step>

</process>

<anti_patterns>
Follow the anti-patterns from the shared review reference, with these additions and overrides:
- Do NOT ask about the report format or file path — just write it
- Do NOT require the user to be on a feature branch — this skill works from any branch
- Do NOT suggest "creating a PR" as a post-review action — this is a code audit, not a branch review
- Do NOT limit the review to recently changed code — review the code as it exists today
- Do NOT flag issues and then caveat with "but this might be intentional since it's existing code" — if it's a real issue, flag it regardless of age
- **OVERRIDE**: The shared anti-pattern "Do NOT flag issues in code that was not changed in this branch/PR" does NOT apply here — agents should flag any real issues in the scoped files
</anti_patterns>

<success_criteria>
Review is complete when:

- [ ] Shared review reference was loaded
- [ ] User's description was resolved into a concrete set of files
- [ ] Resolved scope was confirmed with the user before proceeding
- [ ] All files in scope were read (full content or distilled summary depending on size)
- [ ] Big-picture assessment shared (scope, structure, first impressions, dependencies)
- [ ] Code was analyzed against the full review checklist (implementation, logic/bugs, error handling, naming, dependencies, security, performance, usability, testing, production readiness, readability, language-specific, experts)
- [ ] Each finding has a severity and confidence rating
- [ ] Agent findings collected via JSONL return, deduplicated, and submitted via `finding_submit()`
- [ ] Review UI was opened for batch triage via `review_wait()`
- [ ] User triage decisions (accept, drop, downgrade, discuss) were applied
- [ ] Triaged findings were written to a markdown file in `.zing/` in GFM format
- [ ] File path was shown to the user with instruction to run `/zing:plan` on it
- [ ] If user chose "Discuss findings", each finding was walked through with opportunity for deeper discussion
</success_criteria>
