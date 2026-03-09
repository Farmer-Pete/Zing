
<objective>
Evaluate a zing plan or technical design document for soundness against established engineering principles including clarity, fitness for purpose, YAGNI, TDD readiness, operability, maintainability, correctness/safety, specificity/executability, and code quality. A key goal is eliminating ambiguity: when an agent opens the evaluated plan, it should know exactly what to build and exactly how to build it, with zero guesswork required.

Read the provided file, perform a rigorous evaluation, present results in the review UI, then propose concrete improvements as choices and apply the ones the user approves.
</objective>

<process>

<step name="parse_arguments">
Parse the command arguments:
- The argument is a file path to the zing plan or technical design document
- Example: `/zing:plan-audit docs/design.md` -> filename = "docs/design.md"
- Example: `/zing:plan-audit .zing/recipe-app.md` -> filename = ".zing/recipe-app.md"

If no arguments provided:
- Use Glob to list all markdown files in the `.zing` directory
- If no files found, show an error and exit:
  ```
  No zing files found in .zing/
  Run /zing:new to create one.
  ```
- If one or more files found, use AskUserQuestion to let the user pick which zing spec to evaluate
- Use the chosen file as the argument going forward
</step>

<step name="read_document">
Read the provided file using the Read tool.

If the file does not exist, try resolving it relative to the working directory. If still not found:

```
ERROR: File not found: <filename>
```

Exit.
</step>

<step name="read_referenced_files">
**MCP-only mandate:** Use Serena for code exploration, aid for analysis, CodeGraphContext for architecture. Do not use built-in Read/Grep/Glob for code files. Built-in tools are fine for non-code files (markdown, JSON, YAML, configs, .zing docs).

Before evaluating, scan the plan document for any referenced files. Look for:

- A "Relevant Files" section or similar (e.g., "Files to modify", "Files to create", "Reference files", "Test files")
- File paths mentioned anywhere in the document (e.g., `src/auth/login.ts`, `models/user.py`, `config/database.yml`)
- Module, class, or component names that can be resolved to files using Glob or Grep

Read ALL referenced files that exist in the codebase. This is critical — you cannot properly evaluate a plan without understanding the code it touches. Read them in parallel where possible.

