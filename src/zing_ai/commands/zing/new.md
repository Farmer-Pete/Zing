
<objective>
Two modes:

1. **Linear mode**: If the user provides a Linear filter URL (`https://linear.app/...`), use the Linear MCP to fetch tickets from that filter, find a good one to work on, and save it as a zing file.
2. **Conversation mode**: Collect zing information from the user through a free-form conversation. Act as a listener — do not try to understand, interpret, analyze, or ask clarifying questions about the zing spec. Simply collect what the user tells you. When the user says SAVE or DONE, write everything to a markdown file in the `.zing/` directory.
</objective>

<process>

<step name="setup">
Create the `.zing/` directory in the current working directory if it doesn't already exist.

Then check if a `.gitignore` file exists in the current working directory. If it does, read it and check whether `.zing` or `.zing/` is already listed. If not, append `.zing/` to the end of the file. If no `.gitignore` exists, create one containing `.zing/`.

After the directory and gitignore are set up, create a session by calling the `mcp__zing-ai__session_create` MCP tool. Pass a `title` derived from the user's input if available (e.g., the project name or a short summary), otherwise use `"New Zing Session"` as the default title. The tool returns a response containing `session_id` and `steps` (a mapping of step names to step IDs, e.g. `{plan: id, plan-audit: id, build: id, build-audit: id}`). Store these values — they will be embedded in the zing file later.
</step>

<step name="detect_mode">
Check the user's input. If they provided a Linear URL (matching `https://linear.app/...`), proceed to the **linear_flow** step. Otherwise, proceed to the **greet** step for conversation mode.
</step>

<step name="linear_flow">
The user has provided a Linear filter/view URL. The goal is to find a ticket worth working on.

1. **Parse the URL**: Extract useful information from the Linear URL. The URL typically encodes filters like team, status, assignee, label, etc. Use `WebFetch` on the URL to understand what filter/view it represents if needed. Look for query parameters or path segments that indicate team key, filter criteria, etc.

2. **Get current user**: Call `mcp__claude_ai_Linear__get_user` with `query: "me"` to get your user ID and name. You'll need this to filter for unassigned or assigned-to-me tickets.

3. **Fetch tickets**: Use `mcp__claude_ai_Linear__list_issues` to fetch issues. Apply filters based on what you extracted from the URL (team, label, project, state, etc.). Order by priority — fetch urgent (1) first, then high (2), then normal (3).

4. **Filter for available tickets**: From the results, only consider tickets that are either:
   - Unassigned (no assignee), OR
   - Assigned to the current user ("me")

   Skip tickets assigned to other people.

5. **Find a good ticket**: Go through the tickets in priority order. For each ticket, call `mcp__claude_ai_Linear__get_issue` to get the full details. Look for a ticket that has a **sufficient level of detail** — meaning it has a description with enough context to actually start working on it (not just a title with no body, or a vague one-liner). A good ticket has:
   - A clear description of what needs to be done
   - Enough context to understand the problem or feature
   - Ideally some acceptance criteria or expected behavior

6. **Propose the ticket**: Once you find a suitable ticket, present it to the user. Show:
   - The ticket identifier (e.g., `ENG-123`)
   - The title
   - The priority
   - A brief summary of what it's about

   If a session has already been created at this point, send a browser notification so they know input is needed:
   Call `notification_send(session_id, title="Ticket found", body="A suitable Linear ticket was found. Confirm or skip.")` where `session_id` is the session ID from the `session_create` call.

   Then use `AskUserQuestion` to ask:
   - Question: "Want to work on this ticket?"
   - Options:
     - "Yes, let's do it" (description: "Create a zing file from this ticket")
     - "Skip, show me the next one" (description: "Look for another ticket")
     - "No, let me describe a zing spec instead" (description: "Switch to conversation mode")

   If the user says skip, move to the next suitable ticket and repeat. If there are no more suitable tickets, tell the user and switch to conversation mode. If the user says yes, proceed to save.

7. **Save the ticket as a zing file**: Write a markdown file to `.zing/` using the ticket identifier and title as the filename (e.g., `.zing/ENG-123-fix-auth-bug.md`). The file MUST begin with YAML frontmatter containing the session ID and step IDs from the `session_create` call:

```markdown
---
session: {session_id}
steps:
  plan: {plan_step_id}
  plan-audit: {plan-audit_step_id}
  build: {build_step_id}
  build-audit: {build-audit_step_id}
---
# {Ticket Identifier}: {Title}

Linear ticket: {ticket URL or identifier}

## Description
{Full ticket description body, preserved as-is}
```

Include only what's in the ticket — do not add your own analysis or suggestions. Then proceed to the **confirm** step.
</step>

<step name="greet">
Say exactly:

---

**New Zing**

Tell me about what you'd like to build. Share anything you want captured — goals, features, constraints, tech stack, architecture, user stories, notes, whatever is on your mind.

I'll listen and collect everything. When you're ready, say **SAVE** or **DONE** and I'll write it all to a zing file.

If you have questions or want me to research something, just ask — I'll look it up and ask whether to include it.

---
</step>

<step name="conversation_loop">
This is the core loop. Repeat until the user says SAVE or DONE:

**If the user provides zing information:**
- Respond with a positive emoji (e.g. 👍, ✅, 📝) followed by a short acknowledgment like "Got it" or "Noted", then remind them: "Keep going, or say **SAVE** / **DONE** to create the zing file."
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

**If the user says SAVE or DONE (case-insensitive, can be part of a larger message):**
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
- Do NOT propose tickets assigned to other people — only unassigned or assigned to the current user
- Do NOT propose tickets that lack sufficient detail to actually work on
</anti_patterns>
