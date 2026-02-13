
If $ARGUMENTS is empty or blank, list all markdown files in the `.zing` directory and use the AskUserQuestion tool to let the user pick which one to work on. Use the chosen file as the argument going forward.

Once you have a file, read it. This is something that the user is trying to build. Do not build it yet — first understand the existing codebase, then ask questions, then flesh out the document with an actionable plan.

<process>

<step name="read_zing_doc">
Read the provided zing document to understand what the user wants to build.
</step>

<step name="explore_codebase">
Before asking any questions, explore the codebase to understand the current state of the zing spec. This step has two phases:

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

## Proposed Questions

{Numbered list of questions for the user, grounded in specific codebase findings. Each question should reference what was found. Example:
1. "I see you're using Express with middleware pattern X — should this new feature follow the same pattern?"
2. "The existing data model has table Y — does this feature extend that or need a new model?"}
```

**Important:** Subagents must NOT call AskUserQuestion directly. They only return proposed questions as text in their output.

### Phase C — Centralized Q&A (main thread)

After all subagents complete, collect their findings and proposed questions. Then:

1. **Merge findings:** Combine all subagent findings into a single understanding of the codebase state.
2. **Deduplicate questions:** Remove duplicate or near-duplicate questions from different subagents.
3. **Prioritize:** Order questions by importance — architecture decisions and ambiguities first, edge cases and nice-to-haves last.
4. **Ask the user:** Use AskUserQuestion to ask the user, batching up to 4 related questions per call where they share a theme. Ground every question in specific codebase findings from the subagents. Ask about:
   - Ambiguities or gaps in the zing document
   - Decisions that affect architecture or integration with existing code
   - Behavior in edge cases and error scenarios
   - Anything a junior engineer would likely misunderstand or get wrong
</step>

<step name="flesh_out_document">
After all questions are answered, update the zing document so it can be handed to a junior engineer that frequently misunderstands and forgets things. The updated document must include:

1. All the original content, refined with the user's answers

2. A **Relevant Files** section that lists every file in the codebase that is relevant to this plan, grouped by purpose:
   - **Files to modify** — existing files that will need changes, with a brief note on what changes
   - **Files to create** — new files that need to be written, with a brief note on their purpose
   - **Reference files** — existing files that should be read for context, conventions, or patterns to follow (e.g., "follow the same pattern as this file")
   - **Test files** — existing or new test files relevant to this work

   Use full relative paths from the project root for every file listed.

3. An **Action Plan** section that breaks the entire zing spec down into concrete, actionable steps. Each step should be:
   - Small enough that a single person could complete it in one sitting
   - Ordered by dependency — earlier steps should not depend on later ones
   - Specific — name the exact files, functions, endpoints, models, etc.
   - Testable — it should be clear how to verify the step is done

   Group steps into phases where it makes sense (e.g., "Phase 1: Data Model", "Phase 2: API Endpoints", "Phase 3: Frontend"). Number every step.

4. A **Progress** section at the end of the document to track completion. Generate a checklist from the action plan with every step listed. Use this exact format:

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

Then invoke `Skill(skill: 'zing:plan-audit', args: '{file_path}')` where `{file_path}` is the path to the zing document you just updated.
</step>

</process>
