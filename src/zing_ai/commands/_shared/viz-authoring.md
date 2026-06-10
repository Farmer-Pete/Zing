# Viz Authoring — Shared Reference

Used by `/zing plan`, `/zing pr-audit`, `/zing plan-audit`, and `/zing build-audit`
to author the sibling `.viz.json` for a plan or audit. The schema's enum and
`description` fields are the contract; this file is the human-readable how-to
that explains side values, shape choice, struct usage, cross-flows, and the
coverage discipline that turns a valid viz into a useful one.

Every consumer of this file is expected to have already fetched the live schema
at `http://localhost:{port}/viz/schema.json` so they're working against the
current property set. The shape and side rules below match the schema; if they
ever diverge, the schema wins.

## Side values

Every step, node, and struct field carries one `side` from the same four-value
enum. The semantics are identical at all three levels — they describe what's
happening to that element relative to the change being captured:

- **`unchanged`** — present on both base and proposed; nothing is changing
  about it. Context the reader needs to understand where the changes sit
  (e.g. an existing module the new code calls into).
- **`added`** — new in this change; not on base. The node represents what's
  coming in.
- **`removed`** — present on base, going away in this change. The node
  represents what's leaving.
- **`diverged`** — same identity preserved, semantics shifted in place.
  Reserved for in-place changes where the site/name stays the same but the
  behaviour or value flips (e.g. `on_delete: CASCADE → SET_NULL`,
  `rate-limit: 100/min → 10/min`, `field type: TEXT → JSONB`).

The same four words apply whether you're marking:

- A whole **step** — wholly new vs. wholly going away vs. existing-but-modified-internally.
- A **node** inside a step — a logic node like `parse-event` or a data node like `Activity`.
- A **field** inside a struct node — per-slot, captures field-level granularity.

If a step contains only `unchanged` nodes, it's context — include it sparingly.
A step entirely `removed` (a module being removed) or entirely `added` (one
being added) is fine and expected.

### Where `diverged` can and can't go

- **Step `side: diverged`** — only when more than half the step's nodes carry
  non-unchanged sides AND the step's external interface is roughly unchanged.
  Renders the step's "REWORKED" pill.
- **Node `side: diverged`** — valid only with `shape: diverged` (which requires
  `concern` / `today_label` / `proposed_label`) or with `shape: struct` (which
  has its own internal field-level diverged semantics — see below).
- **Struct wrapper `side: diverged`** — NOT allowed. Per-field sides do the
  change-marking. Use `added` if the struct is wholly new, `removed` if
  wholly gone, `unchanged` if its identity is unchanged and only internal
  fields are moving.
- **Field `side: diverged`** — requires `today` + `proposed` (the slot's
  base-side value and the proposed-side value) so the renderer can show the
  in-place type/semantics shift.

## Logic & behaviour nodes

For nodes that represent something that *happens* — operations, decisions,
boundaries — pick the shape from how it runs:

- Operations → `rect`
- Decisions / branches → `diamond`
- Input/output boundaries → `parallelogram`
- Pre-existing module referenced (not the target of the change) → `hexagon`
- Same-site behavioural split → `diverged` (with `concern` / `today_label` /
  `proposed_label`)

## Data shape nodes

For nodes that represent something that *exists* — a named-field structure
whose internal slots are changing independently — use `shape: "struct"` with
the `kind` discriminator:

- **`kind: "struct"`** (default) — records, classes, tables, interfaces,
  request/response bodies, Pydantic/dataclass/Rust-struct/TS-interface.
  Anything with named fields that define what it *is*.
- **`kind: "union"`** — sum types, enums, tagged unions. Slots are variants
  (a slot without a `type` is fine — e.g. `"click"` / `"scroll"` / `"submit"`).
- **`kind: "collections"`** — modules/services/classes whose interesting
  members are containers (`List`, `Dict`, `Queue`, `Set`, `Deque`). Slots are
  what the scope *holds*, not what defines it.

A struct with mixed slot changes — one added field, one removed, one
type-changed — captures three independent change marks at field granularity.
The wrapper `side` describes what's happening to the struct as a whole; the
per-field sides do the within-struct change-marking.

**When NOT to use `struct`:** if a data type's change is at the outer level
(added/removed wholesale, type swapped — `List[X] → Set[X]`,
`Optional[User] → User`, `Dict → CaseInsensitiveDict`), reach for `rect` +
`side` (or `diverged` for in-place type swaps). Reserve `struct` for cases
where multiple internal slots are changing independently.

## Cross-step wiring

Use `cross_flows` whenever one step's output is consumed by another step's
input. Each carries a `kind` that colours the line:

- `data` — values flowing between steps
- `control` — triggering / control flow (request, event, signal)
- `schema` — the shape definition itself (a type produced by one step,
  consumed as a contract by another)
- `queue` — async / queued handoff
- `utility` — shared infra / helper used by multiple steps
- `observability` — logs, metrics, traces

A logic step producing a struct that downstream consumes is the canonical
cross-axis pattern — `kind: "data"` if the wire carries values,
`kind: "schema"` if it carries the shape definition itself.

## Worked example — both axes in one viz

A change adds a per-user activity feed:

- **Step `ingest`** — logic-flavoured.
  - `parse-event` (`rect`, `added`) — the new parser.
  - `validate` (`diamond`, `unchanged`) — existing decision point.
  - `event-payload` (`struct`, `kind: "struct"`, `unchanged`) — the message
    shape the parser produces; one diverged field (`actor_id`: `int → UUID`),
    one added field (`source: str`).
- **Step `feed-store`** — data-flavoured.
  - `Activity` (`struct`, `kind: "struct"`, `unchanged`): `id` (`unchanged`,
    note `PK`), `kind` (`diverged` → `ActivityKind`), `payload` (`added`),
    `legacy_blob` (`removed`).
  - `ActivityKind` (`struct`, `kind: "union"`, `added`): variants `click`,
    `scroll`, `submit`.
  - `ActivityCache` (`struct`, `kind: "collections"`, `added`):
    `recent: Deque[Activity]`, `pending_writes: List[Activity]`.
- **Cross-flows** — `event-payload` in `ingest` → `Activity` in `feed-store`,
  `kind: "data"`. `ActivityKind` in `feed-store` → `parse-event` in `ingest`,
  `kind: "schema"`.

Each step anchors one axis; the cross-flows show how they couple.

## Coverage check (mandatory before writing the file)

Walk the source — the plan markdown, the PR diff, or the build diff — against
the viz side-by-side. Confirm:

- **Every significant unit in the source appears in the viz** — every plan
  step, every new data model / function / class / module, every new decision
  point or boundary, every same-site behavioural change. A unit with no
  presence in the viz means a missing node, a missing struct field, or a
  step that should be split.
- **Every exception branch or outcome branch in new code is a
  `diamond`-rooted sub-DAG** — not a single `rect`. Exception paths are
  first-class topology; reviewers need to see them at a glance.
- **A single `rect` that summarises a whole new module is too coarse.** Aim
  for ~5–8 nodes per step; if a step has more than ~10 nodes, consider
  splitting; if it has 1–2 nodes, it's probably context for a neighbouring
  step.
- **For PR review specifically** — every changed file should be representable
  as a node, a node label, or an edge/cross-flow label. A changed file with
  no presence in the viz means a missing node or a step that should be split.

If the coverage check surfaces gaps, add nodes or split steps **before**
writing. Don't write a viz you can't defend as covering everything the
change touches.
