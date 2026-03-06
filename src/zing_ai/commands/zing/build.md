
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

<step name="create_log_file">
Derive the log file path from the zing file path by replacing `.md` with `.log` (e.g., `.zing/recipe-app.md` → `.zing/recipe-app.log`). Create the log file (or truncate it if it already exists) using Write with empty content. This file is shared across all steps in the build and must exist before any subagent tries to append to it.
</step>

<step name="execute_step">
This is the core execution loop. The parent agent owns the step loop but delegates each step's execution to a Task subagent for context isolation. For each incomplete step, in order:

1. **Mark the task as in_progress** using TaskUpdate.

2. **Tell the user:**
   ```
   --- Step {N}: {description} ---
   Starting Step {N}. To follow along: `tail -f {log_file_path}`
   ```

3. **Construct a self-contained prompt** for the Task subagent that includes ALL of the following (use the log file path from the `create_log_file` step):
   - The zing overview (everything in the zing file from the start up to but not including the `## Action Plan` section)
   - The specific step instructions (copied verbatim from the Action Plan)
   - The acceptance criteria for this step
   - The list of relevant files for this step (from the Relevant Files section)
   - Anti-patterns: "Do not deviate from the step instructions. Do not add features, refactor, or improve code beyond what the step says. Do not skip acceptance criteria verification. Do not reinterpret the step. Do NOT run any git commands — the parent agent handles all git operations."
   - Log file instructions: "Append progress updates to `{log_file_path}` as you work. Log what you are doing — which files you're reading, what edits you're making, what tests you're running, any issues you encounter, and the final result. The user may be watching this file in real-time with `tail -f`."
   - MCP-only code reading mandate: "Use Serena for code exploration, aid for analysis, CodeGraphContext for architecture. Do not use built-in Read/Grep/Glob for code files."
   - Storybook instructions: "If this step involves creating or modifying Storybook stories (*.stories.*), call the `mcp__storybook-mcp__get-storybook-story-instructions` tool BEFORE writing any story code to get the correct patterns and imports. After writing stories, use `mcp__storybook-mcp__preview-stories` to verify they render correctly."

4. **Launch the Task subagent** using the `Task` tool with `subagent_type: "codegen"` and the constructed prompt. The subagent executes the step, logs its progress to the log file, verifies acceptance criteria, and returns a summary of what was done.

5. **After the subagent returns**, the parent:
   - **Commits the step's changes to git:** Run `git status` to check for uncommitted changes. If there are changes, stage the specific changed files (NEVER use `git add -A` or `git add .`) and commit with message `Step {N}: {short description}`. Do NOT push to remote.
   - Updates the Progress section in the zing file (`- [ ]` → `- [x]`) using Edit
   - Marks the task as completed using TaskUpdate

6. **Move to the next incomplete step immediately** and repeat from point 1. Do NOT prompt the user between steps — always continue automatically.
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
