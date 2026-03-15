
<objective>
Take a zing plan and create Linear tickets from it. If the plan has more than one phase, create a Linear project to group them. Create one ticket per phase containing all the steps and details for that phase. Attach the full zing plan as a document to the project (if one was created).
</objective>

<process>

<step name="parse_arguments">
Parse the command arguments:
- The argument is a file path to the zing plan
- Example: `/zing:plan-linear .zing/recipe-app.md` -> filename = ".zing/recipe-app.md"

If no arguments provided:
- Use Glob to list all markdown files in the `.zing` directory
- If no files found, show an error and exit:
  ```
  No zing files found in .zing/
  Run /zing:new to create one.
  ```
- If one or more files found, use AskUserQuestion to let the user pick which zing plan to use
- Use the chosen file as the argument going forward
</step>

<step name="read_and_parse">
Read the zing plan file using the Read tool.

Parse the document to extract:
1. **Title** — the top-level heading or document name
2. **Phases** — look for the Action Plan section and identify phase groupings (e.g., "### Phase 1: Data Model", "### Phase 2: API Endpoints"). Each phase contains numbered steps.
3. **Full plan content** — the entire document text for attaching later

If the document has no Action Plan section or no identifiable phases, treat the entire Action Plan as a single phase.

Count the number of phases. This determines whether a project is created.
</step>

<step name="select_team">
Call `mcp__linear__list_teams` to get the available teams.

If there is exactly one team, use it automatically.

If there are multiple teams:
Before asking the user, send a browser notification so they know input is needed:
Call `notification_send(session_id, title="Input needed", body="Select a Linear team for ticket creation.")` where `session_id` is the session ID from the zing file frontmatter.
Use AskUserQuestion to let the user pick which team to create tickets in. Present each team as an option.
</step>

<step name="confirm_plan">
Before creating anything in Linear, show the user a summary of what will be created:

- **Plan title**: the extracted title
- **Team**: the selected team name
- **Project**: whether a project will be created (yes if >1 phase, no if single phase)
- **Tickets**: list each phase name that will become a ticket

Before asking the user, send a browser notification so they know input is needed:
Call `notification_send(session_id, title="Confirm ticket creation", body="Review the proposed Linear tickets and confirm.")` where `session_id` is the session ID from the zing file frontmatter.

Use AskUserQuestion to confirm: "Create these in Linear?"
- "Yes, create these" — proceed to the next step
- "View the plan first" — open the file for viewing (run `open -a Typora "{file_path}"`, falling back to `open "{file_path}"` if Typora is not installed), then re-ask this same confirmation question
- "Cancel" — stop and exit with "Cancelled — nothing was created in Linear."
</step>

<step name="ensure_zing_label">
Call `mcp__linear__list_issue_labels` and search for a label named "zing".

If it exists, note its name for use in later steps.

If it does not exist, call `mcp__linear__create_issue_label` with:
  - `name`: "zing"
  - `team`: the selected team

If label creation fails, warn the user but continue — labels are non-critical.
</step>

<step name="create_project">
If there is **more than one phase**, create a Linear project:

- Call `mcp__linear__create_project` with:
  - `name`: the zing plan title
  - `team`: the selected team
  - `labels`: `["zing"]`
  - `description`: a project summary written in Markdown, generated from the zing plan content. The description should give someone landing on the project a quick understanding of the work without reading every ticket. Include:

    1. **Goal** — 1-2 sentences on what is being built and why, drawn from the zing document's preamble/overview (the content before the Action Plan). This is the most important part.
    2. **Scope** — a compact bulleted list of the phases with a one-line summary of what each phase delivers (e.g., "Phase 1: Data Model — Postgres schemas for users, recipes, and ratings"). Don't repeat the full step details — just the essence.
    3. **Key technical decisions** — a brief list of the major tech stack choices, patterns, or architectural decisions mentioned in the plan (e.g., "Express + TypeScript, JWT auth, PostgreSQL with Prisma ORM"). Only include this if the plan specifies concrete technical choices. Skip if the plan is tech-agnostic.
    4. **Sequencing** — one sentence noting the dependency structure (e.g., "Phases are sequential — each builds on the previous" or "Phases 1-2 can be parallelized, Phase 3 depends on both").

    Keep the entire description concise — aim for roughly 10-20 lines of Markdown. This is a summary, not a copy of the plan.

    Append this footer at the end:
    ```
    ---
    Created by [Zing](https://github.com/Farmer-Pete/Zing)
    ```

Report the created project name to the user.

If there is only one phase, skip project creation.

If the API call fails, report the error to the user and stop. Do not proceed to ticket creation without a project if one was expected.
</step>

<step name="create_tickets">
For each phase, create a Linear issue:

