
If $ARGUMENTS is empty or blank, list all markdown files in the `.zing` directory and use the AskUserQuestion tool to let the user pick which one to work on. Use the chosen file as the argument going forward.

Once you have a file, read it. This is something that the user is trying to build. Do not build it yet — first understand the existing codebase, then ask questions, then flesh out the document with an actionable plan.

<process>

<step name="read_zing_doc">
Read the provided zing document to understand what the user wants to build.
</step>

<step name="assess_complexity">
After reading the zing document, assess the complexity of the spec to decide whether to run the full exploration flow or a fast inline path.

### Heuristics — classify as **simple** only if ALL of the following are true:

1. **Word count:** The spec body (excluding YAML frontmatter, headings, and metadata) is under 150 words.
2. **Scope:** The spec mentions a single file or a very small, clearly bounded scope.
3. **Language:** The spec uses bug-fix / typo / config-change language — it contains at least one of: "fix", "typo", "rename", "config", "update value", "change default".
4. **No expansion signals:** The spec does NOT mention multiple components, new features, architectural changes, or new files to create.

If **all four** conditions are met, the task is `simple`. Otherwise it is `standard`.

### Write the classification to frontmatter

Update the zing document's YAML frontmatter to include:

```yaml
complexity: simple   # or: complexity: standard
```

Save the file after updating.

### Fast path for simple tasks

If the task is `simple`:

1. Do NOT launch investigation subagents.
2. Do NOT call `review_wait` for planning questions.
3. Instead, based on the spec alone, write a concise Action Plan directly inline — 1 to 3 steps, each specific enough for a build agent to execute (name the exact file, function, or line involved).
4. Also write a **Relevant Files** section listing only the files that need to change.
5. Generate the **Progress** section immediately after the Action Plan. Use this exact format:

   ```
   ## Progress

   - [ ] Step 1: {description}
   - [ ] Step 2: {description}
   - [ ] Step 3: {description}
   ...
   ```

   All items start unchecked.
6. Save the updated document.
7. Skip to the `next_steps` step — the `explore_codebase` and `flesh_out_document` steps are skipped entirely.

### Standard path

If the task is `standard`, continue to the `explore_codebase` step as normal.
</step>

<step name="explore_codebase">
Before asking any questions, explore the codebase to understand the current state of the zing spec.

**This step runs only for `standard` complexity tasks.** If `complexity: simple` was written to frontmatter in `assess_complexity`, skip this step entirely and proceed to `next_steps`.

### Session setup

After reading the zing doc, parse its YAML frontmatter. Extract the `session` value (session ID) and the `steps` mapping (which maps step names like `plan`, `plan-audit`, `build`, `build-audit` to their step IDs).

If there is no `session` in the frontmatter (or no frontmatter at all), this is a standalone invocation. Call `session_create(title)` to get a new session ID and step IDs, then update the zing doc's frontmatter to include `session: {session_id}` and `steps:` with the returned step ID mapping. Save the file after updating.

Once you have the session ID and step IDs, resolve the zing file path to an absolute path and call `session_update(session_id, zing_file=abs_path, title=doc_title)` to associate the zing file with the session.

Then call `step_start(session_id, steps.plan)` where `steps.plan` is the plan step ID from the frontmatter. This transitions the plan step from PENDING to STARTED.

The session ID and plan step ID will be used by subagents for agent lifecycle tracking.

### Phase A+B — Identify areas AND launch all subagents (ONE response)

After reading the zing document, identify 3-5 discrete investigation areas based on what the spec describes. Examples of areas (adapt to the actual spec):
- "Data model" — database schemas, ORM models, migrations
- "API layer" — routes, controllers, middleware, request/response types
- "Frontend components" — UI components, state management, pages
- "Test infrastructure" — test setup, fixtures, helpers, coverage
- "Config/deployment" — environment config, CI/CD, infrastructure

Choose areas that are relevant to the specific zing document. Each area should be distinct enough that a separate agent can explore it independently without overlapping with others.

**CRITICAL PARALLELISM REQUIREMENT:** You MUST do ALL of the following in a SINGLE response — the text announcement AND every Task tool call together:

