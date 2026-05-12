---
session: bak-1321-fixture
ticket_id: BAK-1321
complexity: standard
steps:
  plan: bak1321-plan
  plan-audit: bak1321-plan-audit
  build: bak1321-build
  build-audit: bak1321-build-audit
---
# BAK-1321 · DirectFlatten pipeline

Test fixture for the viz validator and layout engine. Mirrors the topology of
the canonical Convene prototype (`prototypes/plan-viz/v4/convene.html`) so
layout regressions can be caught against a known-good shape.

## Steps

1. **Schema + migration 0135** — three coupled schema changes (FK on_delete
   diverge, `flatten_status` column, partial index) packaged in one
   `atomic=False` migration.
2. **DIRECT_FLATTEN_PIPELINE flag** — org-scoped feature flag, default OFF.
3. **TimerGroup → utils/perf.py** — rename existing `Timer` to `MetricLogger`,
   lift inline timer utilities.
4. **HashReader · yield HashedRow** — diverge yield shape from tuple to a
   dataclass so the new direct-flatten consumer can access raw JSON text.
5. **publish_celery_task_to_dlq** — publish protocol-v2 envelopes to the
   shared DLQ via `current_app.send_task`.
6. **`_parse` · multi-event unpacking** — Google branch loops events, computes
   `file_offset`; non-Google branch yields once per line.
7. **`convert_to_struct`** — diverge `msgspec.convert` outcomes (success,
   ValidationError, Exception) to return tuples instead of dropping rows.
8. **`_flatten` · failure capture per row** — capture three result classes
   (ok, unflattenable, exception) without poisoning the batch.
9. **`_copy_activity` · COPY harness with inline tsvector** — replace the
   separate `temp_search_vector` JOIN with an inline `to_tsvector` call.
10. **`_copy_flattened` · WHERE NOT EXISTS dedup** — file-offset shapes
    diverge between pipelines.
11. **`sqs_message_handler` · flag-gated dispatch** — pick the new or old
    path based on the org-scoped flag.
12. **`flatten_retry_one` · per-row DLQ retry task** — status-guarded UPDATE
    makes re-drives idempotent.
13. **Sentry · `functions_to_trace` + `sample_map`** — remove phantom trace
    entries, add new tracing for direct-flatten.

## Notes

This plan is **not** intended to be built. It exists solely to exercise the
viz layout and validation pipeline against a realistically-sized graph
(13 steps, ~80 nodes, ~17 cross-flows).
