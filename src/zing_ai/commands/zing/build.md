
<objective>
Execute a zing plan by working through its action plan steps one at a time. Follow the steps exactly as written. After completing each step, verify it against its acceptance criteria, commit the changes, and update the progress tracker. If an issue is encountered, propose solutions and let the user decide.
</objective>

<process>

<step name="parse_arguments">
Parse the command arguments:
- The argument is a file path to the zing markdown file
- Example: `/zing:build .zing/recipe-app.md` -> filename = ".zing/recipe-app.md"

If no arguments provided:
- Use Glob to list all markdown files in the `.zing` directory
- If no files found, show an error and exit:
  ```
  No zing files found in .zing/
  Run /zing:new to create one.
  ```
- If one or more files found, use AskUserQuestion to let the user pick which zing spec to build
- Use the chosen file as the argument going forward
</step>

<step name="read_zing">
Read the zing file using the Read tool. Parse and understand:

1. **The overall zing spec** — what is being built, the goals, tech stack, constraints
2. **The Action Plan** — the numbered steps grouped into phases
3. **The Progress section** — which steps are already marked as complete (`- [x]`) vs incomplete (`- [ ]`)
4. **Relevant Files** — files to modify, create, reference, and test

If the zing file has no Action Plan section, show an error and exit:
```
ERROR: No Action Plan found in {filename}
Run /zing:plan {filename} to create an action plan.
```

If the zing file has no Progress section, show an error and exit:
```
ERROR: No Progress section found in {filename}
Run /zing:plan-audit {filename} to add one.
```

### Session setup

After reading the zing doc, parse its YAML frontmatter. Extract the `session` value (session ID) and the `steps` mapping (which maps step names like `plan`, `plan-audit`, `build`, `build-audit` to their step IDs).

If there is no `session` in the frontmatter (or no frontmatter at all), this is a standalone invocation. Call `session_create(title)` to get a new session ID and step IDs, then update the zing doc's frontmatter to include `session: {session_id}` and `steps:` with the returned step ID mapping. Save the file after updating.

Once you have the session ID and step IDs, resolve the zing file path to an absolute path and call `session_update(session_id, zing_file=abs_path, title=doc_title)` to associate the zing file with the session.

Then call `step_start(session_id, steps.build)` where `steps.build` is the build step ID from the frontmatter. This transitions the build step from PENDING to STARTED.

The session ID and build step ID will be used for agent lifecycle tracking and logging throughout the build.
</step>

<step name="create_tasklist">
Create a task for every step in the Action Plan using TaskCreate. Use the step number and description as the subject, and include the full step details (instructions, files involved, acceptance criteria) in the description.

Then check the Progress section. For any step marked `- [x]` (complete), immediately mark the corresponding task as completed using TaskUpdate.

Show the user a summary:
```
Zing: {zing name}
Total steps: {N}
Completed: {X}
Remaining: {N - X}
Next step: Step {number}: {description}
```
</step>

<step name="execute_step">
This is the core execution loop. The parent agent owns the step loop but delegates each step's execution to a Task subagent for context isolation. For each incomplete step, in order:

1. **Mark the task as in_progress** using TaskUpdate.

2. **Tell the user:**
   ```
   --- Step {N}: {description} ---
   Starting Step {N}. Progress is visible in the Zing dashboard.
   ```

3. **Register the subagent** by calling `agent_start(session_id, step_id, name="Step {N}: {description}", description="Executing step {N} of the build plan")` where `step_id` is `steps.build` from the frontmatter.

4. **Construct a self-contained prompt** for the Task subagent that includes ALL of the following:
   - The session ID and build step ID (from the frontmatter `session` and `steps.build` values)
   - The agent name used in the `agent_start` call (e.g. `"Step {N}: {description}"`)
   - The zing overview (everything in the zing file from the start up to but not including the `## Action Plan` section)
   - The specific step instructions (copied verbatim from the Action Plan)
   - The acceptance criteria for this step
   - The list of relevant files for this step (from the Relevant Files section)
   - Anti-patterns: "Do not deviate from the step instructions. Do not add features, refactor, or improve code beyond what the step says. Do not skip acceptance criteria verification. Do not reinterpret the step. Do NOT run any git commands — the parent agent handles all git operations."
   - Logging instructions: "Use `step_log(session_id, step_id, agent_name, message)` to log progress as you work. Log what you are doing — which files you're reading, what edits you're making, what tests you're running, any issues you encounter, and the final result. These logs stream to the Zing dashboard in real-time."
   - MCP-only code reading mandate: "Use Serena for code exploration, aid for analysis, CodeGraphContext for architecture. Do not use built-in Read/Grep/Glob for code files."
   - Storybook instructions: "If this step involves creating or modifying Storybook stories (*.stories.*), call the `mcp__storybook-mcp__get-storybook-story-instructions` tool BEFORE writing any story code to get the correct patterns and imports. After writing stories, use `mcp__storybook-mcp__preview-stories` to verify they render correctly."

5. **Launch the Task subagent** using the `Task` tool with `subagent_type: "general-purpose"` and the constructed prompt. The subagent executes the step, logs progress via `step_log`, verifies acceptance criteria, and returns a summary of what was done.

6. **After the subagent returns**, the parent:
   - Calls `agent_stop(session_id, step_id, name)` where `name` is the same agent name used in `agent_start` (e.g. `"Step {N}: {description}"`)
   - **Commits the step's changes to git:** Run `git status` to check for uncommitted changes. If there are changes, stage the specific changed files (NEVER use `git add -A` or `git add .`) and commit with message `Step {N}: {short description}`. Do NOT push to remote.
   - Updates the Progress section in the zing file (`- [ ]` → `- [x]`) using Edit
   - Marks the task as completed using TaskUpdate

7. **Move to the next incomplete step immediately** and repeat from point 1. Do NOT prompt the user between steps — always continue automatically.
</step>

<step name="completion">
When all steps are complete, tell the user:

```
All steps complete.
```

End your summary with: "Zing! Build complete — handing off to audit."

Then unconditionally invoke the audit skill:

```
Skill(skill: 'zing:build-audit')
```

No file path argument is needed — build-audit uses git diff.
</step>

</process>

<anti_patterns>
- NEVER deviate from the action plan steps — follow them exactly as written
- NEVER add features, refactor, or "improve" code beyond what a step specifies
- NEVER skip acceptance criteria verification — always check before moving on
- NEVER continue past an issue without asking the user — stop and propose solutions
- NEVER use `git add -A` or `git add .` — stage specific files only
- NEVER push to remote — only commit locally
- NEVER mark a step complete if acceptance criteria are not met
- NEVER work on steps out of order unless a step is explicitly marked as independent
- NEVER combine multiple steps into one commit — one commit per step
</anti_patterns>