1. Output text listing each investigation area (so the user knows what's happening)
2. Make ALL 3-5 Task tool calls in that SAME response

This means your response contains text PLUS multiple tool calls. Do NOT:
- Send the announcement text first, then launch subagents in a follow-up message
- Launch one subagent, wait for it, then launch the next
- Launch subagents in separate messages

Here is the structure your response MUST follow:
```
[Text: "Launching N subagents to explore the codebase in parallel:
1. **Area 1** — description
2. **Area 2** — description
..."]
[Task tool call for area 1]
[Task tool call for area 2]
[Task tool call for area 3]
... all in the SAME response
```

Each subagent receives a prompt with:

1. The full zing document content
2. Its assigned investigation area and what to look for
3. The MCP-only mandate (below)
4. Instructions to return findings and proposed questions (below)

**MCP-only mandate for each subagent prompt (include verbatim):**
> Use Serena for code exploration, aid for analysis, CodeGraphContext for architecture. Do not use built-in Read/Grep/Glob for code files. Built-in tools are fine for non-code files (markdown, JSON, YAML, configs, .zing docs).
> - Use Serena's `find_file` to locate relevant source files, and `get_symbols_overview` / `find_symbol` to explore them
> - Use Serena's `search_for_pattern` to search for key terms, patterns, and interfaces mentioned in the zing document
> - Use `mcp__aid__distill_file` or `mcp__aid__distill_directory` for compact structure summaries
> - Use `mcp__CodeGraphContext__analyze_code_relationships` for understanding call chains and dependencies

**Each subagent must return its results in this format:**
```
## Findings: {area name}

{Bulleted list of what exists in the codebase relevant to this area — files found, patterns observed, tech stack details, existing interfaces, gaps identified}
```

**Important:** Subagents must NOT call AskUserQuestion directly. Instead, each subagent tracks its lifecycle via MCP tools and returns its findings as JSONL. Include these instructions verbatim in each subagent prompt:

> **Agent lifecycle:** At the very start of your task, call `agent_start(session_id, step_id, name, description)` where `name` is your area name (e.g. "data-model") and `description` is a short phrase describing what you're investigating. At the very end (after all analysis is done), call `agent_stop(session_id, step_id, name)`.
>
> **Only create findings that require a decision from the user.** Do NOT create findings for statements, analysis results, or confirmations like "X works fine" or "No changes needed" — those belong in your returned findings text, not in the review UI. Every finding must ask the user to decide something.
>
> The `title` field should be a short question. The `body` field provides context and analysis, and **must end with a clear question** that tells the user what you need them to decide. The body supports full markdown — use code snippets, tables, and mermaid diagrams where they help. Always prefer showing code over describing it.
>
> **Prefer `triage` type with `options`** — provide 2-4 concrete options based on what you found in the codebase. Only use `text` type if the question is truly open-ended and you cannot suggest any reasonable options.
>
> **Do NOT call `finding_submit` directly.** Instead, format each finding as a JSON line and return them all at the end of your task output using this exact format:
>
> ```
> ## Findings: {area name}
> ...bulleted findings text...
>
> ---JSONL---
> {"type":"triage","title":"How should we handle the /mcp route conflict?","body":"The FastAPI router has a `/{session_id}` catch-all that would intercept `/mcp`. I found three viable approaches in the codebase.\n\nWhich approach should we use?","options":[{"label":"Mount at /mcp-server","description":"Use app.mount(\"/mcp-server\", mcp_app) with streamable_http_path=\"/\""},{"label":"Restructure routes","description":"Move /{session_id} to /sessions/{session_id} to avoid conflicts"},{"label":"Extract and insert route","description":"Insert the /mcp route before the catch-all in FastAPI's route list"}]}
> {"type":"text","title":"What naming convention should we use for the new endpoints?","body":"I couldn't find an existing convention for this type of route in the codebase.\n\nWhat naming pattern would you prefer?"}
> ```
>
> Each line after `---JSONL---` must be a single valid JSON object. If you have no findings that require user decisions, omit the `---JSONL---` section entirely.
>
> If `agent_start` or `agent_stop` returns an error:
> - `KeyError` = abort with FATAL error (wrong session/step ID)
> - `ValueError` = fix and retry

Replace `SESSION_ID` and `STEP_ID` in the subagent prompt with the actual session ID and plan step ID values.

### Phase C — Collect results and submit to review UI (main thread)

After all subagents return, check each subagent's output for a `FATAL:` prefix. If any agent returned a fatal error, report the error to the user and abort.

Otherwise, collect and deduplicate findings from all subagents:

1. **Parse JSONL from each subagent:** For each subagent's return text, split on the `---JSONL---` marker. If present, parse each subsequent non-empty line as a JSON object. These are the finding objects.

2. **Deduplicate findings:** Across all subagents, deduplicate findings by exact match on the `(type, title)` tuple — two findings are duplicates if and only if they have the same `type` string and the same `title` string. When duplicates are found, keep the first occurrence (from the first subagent that returned it) and discard later ones.

3. **Submit findings:** For each unique finding, call `finding_submit(session_id, step_id, finding_data)` where `step_id` is the plan step ID and `finding_data` is the parsed JSON object.

4. **Wait for review:** Call `review_wait(session_id, step_id)` where `step_id` is the plan step ID. The returned JSON includes a `review_url` — display this URL to the user so they can open the review dashboard and answer all planning questions at once. When the user submits the review, `review_wait` returns a list of items — each containing the original question, context, and the user's answer.

5. **Merge findings:** Combine all subagent findings (from the bulleted text above the `---JSONL---` marker in each subagent's output) into a single understanding of the codebase state.

6. **Incorporate answers:** Iterate over the returned review items. Most items will have a direct answer — incorporate those into your understanding.

   For any items the user marked **"Discuss"**, walk through each one conversationally before continuing:
   - Lead with what the subagent found, referencing the relevant code naturally: "In `auth.py` around line 15, ..."
   - Show a short code snippet so the user can see exactly what's being discussed
   - Explain the trade-offs or concerns in plain language
   - Ask the user what they'd like to do and record their decision
   - Vary how you introduce each finding — don't start every one the same way

   Use all answers (both direct and discussed) alongside the merged findings when fleshing out the plan in the next step.
</step>

<step name="flesh_out_document">
After all questions are answered, update the zing document so it can be handed to a junior engineer that frequently misunderstands and forgets things. The updated document must include:

1. All the original content, refined with the user's answers

2. **Mermaid diagrams** where they help the reader understand the system or the changes. Use them when they genuinely add clarity — not for trivial plans. Good candidates include:
   - **Sequence diagrams** for request flows, API interactions, or multi-step processes (e.g., how a user action flows through frontend → API → database → response)
   - **Flowcharts** for decision logic, branching behavior, or state transitions (e.g., authentication flow, error handling paths)
   - **Architecture/component diagrams** for showing how pieces connect or how new components fit into the existing system

   Place diagrams near the content they illustrate. Use standard Mermaid syntax in fenced code blocks (` ```mermaid `).

3. A **Relevant Files** section that lists every file in the codebase that is relevant to this plan, grouped by purpose:
   - **Files to modify** — existing files that will need changes, with a brief note on what changes
   - **Files to create** — new files that need to be written, with a brief note on their purpose
   - **Reference files** — existing files that should be read for context, conventions, or patterns to follow (e.g., "follow the same pattern as this file")
   - **Test files** — existing or new test files relevant to this work

   Use full relative paths from the project root for every file listed.

4. An **Action Plan** section that breaks the entire zing spec down into concrete, actionable steps. Each step should be:
   - Small enough that a single person could complete it in one sitting
   - Ordered by dependency — earlier steps should not depend on later ones
   - Specific — name the exact files, functions, endpoints, models, etc.
   - Testable — it should be clear how to verify the step is done

   Group steps into phases where it makes sense (e.g., "Phase 1: Data Model", "Phase 2: API Endpoints", "Phase 3: Frontend"). Number every step.

   **Context and current behavior:** The action plan should begin with an overview section (before the numbered steps) that explains how the relevant parts of the system currently work at a detailed technical level — what code runs, what data flows where, what the current structure looks like. Be visual in this section — use Mermaid diagrams liberally (sequence diagrams, flowcharts, component diagrams) to show the current architecture, data flows, and how the proposed changes fit in. A picture is worth a thousand words, and diagrams communicate system behavior far more effectively than prose alone. This gives the reader the context they need before diving into individual steps. For individual steps or phases that involve complicated changes, also include a brief explanation of how things work today and how they're changing, rather than just saying "add X to Y". The goal is that a reader who has never seen this codebase can follow the plan without needing to go read the code themselves first.

5. A **Progress** section at the end of the document to track completion. Generate a checklist from the action plan with every step listed. Use this exact format:

   ```
   ## Progress

   - [ ] Step 1: {description}
   - [ ] Step 2: {description}
   - [ ] Step 3: {description}
   ...
   ```

   All items start unchecked. The user (or an agent) will check them off as work is completed.
</step>

<step name="next_steps">
After saving the updated document, print a brief summary of what was added (Relevant Files, Action Plan, Progress sections).

End your summary with: "Zing! Plan complete."

Then show the user the dashboard link where they can view the rendered plan:

> View the plan: {url}?tab=plan

Where `{url}` is the session URL returned by `session_create` (e.g., `http://localhost:{port}/{session_id}`).

Before asking the user, send a browser notification so they know input is needed:
Call `notification_send(session_id, title="Plan ready for review", body="The plan is complete. Review and approve or request changes.")` where `session_id` is the session ID from the zing file frontmatter.

### Simple tasks — chain directly to build

If `complexity: simple` is set in the zing file frontmatter, skip the plan-audit step entirely. Do NOT ask the user whether to proceed to audit. Instead, immediately invoke:

```
Skill(skill: 'zing:build', args: '{file_path}')
```

### Standard tasks — offer plan-audit

For standard complexity tasks, use `AskUserQuestion` to ask if the user wants to make modifications before handing off to audit. Offer two options: one to proceed to audit, and one to make modifications.

If the user chooses to proceed, invoke `Skill(skill: 'zing:plan-audit', args: '{file_path}')` where `{file_path}` is the path to the zing document you just updated.

If the user chooses to make modifications, enter a conversational loop: make the requested changes to the zing document, save it, and continue chatting naturally. When the user says "DONE" (case insensitive) invoke `Skill(skill: 'zing:plan-audit', args: '{file_path}')`.
</step>

</process>
