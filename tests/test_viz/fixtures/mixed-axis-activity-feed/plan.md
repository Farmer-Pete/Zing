# Activity feed (mixed-axis worked example)

A small two-step plan that exercises both axes of the viz schema in one
graph:

1. **Ingest** — logic-flavoured. The new `parse_event` operation walks
   incoming raw events past an existing `validate` decision and emits an
   `EventPayload` whose shape is partly changing (`actor_id` swaps from
   `int` to `UUID`; a new `source` field appears).

2. **Feed store** — data-flavoured. Stores `Activity` records (with
   per-field changes), introduces an `ActivityKind` tagged union, and
   adds an `ActivityCache` whose members are collections.

Cross-flows wire `EventPayload` into `Activity` (data) and route the
union's variant set back to the parser (schema).

The intent of this fixture is to keep the renderer + validator honest
about both primitives existing in the same viz at the same time.