- Call `mcp__linear__create_issue` with:
  - `title`: the phase name (e.g., "Phase 1: Data Model")
  - `team`: the selected team
  - `project`: the project name (if a project was created, otherwise omit)
  - `labels`: `["zing"]`
  - `description`: a well-structured ticket description for that phase, formatted as Markdown. Follow these guidelines when writing the description:

    1. **Lead with context, not implementation.** Start every ticket with a brief explanation of *what* the feature is and *why* it's needed. A developer picking up the ticket cold should understand the purpose before seeing any code details.
    2. **Separate requirements from implementation.** State what the system should do first (the "what"), then optionally provide implementation guidance (the "how") in a distinct section. Don't interleave them.
    3. **Use structured formats for dense specs.** When listing fields, parameters, or configurations, use tables (e.g., Field | Type | Constraints) or code blocks instead of comma-separated lists embedded in prose. If a spec reads like code, format it like code.
    4. **Use diagrams for model relationships.** When a ticket involves data models or entity relationships, include a Mermaid ER diagram showing the relevant models and how they connect. This is far easier to parse than prose descriptions of foreign keys and cardinality.
    5. **Group related items.** Don't use flat lists when items have natural categories. Tests should be grouped by area (e.g., happy path, filtering, error handling, access control). Implementation steps should follow a logical order with clear boundaries.
    6. **Make acceptance criteria specific.** "Works correctly" or "all filters work" aren't verifiable. Each criterion should describe an observable outcome that can be unambiguously checked.
    7. **Link to dependencies and related context.** Reference parent projects, prerequisite tickets, and relevant existing code patterns by name so the reader can navigate to them.
    8. **Optimize for scanning, not just completeness.** Having all the information isn't enough — it needs to be visually organized so a reader can find what they need without reading every word. Use headings, whitespace, and formatting deliberately.

    Include every step with its full description, acceptance criteria, file paths, and any other details from the plan — but restructure and reformat the content according to the guidelines above rather than dumping it verbatim.

    Append this footer at the end of every ticket description:
    ```
    ---
    Created by [Zing](https://github.com/Farmer-Pete/Zing)
    ```

If there are dependency relationships between phases (phase 2 depends on phase 1, etc.), set `blockedBy` on each ticket to reference the previous phase's ticket ID, creating a sequential dependency chain.

After creating each ticket, report its identifier (e.g., "TEAM-123") to the user.

If a ticket creation fails, stop immediately. Report the error and list which tickets were already created (so the user can clean up if needed). Do not continue creating remaining tickets.
</step>

<step name="attach_plan">
If a project was created (i.e., more than one phase), attach the full zing plan as a document:

- Call `mcp__linear__create_document` with:
  - `title`: "Zing Plan: {plan title}"
  - `project`: the project name or ID
  - `content`: the entire zing plan file content

Report that the plan was attached to the project.

If no project was created, skip this step.

If the document creation fails, report the error but continue to the next step — the tickets are already created and the document is non-critical.
</step>

<step name="update_zing_file">
Write the Linear ticket identifiers back into the zing plan file for traceability.

For each phase heading in the Action Plan section (e.g., `### Phase 1: Data Model`), append the ticket identifier in parentheses: `### Phase 1: Data Model (TEAM-123)`.

If a project was created, add a line at the top of the Action Plan section: `**Linear Project:** {project name}`

Use the Edit tool for each change. If any edit fails, report the warning but continue — this step is non-critical.
</step>

<step name="summary">
Print a summary of everything created:

- Project name (if created)
- List of tickets with their identifiers and phase names
- Whether the full plan was attached

End with: "Zing! Linear tickets created."

Before asking the user, send a browser notification so they know input is needed:
Call `notification_send(session_id, title="Tickets created", body="Linear tickets are ready. Choose next step.")` where `session_id` is the session ID from the zing file frontmatter.

Then use AskUserQuestion to ask the user what they'd like to do next:
- Option 1: "Start build" — invoke `Skill(skill: 'zing:build', args: '{file_path}')` where `{file_path}` is the path to the zing plan file
- Option 2: "Start build (fresh context)" — print the following and stop:
  ```
  Run these commands to start the build with a clean context window:
  /clear
  /zing:build {file_path}
  ```
  where `{file_path}` is the path to the zing plan file. Do NOT invoke the build skill — just print these instructions and end.
- Option 3: "Not now" — end the session without invoking anything
</step>

</process>

<anti_patterns>
- Don't create tickets without reading the full plan first
- Don't skip phases or merge them — one ticket per phase
- Don't create a project if there's only one phase
- Don't leave out step details from ticket descriptions — each ticket should contain ALL information for its phase
- Don't guess the team — always list teams and confirm
</anti_patterns>

<success_criteria>
Skill execution is complete when:

- [ ] Zing plan file was read and parsed (title, phases extracted)
- [ ] Team was selected (auto or user-picked)
- [ ] User confirmed what will be created
- [ ] "zing" label ensured (found or created)
- [ ] Project created in Linear with "zing" label (if >1 phase) or skipped (if single phase)
- [ ] One ticket created per phase, with "zing" label, full phase content, and Zing footer in description
- [ ] Dependency chain set between tickets (if multiple phases)
- [ ] Full plan attached as document to project (if project was created)
- [ ] Zing file updated with ticket identifiers on phase headings
- [ ] Summary printed with all identifiers
- [ ] User asked about next steps
</success_criteria>
