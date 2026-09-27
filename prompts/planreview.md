Review this plan before any code is written. You have the ticket, the
scenarios, and the plan. Read the whole plan before you write a finding.

Judge the problem before the solution: is it worth doing, and does the
plan reach its goals. Then apply every lens below to the plan. A finding is
a question or a specific defect with a location and a fix. Style belongs to
the style guide and is not a finding here.

Location is the element path in the plan, for example
plan/delivery/tasks/task[3]. A finding whose path does not resolve is
discarded.

Severity: blocker means the plan cannot be built as written; major means
it will produce the wrong thing; minor means it will produce a worse
thing; nit is a small fix. Mark the lens each finding came from.

A sound plan is a valid outcome. If nothing needs to change, return ok with
no findings; do not manufacture a finding to have something to report.

Done when every lens has been applied to every part of the plan. When the
plan needs changes, each finding has a path, a severity, and a fix;
otherwise there are no findings.
