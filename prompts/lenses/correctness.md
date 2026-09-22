## In a plan
Check:
- Every type and field has an exact type and its constraints.
- Every state lists every allowed value and every transition.
- Every rule reads as if X then Y and has a worked example.
- Every edge case and failure mode names its exact behavior.
- Every migration has schema, backfill, locks, compatibility, and rollback.
- The parts agree with each other: files, changes, types, tests, tasks.

## In code
Find logic errors, off-by-one, nil and error paths, races, leaks, and
any place the code disagrees with the types and rules the plan stated.
