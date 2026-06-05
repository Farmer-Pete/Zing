
<objective>
Seed a Zing test session with a real plan + viz so the user can manually
exercise the plan-detail viewer (focus / release / pan / zoom) without
running the full `/zing/plan` → audit → build → audit pipeline.

The sim CLI handles all the MCP plumbing (session create, session update,
state file). Your job is content: produce a plausible `.md` + `.viz.json`
pair, then run the sim commands to attach them.

`$ARGUMENTS` is a free-form topic (real or synthetic) for the test plan.
If empty, ask the user once for a one-line topic, then proceed.
</objective>

<preconditions>

1. `zing-ai mcp` must be running on `http://localhost:9876`. Verify with
   `curl -s http://localhost:9876/viz/schema.json | head -c 60` — should
   return a JSON Schema header. If it doesn't, tell the user to start the
   server (`zing-ai mcp`) in another terminal and stop.

2. The `zing-ai` CLI must include the `sim viz-attach`, `sim url`, and
   `sim viz-teardown` subcommands. Verify with `zing-ai sim --help`. If
   they're missing, the user needs to update their install.

</preconditions>

<process>

<step name="pick_slug">
Derive a short URL-friendly slug from `$ARGUMENTS`. Lowercase, words
joined by hyphens, no special characters. Examples:
- `"caching layer rewrite"` → `caching-layer-rewrite`
- `"BAK-9999 testing flow"` → `bak-9999-testing-flow`

Use this slug consistently for filenames and the staging directory.
</step>

<step name="create_session">
Run `zing-ai sim create "Viz test · <slug>"`. This calls `session_create`
on the running MCP server and writes the new session_id to the local
sim state file (`~/.zing-ai-sim.json`).

If the command fails with "Could not connect to Zing server", the user's
`zing-ai mcp` isn't running — stop and surface that.
</step>

<step name="generate_pair">
Create the directory `/tmp/zing-sim/<slug>/`. Inside it, generate two
files sharing the slug as their stem:

- `/tmp/zing-sim/<slug>/<slug>.md` — plan markdown
- `/tmp/zing-sim/<slug>/<slug>.viz.json` — sibling viz graph

**Follow two existing step blocks in `~/.claude/commands/zing/plan.md`** —
do NOT invent your own content format. Apply only the deltas listed
below.

Block 1: **`<step name="flesh_out_document">`** — produces the markdown.
Follow it as written, with these adjustments:
- Write to `/tmp/zing-sim/<slug>/<slug>.md` instead of
  `.zing/<plan-slug>.md`.
- Skip the YAML frontmatter entirely (no `session:`, no `steps:`,
  no `ticket_id:`). The sim CLI doesn't read it.
- Keep the H1 — it must match the viz's `title` field.
- If the user gave a real topic that you don't know in depth, invent a
  plausible 3–6 step plan rather than refusing. The point is to
  exercise the viewer with realistic-looking content, not to ship a
  correct implementation plan.

Block 2: **`<step name="write_viz_graph">`** — produces the viz JSON.
Follow it as written, with these adjustments:
- Write to `/tmp/zing-sim/<slug>/<slug>.viz.json` instead of
  `.zing/<plan-slug>.viz.json`.
- The schema fetch (`WebFetch http://localhost:9876/viz/schema.json`)
  is mandatory — always refetch, do not assume the schema's shape.

Skip every other step in `plan.md` (read_zing_doc, assess_complexity,
explore_codebase, finalize_plan_step, next_steps) and skip every
`mcp__zing-ai__*` MCP call — the sim CLI is the substitute for session
lifecycle here.

Aim for 3–6 steps and 2–4 cross-flows so the viewer's focus mode has
something interesting to traverse.
</step>

<step name="attach">
Run:

```
zing-ai sim viz-attach /tmp/zing-sim/<slug>/<slug>.viz.json \
  --md /tmp/zing-sim/<slug>/<slug>.md
```

`sim viz-attach` validates the viz against the schema (fail fast — no
half-attach on a malformed graph), copies the pair into the per-session
staging directory `~/.zing-ai/sim-sessions/<session_id>/`, and calls
`session_update` over MCP to set `zing_file`. It prints a JSON summary
ending in `plan_url`.

If validation fails, read the printed errors (each carries a JSON
pointer and a "did you mean" hint where applicable), fix the issues in
`/tmp/zing-sim/<slug>/<slug>.viz.json`, and retry. Do not modify the
schema or work around validator errors.

If `viz-attach` reports `"session ... not found on server — the MCP
server may have been restarted"`, the server was restarted between
`sim create` and this step. Run `zing-ai sim viz-teardown`, then start
over from the `create_session` step.
</step>

<step name="report">
Run `zing-ai sim url --plan` to print the plan-detail URL, and surface
it to the user as the link to click. Tell them:

- The URL: `http://localhost:9876/command-center/<session_id>/plan`.
- That `zing-ai sim viz-teardown` will clear the local sim state and
  staging directory when they're done testing.
- That this flow exercises only the plan-detail viewer — NOT the
  kanban Design pill, plan-audit / build-audit sync, or the step
  validators. Direct them to the URL; do not expect to navigate from
  the kanban.
</step>

</process>

<verification>

Before declaring success:

- `zing-ai sim viz-attach` returned exit 0 with a non-zero `steps` count
  and a non-zero `cross_flows` count in the summary JSON.
- `zing-ai sim url --plan` printed a plan-detail URL.
- `curl -sI <plan-url>` returns `200 OK`. If it returns 404, the
  staged sibling pair has drifted — verify both `<session_id>.md` and
  `<session_id>.viz.json` exist under
  `~/.zing-ai/sim-sessions/<session_id>/`.

</verification>
