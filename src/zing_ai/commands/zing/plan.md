
If $ARGUMENTS is empty or blank, list all markdown files in the `.zing` directory and use the AskUserQuestion tool to let the user pick which one to work on. Use the chosen file as the argument going forward.

Once you have a file, read it. This is something that the user is trying to build. Do not build it yet — first understand the existing codebase, then ask questions, then flesh out the document with an actionable plan.

<process>

<step name="read_zing_doc">
Read the provided zing document to understand what the user wants to build.
</step>

<step name="explore_codebase">
Before asking any questions, explore the codebase to understand the current state of the zing spec.

### Session setup

After reading the zing doc, check for `session` in its YAML frontmatter. If a `session` value is present, use that as the session ID. If there is no `session` in the frontmatter (or no frontmatter at all), call `create_review()` to get a new session ID, then update the zing doc's frontmatter to include `session: {session_id}`. Save the file after updating.

The create_review() call requires: session_id (unique identifier), title (human-readable), zing_file (absolute path to the zing doc — resolve using the working directory before calling). Always resolve the zing file path to an absolute path before passing it to create_review().

After obtaining the session ID, call `start_step(session_id, "planning-questions", {N})` where `{N}` is the number of subagents you will launch. This returns a `step_id` — a unique identifier for this step.

This session ID and step ID (from `start_step`) will be used by subagents to POST their questions to the review server.

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

**Important:** Subagents must NOT call AskUserQuestion directly. Instead, each subagent POSTs its questions to the review server. Include these instructions verbatim in each subagent prompt:

> **Only POST items that require a decision from the user.** Do NOT post statements, analysis results, or confirmations like "X works fine" or "No changes needed" — those belong in your returned findings text, not in the review UI. Every posted item must ask the user to decide something.
>
> The `title` field should be a short question. The `body` field provides context and analysis, and **must end with a clear question** that tells the user what you need them to decide.
>
> **Prefer `choice` type** — provide 2-4 concrete options based on what you found in the codebase. Only use `text` type if the question is truly open-ended and you cannot suggest any reasonable options.
>
> **Choice finding** (preferred — use whenever you can suggest options):
> ```
> Call the `submit_finding` MCP tool with:
>   session_id: SESSION_ID
>   step_id: STEP_ID
>   finding: {"type":"choice","title":"How should we handle the /mcp route conflict?","body":"The FastAPI router has a `/{session_id}` catch-all that would intercept `/mcp`. I found three viable approaches in the codebase.\n\nWhich approach should we use?","options":[{"label":"Mount at /mcp-server","description":"Use app.mount(\"/mcp-server\", mcp_app) with streamable_http_path=\"/\""},{"label":"Restructure routes","description":"Move /{session_id} to /sessions/{session_id} to avoid conflicts"},{"label":"Extract and insert route","description":"Insert the /mcp route before the catch-all in FastAPI's route list"},{"label":"Skip","description":"Not important enough to decide now"}],"context":"routes.py /{session_id} catch-all; MCP SDK streamable_http_app() /mcp route"}
> ```
>
> **Text finding** (only when no reasonable options exist):
> ```
> Call the `submit_finding` MCP tool with:
>   session_id: SESSION_ID
>   step_id: STEP_ID
>   finding: {"type":"text","title":"What naming convention should we use for the new endpoints?","body":"I couldn't find an existing convention for this type of route in the codebase.\n\nWhat naming pattern would you prefer?","context":"No existing convention found"}
> ```
>
> If the tool raises an error, check the error message:
> - `ValueError` = fix the finding data and retry
> - `KeyError` = abort with FATAL error (wrong session/step ID)
> - `RuntimeError` = abort immediately (step already completed)
>
> After posting ALL questions, signal completion:
> ```
> Call the `mark_agent_complete` MCP tool with:
>   session_id: SESSION_ID
>   step_id: STEP_ID
> ```

Replace `PORT`, `SESSION_ID`, and `STEP_ID` in the subagent prompt with the actual port, session ID, and step ID values.

### Phase C — Collect answers from review UI (main thread)

After all subagents return, check each subagent's output for a `FATAL:` prefix. If any agent returned a fatal error, report the error to the user and abort.

Otherwise, call `wait_for_review(session_id)`. This opens the review UI in the browser where the user can see all the planning questions posted by the subagents and answer them all at once. When the user submits the review, `wait_for_review` returns a list of items — each containing the original question, context, and the user's answer.

1. **Merge findings:** Combine all subagent findings (from their returned text output) into a single understanding of the codebase state.
2. **Incorporate answers:** Iterate over the returned review items and incorporate the user's answers into your understanding. Use these answers alongside the merged findings when fleshing out the plan in the next step.
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

End your summary with: "Zing! Plan complete — handing off to audit."

Before chaining to audit, use AskUserQuestion to ask the user:
- "View the plan in a markdown viewer before continuing?"
  - "Yes, open it" — open the file for viewing (see instructions below), then proceed to chain to plan-audit
  - "No, continue" — proceed directly to plan-audit

**Opening the markdown file:** Run `open -a Typora "{file_path}"`. If Typora is not installed (the command fails), fall back to `open "{file_path}"` to use the system default handler.

Then invoke `Skill(skill: 'zing:plan-audit', args: '{file_path}')` where `{file_path}` is the path to the zing document you just updated.
</step>

</process>