If a referenced file does not exist (e.g., it's a file the plan proposes to create), note it but skip it.

If the plan references no files at all, use Glob and Grep to find files relevant to what the plan describes (based on component names, module names, feature areas mentioned) and read those. A plan that touches code but doesn't reference specific files is itself a warning sign — note this for the evaluation.
</step>

<step name="distill_referenced_files">
Before launching the evaluation agents, create lean file summaries for each referenced file read in the previous step. For each code file that was found and read, call `mcp__aid__distill_file` to produce a compact API summary. Collect all the distilled summaries into a single text block called `file_summaries`. For non-code files (markdown, JSON, YAML, configs), include a brief inline summary instead of distilling.

This keeps agent prompts lean — agents receive distilled summaries, not full file content.
</step>

<step name="evaluate">
You are now a senior technical design reviewer. Launch 4 parallel Task subagents — one for each evaluation pass. Each agent independently evaluates the plan against its assigned criteria and posts results to the review server.

### Session setup

After reading the zing doc, check for `session` in its YAML frontmatter. If a `session` value is present, use that as the session ID. If there is no `session` in the frontmatter (or no frontmatter at all), call `create_review()` to get a new session ID, then update the zing doc's frontmatter to include `session: {session_id}`. Save the file after updating.

This session ID will be used by subagents to POST their findings to the review server.

### Preparing the agent prompts

Each agent receives the same shared context block:

```
## Context

### Zing Document
{full content of the zing document}

### Referenced File Summaries
{file_summaries from distill step}
```

Each agent also receives:
- Its specific pass criteria, litmus tests, and warning signs (copied verbatim below)
- The MCP-only mandate: "Use Serena for code exploration, aid for analysis, CodeGraphContext for architecture. Do not use built-in Read/Grep/Glob for code files. Built-in tools are fine for non-code files (markdown, JSON, YAML, configs, .zing docs)."
- The **session ID** and **server port** for posting findings to the review server
- Instructions for how to POST evaluation tables and improvement proposals (see below)

### Launching the agents

Launch all 4 agents in parallel using 4 `Task` tool calls in a single message. Print a status line before launching:

```
Launching 4 evaluation passes in parallel...
```

---

#### Agent 1: Pass 1 — Design Fundamentals

*Focus: Is the architecture sound and appropriately scoped?*

**Criteria to evaluate (assign Strong/Adequate/Weak/Missing with justification):**

**Clarity & Simplicity**
- Can the design be understood without extensive explanation?
- Are there minimal moving parts — only what's necessary to solve the problem?
- Does each component have a clear, well-defined responsibility and interface that can be described in one sentence?

**Fitness for Purpose**
- Does the design solve the actual stated problem, not a hypothetical future one?
- Are trade-offs (consistency vs. availability, latency vs. throughput, complexity vs. flexibility, etc.) explicitly identified and justified?
- Is the design scaled appropriately for the expected load and usage patterns, not over-engineered for 1000x beyond?

**YAGNI — You Aren't Gonna Need It**
- Is every component justified by a current, concrete requirement?
- Are there speculative abstractions, extension points, configuration options, or features built for requirements that don't exist yet? Flag each one.
- Does the design defer decisions where possible rather than pre-deciding based on hypotheticals?
- Apply the cost test: for any piece that isn't driven by a current requirement, would it truly be harder to add later than to carry now?

**Maintainability**
- Is there low coupling and high cohesion? Would a change to one component cascade across the system?
- Is the design evolvable — can key decisions (storage engine, protocol, data model) be revisited without a rewrite?

**Litmus tests to answer:**

1. **"What's the simplest thing that could work?"** — Is this design significantly more complex than necessary? If so, is every bit of additional complexity clearly justified?
2. **"What requirement drives this component?"** — Is there any component that cannot be traced to a concrete, current requirement?

**Warning signs to check:**

- "We might need this someday" justifications
- Only one approach was considered (no alternatives evaluated)
- Components exist "for future flexibility" without a current requirement

**Posting results to the review server:**

First, POST your evaluation tables as a `text` item so they appear as read-only reference in the review UI:

```bash
curl -s -w "\n%{http_code}" -X POST http://localhost:PORT/SESSION_ID/findings \
  -H "Content-Type: application/json" \
  -d '{"type":"text","title":"Pass 1: Design Fundamentals","body":"| Criterion | Rating | Justification |\n|---|---|---|\n| Clarity & Simplicity | {rating} | {justification} |\n| Fitness for Purpose | {rating} | {justification} |\n| YAGNI | {rating} | {justification} |\n| Maintainability | {rating} | {justification} |\n\n| Litmus Test | Result |\n|---|---|\n| Simplest thing that could work? | {result} |\n| What requirement drives each component? | {result} |\n\n| Warning Sign | Found? | Details |\n|---|---|---|\n| \"Might need this someday\" justifications | {Yes/No} | {details} |\n| Only one approach considered | {Yes/No} | {details} |\n| Components for \"future flexibility\" | {Yes/No} | {details} |","context":"Informational — no response needed"}'
```

Then, for each issue found (any criterion rated "Weak" or "Missing", any warning sign marked "Yes"), POST a `choice` item with 2-3 concrete improvement options plus "Skip":

```bash
curl -s -w "\n%{http_code}" -X POST http://localhost:PORT/SESSION_ID/findings \
  -H "Content-Type: application/json" \
  -d '{"type":"choice","question":"Problem: {describe the specific problem found}","options":[{"label":"{approach 1 name}","description":"{concrete edit to make}"},{"label":"{approach 2 name}","description":"{concrete edit to make}"},{"label":"Skip","description":"Not important enough to address now"}]}'
```

Check the HTTP status code on the last line of output:
- **200** = accepted, continue
- **400** = fix your JSON and retry
- **404** = verify the session ID is correct. If it is correct, abort immediately with a `FATAL: session not found` error message (do not post more findings or signal agent-complete)

After posting ALL items, signal completion:

```bash
curl -s -X POST http://localhost:PORT/SESSION_ID/agent-complete
```

---

#### Agent 2: Pass 2 — Robustness & Safety

*Focus: Is the system safe, testable, and operable?*

**Criteria to evaluate (assign Strong/Adequate/Weak/Missing with justification):**

**Correctness & Safety**
- Is data integrity protected? Are consistency guarantees, transactional boundaries, and data loss scenarios explicit?
- Is security structural (AuthN/AuthZ, input validation, trust boundaries as part of the architecture) rather than bolted on?
- Are edge cases addressed — race conditions, redelivery, ordering, idempotency?

**Operability**
- Is the system observable at runtime (logging, metrics, tracing)?
- Are failure modes identified? Does the design answer "what happens when X fails?" for every dependency and component?
- Can changes be deployed incrementally and rolled back if needed?

**TDD — Test-Driven Development Readiness**
- Does the design express clear, testable behaviors — not just implementation details?
- Are the interfaces and boundaries defined in a way that makes unit, integration, and end-to-end testing straightforward without heroic setup?
- Is there a test strategy? Do tests serve as living documentation of expected behavior?
- Would TDD naturally produce this design, or does the design contain elements that would be difficult to test or that no test would ever drive?

**Litmus tests to answer:**

3. **"What happens when this fails?"** — Are failure scenarios documented, or is only the happy path described?
4. **"How will we know it's working?"** — Is there a plan for observability and success metrics?
5. **"How do we test this?"** — Can components be tested in isolation, or does testing require spinning up the entire system?

**Warning signs to check:**

- Only the happy path is described
- Data model is an afterthought
- Deployment/migration strategy is deferred ("we'll figure it out later")
- No tests or test strategy described

**Posting results to the review server:**

First, POST your evaluation tables as a `text` item so they appear as read-only reference in the review UI:

```bash
curl -s -w "\n%{http_code}" -X POST http://localhost:PORT/SESSION_ID/findings \
  -H "Content-Type: application/json" \
  -d '{"type":"text","title":"Pass 2: Robustness & Safety","body":"| Criterion | Rating | Justification |\n|---|---|---|\n| Correctness & Safety | {rating} | {justification} |\n| Operability | {rating} | {justification} |\n| TDD Readiness | {rating} | {justification} |\n\n| Litmus Test | Result |\n|---|---|\n| What happens when this fails? | {result} |\n| How will we know it'\''s working? | {result} |\n| How do we test this? | {result} |\n\n| Warning Sign | Found? | Details |\n|---|---|---|\n| Only happy path described | {Yes/No} | {details} |\n| Data model is afterthought | {Yes/No} | {details} |\n| Deployment strategy deferred | {Yes/No} | {details} |\n| No test strategy | {Yes/No} | {details} |","context":"Informational — no response needed"}'
```

Then, for each issue found (any criterion rated "Weak" or "Missing", any warning sign marked "Yes"), POST a `choice` item with 2-3 concrete improvement options plus "Skip":

```bash
curl -s -w "\n%{http_code}" -X POST http://localhost:PORT/SESSION_ID/findings \
  -H "Content-Type: application/json" \
  -d '{"type":"choice","question":"Problem: {describe the specific problem found}","options":[{"label":"{approach 1 name}","description":"{concrete edit to make}"},{"label":"{approach 2 name}","description":"{concrete edit to make}"},{"label":"Skip","description":"Not important enough to address now"}]}'
```

Check the HTTP status code on the last line of output:
- **200** = accepted, continue
- **400** = fix your JSON and retry
- **404** = verify the session ID is correct. If it is correct, abort immediately with a `FATAL: session not found` error message (do not post more findings or signal agent-complete)

After posting ALL items, signal completion:

```bash
curl -s -X POST http://localhost:PORT/SESSION_ID/agent-complete
```

---

#### Agent 3: Pass 3 — Plan as Executable Spec

*Focus: Could an agent pick up this plan and build exactly the right thing with zero guesswork?*

**Criteria to evaluate (assign Strong/Adequate/Weak/Missing with justification):**

**Specificity & Executability**
- Could an agent (or a developer unfamiliar with the codebase) read this plan and build exactly what's intended with zero guesswork? If any step requires the implementer to "figure out" an approach, the plan is underspecified.
- Are there vague or weasel words? Flag every instance of: "appropriate", "as needed", "etc.", "similar", "relevant", "proper", "suitable", "some kind of", "various", "handle accordingly", "if necessary". Each one is an ambiguity the implementer must resolve — the plan should resolve it instead.
- Is the tech stack fully specified? Are specific libraries, frameworks, and tools named — not just categories? (e.g., "use bcrypt for password hashing" not "use a hashing library")
- Are data models and schemas concretely defined with actual field names, types, and relationships — or just described in prose?
- Are API contracts specified? Endpoints, HTTP methods, request/response shapes, status codes — or just "create an API for X"?
- Are file paths specified? Does the plan say exactly which files to create or modify, or does it leave file organization to the implementer?
- Could two different developers read this plan and build substantially different things? If yes, the plan is ambiguous.
- Are there any "TBD", "TODO", or "decide later" markers? Each one is a gap in the spec.
- For each action step: is it clear what "done" looks like? Not just what to do, but what the concrete output is?

**Step Atomicity**
- Is each action step atomic and self-contained? A step is atomic if it can be implemented, tested, and verified independently — without requiring other unfinished steps to be completed first.
- Do implementation and tests live in the same step? If step 3 says "implement feature X" and step 7 says "write tests for feature X", that's wrong — the tests belong in step 3. Every step that writes code must include its tests.
- Are there dependency chains where multiple steps must all be completed before any of them can be verified? This is a sign those steps should be combined into one. For example, if steps 4, 5, and 6 all build parts of a system and none can be tested until all three are done, they should be a single step.
- Can each step produce a working, testable increment? After completing any step, the system should be in a valid state — not a broken intermediate state that only becomes valid after later steps.
- Are there "scaffolding" steps that create structure without behavior (e.g., "create empty files", "set up folder structure", "add placeholder interfaces")? These should be merged into the steps that actually implement the behavior.

**Litmus tests to answer:**

6. **"Could two people build different things from this plan?"** — If you gave this plan to two developers independently, would they produce substantially the same system? If not, where does the plan diverge into ambiguity?
7. **"Can each step be completed, tested, and committed independently?"** — After finishing any single step, is the system in a valid, working state? Or are there steps that leave the system broken until later steps are also completed?

**Warning signs to check:**

- Implementation details dominate over interface definitions
- Steps missing acceptance criteria — each step in the action plan should have clear, specific criteria so Claude (or any agent) knows when the step is done. Flag any step that says what to do but not how to verify it's complete.
- Vague or weasel words — the plan uses "appropriate", "as needed", "etc.", "similar", "relevant", "proper", "suitable", "various", "handle accordingly", or "if necessary" instead of making concrete decisions. Flag every instance.
- Unspecified tech choices — the plan describes categories ("a database", "some caching layer", "a hashing library") instead of naming specific technologies, libraries, or approaches.
- Missing data models — the plan describes features that involve data but doesn't define schemas, field names, types, or relationships.
- TBD/TODO markers — the plan contains explicit "TBD", "TODO", "decide later", or "out of scope for now" markers for decisions that would block implementation.
- Tests separated from implementation — tests for a feature are in a different step than the implementation of that feature, rather than being part of the same atomic step.
- Steps that can't be verified independently — multiple steps must all be completed before any of them can be tested or verified, indicating they should be combined into a single step.
- Scaffolding steps without behavior — steps that only create empty files, folder structures, or placeholder interfaces with no testable behavior, instead of being merged into the step that implements the actual behavior.

**Posting results to the review server:**

First, POST your evaluation tables as a `text` item so they appear as read-only reference in the review UI:

```bash
curl -s -w "\n%{http_code}" -X POST http://localhost:PORT/SESSION_ID/findings \
  -H "Content-Type: application/json" \
  -d '{"type":"text","title":"Pass 3: Plan as Executable Spec","body":"| Criterion | Rating | Justification |\n|---|---|---|\n| Specificity & Executability | {rating} | {justification} |\n| Step Atomicity | {rating} | {justification} |\n\n| Litmus Test | Result |\n|---|---|\n| Could two people build different things from this plan? | {result} |\n| Can each step be completed, tested, and committed independently? | {result} |\n\n| Warning Sign | Found? | Details |\n|---|---|---|\n| Implementation over interfaces | {Yes/No} | {details} |\n| Steps missing acceptance criteria | {Yes/No} | {details} |\n| Vague or weasel words | {Yes/No} | {details} |\n| Unspecified tech choices | {Yes/No} | {details} |\n| Missing data models | {Yes/No} | {details} |\n| TBD/TODO markers | {Yes/No} | {details} |\n| Tests separated from implementation | {Yes/No} | {details} |\n| Steps that can'\''t be verified independently | {Yes/No} | {details} |\n| Scaffolding steps without behavior | {Yes/No} | {details} |","context":"Informational — no response needed"}'
```

Then, for each issue found (any criterion rated "Weak" or "Missing", any warning sign marked "Yes"), POST a `choice` item with 2-3 concrete improvement options plus "Skip":

```bash
curl -s -w "\n%{http_code}" -X POST http://localhost:PORT/SESSION_ID/findings \
  -H "Content-Type: application/json" \
  -d '{"type":"choice","question":"Problem: {describe the specific problem found}","options":[{"label":"{approach 1 name}","description":"{concrete edit to make}"},{"label":"{approach 2 name}","description":"{concrete edit to make}"},{"label":"Skip","description":"Not important enough to address now"}]}'
```

Check the HTTP status code on the last line of output:
- **200** = accepted, continue
- **400** = fix your JSON and retry
- **404** = verify the session ID is correct. If it is correct, abort immediately with a `FATAL: session not found` error message (do not post more findings or signal agent-complete)

After posting ALL items, signal completion:

```bash
curl -s -X POST http://localhost:PORT/SESSION_ID/agent-complete
```

---

#### Agent 4: Pass 4 — Code Quality

*Focus: Is the proposed code well-structured and idiomatic?*

**Criteria to evaluate (assign Strong/Adequate/Weak/Missing with justification):**

**Code Quality & Idiomacy**
- Is the code at the right abstraction level? Is it modular enough — not too granular, not too monolithic?
- Can a better solution be found in terms of maintainability, readability, performance, or security?
- Are there best practices, design patterns, or language-specific patterns that could substantially improve the code?
- Is the code reasonably understandable by someone with little prior experience? Are any esoteric language features being used unnecessarily?
- Can readability be improved by smaller methods, better names, or restructured control flow?
- Is the code in the right file/folder/package? Does the project structure make sense?
- Is the code idiomatic to the language it's written in?
- Are any new patterns introduced? If so, are they good patterns worth adopting, or unnecessary divergence from established conventions?
- Can this solution be simplified without losing functionality?

**Posting results to the review server:**

First, POST your evaluation table as a `text` item so it appears as read-only reference in the review UI:

```bash
curl -s -w "\n%{http_code}" -X POST http://localhost:PORT/SESSION_ID/findings \
  -H "Content-Type: application/json" \
  -d '{"type":"text","title":"Pass 4: Code Quality","body":"| Criterion | Rating | Justification |\n|---|---|---|\n| Code Quality & Idiomacy | {rating} | {justification} |","context":"Informational — no response needed"}'
```

Then, for each issue found (criterion rated "Weak" or "Missing"), POST a `choice` item with 2-3 concrete improvement options plus "Skip":

```bash
curl -s -w "\n%{http_code}" -X POST http://localhost:PORT/SESSION_ID/findings \
  -H "Content-Type: application/json" \
  -d '{"type":"choice","question":"Problem: {describe the specific problem found}","options":[{"label":"{approach 1 name}","description":"{concrete edit to make}"},{"label":"{approach 2 name}","description":"{concrete edit to make}"},{"label":"Skip","description":"Not important enough to address now"}]}'
```

Check the HTTP status code on the last line of output:
- **200** = accepted, continue
- **400** = fix your JSON and retry
- **404** = verify the session ID is correct. If it is correct, abort immediately with a `FATAL: session not found` error message (do not post more findings or signal agent-complete)

After posting ALL items, signal completion:

```bash
curl -s -X POST http://localhost:PORT/SESSION_ID/agent-complete
```

---

### Collecting results

After all 4 Task agents return, check each agent's output for a `FATAL:` prefix. If any agent returned a fatal error, report the error to the user and abort.

Otherwise, proceed to the `present_summary` step. The evaluation tables and improvement proposals have already been posted to the review server by the agents — they will appear in the review UI alongside the actionable choice items.

</step>

<step name="present_summary">
After all four passes are complete, present a consolidated summary:

### Summary Verdict

State one of: **Strong Design**, **Adequate with Gaps**, **Needs Significant Rework**

Provide a 1-2 sentence summary justification.

### Top Strengths

List all notable strengths. If there are genuinely none, say "None".

### Top Risks / Weaknesses

List all notable risks and weaknesses. If there are genuinely none, say "None".

### Recommendations

List specific, actionable changes to improve the design, ordered by priority. If the design is strong and no changes are needed, say "None — design is sound."
</step>

<step name="propose_improvements">
If the verdict is **Strong Design**, skip this step and say "No improvements needed — the design looks solid."

Otherwise, call `wait_for_review(session_id)`. This opens the review UI in the browser where the user can see all evaluation tables (as read-only reference) and improvement proposals (as radio-button choices) posted by the 4 agents. The user picks their preferred approach for each improvement — or selects "Skip" — and submits all decisions at once.

When `wait_for_review` returns, iterate over the returned items. Each item contains the original problem description, the options, and the user's selected option. For each choice the user made (excluding "Skip"):
- Apply the corresponding edit to the zing file using the Edit tool
- The option's `description` field contains the concrete edit to make

After all improvements have been applied, summarize what was changed.
</step>

<step name="ensure_progress_section">
After improvements are complete, check if the document has a `## Progress` section with a checklist.

If it does NOT have one, generate a Progress section from the document's action plan, steps, or key deliverables. Use the Edit tool to append it to the end of the file. Use this format:

```
## Progress

- [ ] Step 1: {description}
- [ ] Step 2: {description}
- [ ] Step 3: {description}
...
```

Derive the steps from whatever actionable items exist in the document — action plan steps, phases, tasks, deliverables, or features. All items start unchecked.

If the document already has a `## Progress` section, skip this step.

Tell the user whether you added a Progress section or if one already existed.

End your summary with: "Zing! Audit complete."

Then use AskUserQuestion to ask the user what they'd like to do next:
- Option 1: "Start build" — invoke `Skill(skill: 'zing:build', args: '{file_path}')` where `{file_path}` is the path to the zing plan file that was audited
- Option 2: "Create Linear tickets" — invoke `Skill(skill: 'zing:plan-linear', args: '{file_path}')` where `{file_path}` is the path to the zing plan file that was audited
- Option 3: "View the plan" — open the file for viewing (run `open -a Typora "{file_path}"`, falling back to `open "{file_path}"` if Typora is not installed), then re-ask this same question
- Option 4: "Not now" — end the session without invoking anything
</step>

</process>

<anti_patterns>
- Don't skip passes or criteria — complete all four passes even if some seem less relevant
- Don't combine passes — complete each pass and present its results before starting the next
- Don't inflate ratings — be honest about gaps
- Don't bypass the review UI for improvements — let the user pick approaches in the browser
- Don't ask questions about findings rated "Strong" — focus on gaps
- Don't make assumptions about the user's domain — ask if context is unclear
- Don't make vague suggestions — every proposed improvement should be a specific, concrete edit
</anti_patterns>

<success_criteria>
Evaluation is complete when:

- [ ] Document was read and understood
- [ ] All files referenced in the plan were read (or noted as not yet existing)
- [ ] Pass 1 (Design Fundamentals): 4 criteria rated, 2 litmus tests answered, 3 warning signs checked, results posted to review server
- [ ] Pass 2 (Robustness & Safety): 3 criteria rated, 3 litmus tests answered, 4 warning signs checked, results posted to review server
- [ ] Pass 3 (Plan as Executable Spec): 2 criteria rated, 2 litmus tests answered, 9 warning signs checked, results posted to review server
- [ ] Pass 4 (Code Quality): 1 criterion rated, results posted to review server
- [ ] Summary verdict, strengths, risks, and recommendations presented
- [ ] Improvements reviewed in UI and applied (or skipped if design is strong)
- [ ] Progress section ensured — added if missing, skipped if already present
</success_criteria>
