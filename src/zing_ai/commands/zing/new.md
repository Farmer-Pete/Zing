
<objective>
Two modes:

1. **Ticket mode**: If the user provides a Linear ticket identifier (e.g. `BAK-1179`) or a full Linear ticket URL (e.g. `https://linear.app/turngate/issue/BAK-1179/...`), fetch the ticket via Linear MCP and write its title and description straight into a zing file — no list browsing, no confirmation.
2. **Conversation mode**: Collect zing information from the user through a free-form conversation. Act as a listener — do not try to understand, interpret, analyze, or ask clarifying questions about the zing spec. Simply collect what the user tells you. When the user says DONE, write everything to a markdown file in the `.zing/` directory.
</objective>

<process>

<step name="setup">
Create the `.zing/` directory in the current working directory if it doesn't already exist.

Then check if a `.gitignore` file exists in the current working directory. If it does, read it and check whether `.zing` or `.zing/` is already listed. If not, append `.zing/` to the end of the file. If no `.gitignore` exists, create one containing `.zing/`.

After the directory and gitignore are set up, create a session by calling the `mcp__zing-ai__session_create` MCP tool. Pass a `title` derived from the user's input if available (e.g., the project name or a short summary), otherwise use `"New Zing Session"` as the default title. The tool returns a response containing `session_id` and `steps` (a mapping of step names to step IDs, e.g. `{plan: id, plan-audit: id, build: id, build-audit: id}`). Store these values — they will be embedded in the zing file later.
</step>

<step name="detect_mode">
Check `$ARGUMENTS`:

- If `$ARGUMENTS` matches the pattern `^[A-Z]+-\d+$` (a bare ticket identifier, e.g. `BAK-1179`), use it as the identifier and proceed to the **ticket_flow** step.
- If `$ARGUMENTS` matches the pattern `https://linear\.app/.+/issue/([A-Z]+-\d+)/.+` (a full Linear ticket URL), extract the captured identifier from the URL path (e.g. `BAK-1179`) and proceed to the **ticket_flow** step.
- Otherwise, proceed to the **greet** step for conversation mode.
</step>

<step name="ticket_flow">
The user has provided a Linear ticket identifier. Fetch it and write a zing file directly — no confirmation needed.

1. **Fetch the ticket**: Call `mcp__claude_ai_Linear__get_issue` with the extracted identifier (e.g. `BAK-1179`).

2. **Generate a filename**: Derive a slug from the identifier and title (e.g., `BAK-1179-fix-auth-bug.md`) using kebab-case.

3. **Write the zing file**: Write a markdown file to `.zing/<slug>.md`. The file MUST begin with YAML frontmatter containing the session ID, step IDs, ticket_id, and complexity:

```markdown
---
session: {session_id}
ticket_id: {identifier}
complexity: medium
steps:
  plan: {plan_step_id}
  plan-audit: {plan-audit_step_id}
  build: {build_step_id}
  build-audit: {build-audit_step_id}
---
# {identifier}: {title}

{Full ticket description body, preserved as-is}
```

Include only what's in the ticket — do not add your own analysis or suggestions.

4. **Update the session**: Call `mcp__zing-ai__session_update` with:
   - `session_id`: the session ID from `session_create`
   - `zing_file`: the absolute path to the zing file just written
   - `ticket_id`: the ticket identifier (e.g. `BAK-1179`)
   - `title`: the ticket title

5. Proceed to the **confirm** step.
</step>

<step name="greet">
Say exactly:

---

**New Zing**

Tell me about what you'd like to build. Share anything you want captured — goals, features, constraints, tech stack, architecture, user stories, notes, whatever is on your mind.

I'll listen and collect everything. When you're ready, say **DONE** and I'll write it all to a zing file.

If you have questions or want me to research something, just ask — I'll look it up and ask whether to include it.

---
</step>

<step name="conversation_loop">
This is the core loop. Repeat until the user says DONE:

**If the user provides zing information:**
- Respond with a positive emoji (e.g. 👍, ✅, 📝) followed by a short acknowledgment like "Got it" or "Noted", then remind them: "Keep going, or say **DONE** to create the zing file."
- Do NOT ask follow-up questions
- Do NOT suggest improvements or alternatives
- Do NOT try to organize or restructure what they said
- Do NOT ask "anything else?" or "what about X?"
- Just confirm receipt and wait

**If the user asks a question or requests research:**
- Perform the research using WebSearch, WebFetch, Read, Glob, Grep, or Bash as appropriate
- Present the findings clearly
- Then ask: "Should I include this in the zing file?"
- If yes, note it for inclusion. If no, move on.

**If the user says DONE (case-insensitive):**

- Proceed to the next step
</step>

<step name="generate_filename">
Generate a short, descriptive kebab-case filename based on the zing information collected. For example:
- `recipe-app.md`
- `cli-dashboard-tool.md`
- `inventory-management-system.md`

The filename should be descriptive enough to identify the zing spec at a glance.
</step>

<step name="save_zing_file">
Create the `.zing/` directory if it doesn't exist, then write a markdown file there.

The file MUST begin with YAML frontmatter containing the session ID and step IDs from the `session_create` call:

```markdown
---
session: {session_id}
steps:
  plan: {plan_step_id}
  plan-audit: {plan-audit_step_id}
  build: {build_step_id}
  build-audit: {build-audit_step_id}
---
# {Project Name}

## Overview
{High-level description of the project}

## Goals
{What the project aims to achieve}

## Features
{Features, capabilities, user stories}

## Technical Details
{Tech stack, architecture, constraints, integrations}

## Notes
{Anything else the user mentioned that doesn't fit above}

## Research
{Any research results the user chose to include}
```

The file should contain ALL information the user provided, organized into logical sections. Use the structure above as a guide, but adapt based on what was actually provided — only include sections where the user gave relevant information.

Rules for the file:
- The YAML frontmatter with session and steps is REQUIRED — always include it
- Include EVERYTHING the user said — do not omit, summarize, or editorialize
- Preserve the user's original wording as much as possible
- If something doesn't fit a section, put it in Notes
- Do not add your own suggestions, recommendations, or analysis
- Do not add sections the user didn't provide information for

After writing the file, call `mcp__zing-ai__session_update` with:
- `session_id`: the session ID from `session_create`
- `zing_file`: the absolute path to the zing file just written
- `title`: the project name or a short summary derived from the zing content
</step>

<step name="confirm">
After saving, tell the user:

```
Saved to .zing/{filename}
```

Where `{filename}` is the path relative to the current working directory (e.g., `.zing/recipe-app.md`).

Before chaining to the next skill, print an excited sentence containing "Zing!" with a lightning bolt-related emoji (e.g. ⚡). Vary the sentence each time — don't repeat the same one.

Resolve the zing file path to an absolute path (using the current working directory) before passing it as the skill argument.

Then invoke the `Skill` tool with skill name `zing:plan` and args set to the file path (e.g., `.zing/recipe-app.md`) to continue the pipeline.
</step>

</process>

<anti_patterns>
- Do NOT ask clarifying questions about the zing spec — just listen
- Do NOT suggest features, improvements, or alternatives
- Do NOT reorganize or reword what the user says while collecting
- Do NOT prompt the user with "what about testing?" or "have you considered X?"
- Do NOT add your own analysis or recommendations to the saved file
- Do NOT ask "anything else?" — wait silently for the user to continue or say DONE
</anti_patterns>
