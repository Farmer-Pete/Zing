# Viz Preview Gate — Shared Reference

Used by `/zing pr-audit`, `/zing plan-audit`, `/zing build-audit` to surface the
viz + markdown in the Command Center Flow for human approval **before** the
audit's findings or plan changes are officially submitted to Zing.

The pattern mirrors the existing `discuss` action on findings: the user can
accept, reject (with comments), or skip. Reject opens a free-form discussion
in the **Claude Code session** (no browser chat UI) — you discuss inline,
optionally edit the viz + markdown, and optionally re-submit for another
review round.

## When to gate

- **`/zing pr-audit`** — Two gates:
  - **Gate A (early)**: right after `build_topology_viz`, before `big_picture`.
    Only fire if a viz was actually written (skip the gate if the PR was
    classified as structurally trivial and no `.viz.json` was produced).
    `gate_label = "Topology review"`.
  - **Gate B (late)**: right after `write_report`, before `submit_review`.
    Only fire if a viz was actually written. `gate_label = "Final audit review"`.
- **`/zing plan-audit`** — One gate, inside `sync_viz_graph` immediately after
  the viz JSON is rewritten and re-validated, before the closing `step_stop`.
  Only fire if the audit actually changed the markdown (otherwise no viz
  rewrite was needed). `gate_label = "Plan audit review"`.
- **`/zing build-audit`** — One gate, same placement as `plan-audit`.
  `gate_label = "Build audit review"`.

Skip the gate entirely if no viz exists for the session (`.viz.json` absent or
not rewritten this run).

## Procedure

1. **Request the preview.** Call:

   ```
   mcp__zing-ai__viz_preview_request(
     session_id=<session_id from the zing file's frontmatter>,
     viz_path=<absolute path to the .viz.json>,
     md_path=<absolute path to the sibling .md>,
     gate_label=<from the table above>,
   )
   ```

   If the response includes `"error"` with `"issues"`, the viz failed schema
   or cross-reference validation. Fix the JSON and retry — do **not** call
   `viz_preview_wait` until `viz_preview_request` returns `{"status":
   "requested"}`. The user will see the preview as a card in the Flow with
   tabbed Visualization / Markdown views and a comment box.

2. **Wait for the decision.** Call:

   ```
   mcp__zing-ai__viz_preview_wait(session_id=<same session_id>)
   ```

   This blocks until the user clicks Accept or Reject. Skip does not resolve
   the wait — the preview stays queued. Returns
   `{"decision": "accept" | "reject", "comments": "<text>"}`.

3. **Handle the decision.**

   - **Accept, no comments** → Proceed to the next step.
   - **Accept, with comments** → Read the comments, apply them to the viz +
     markdown as the user clearly intended (small tweaks, naming changes,
     phrasing fixes). Do **not** re-request the preview — the user already
     approved. Re-validate the viz (`zing-ai viz validate <slug>`) before
     proceeding.
   - **Reject (with or without comments)** → Open a free-form discussion in
     the Claude Code session:
     - If comments are empty, ask the user what they want changed.
     - If comments are present, summarize your understanding ("I read this
       as wanting X, Y, Z — does that match?") and clarify any ambiguities.
     - Iterate on the artifact: edit the viz + markdown as the discussion
       converges. Re-validate after each meaningful edit.
     - When the user signals they are satisfied with the proposed changes,
       ask: **"Re-submit for another review round in the Flow, or proceed
       without another review?"**
       - If "re-submit" → loop back to step 1. The iteration counter on the
         preview card increments automatically.
       - If "proceed" → continue to the next step in the audit skill.

## Notes

- The back-and-forth on reject is conversational. Use plain prose; reach for
  `AskUserQuestion` only at decision points ("re-submit or proceed?",
  "accept the rewritten step 3 as-is?"). Do not put each turn behind a
  question menu.
- Replacing a pending preview with a new `viz_preview_request` call
  auto-resolves any waiting `viz_preview_wait` as a reject with a
  "superseded" note, so a stale wait never blocks a fresh request.
- The preview is rendered from disk every time it is opened. Edits to the
  viz/md files are picked up on the next view — no separate "publish" step.
