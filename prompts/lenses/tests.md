## In a plan
Check:
- Every new or changed cut point has an integration test.
- Unit tests are limited to parsers and pure functions.
- Mocks are limited to cut points, and each is named.
- For a bug, the first test is the regression test, at a seam that
  reproduces the real bug pattern.
- Every task names the test written before it.
- No task's only change is a failing test: a regression test and its fix
  land in the same task.
- Every scenario's given and check can be observed by an agent inside the
  build sandbox. A scenario that needs a live `zing serve`, a machine
  outside the sandbox, the owner's own config (`~/.codex`, `~/.claude`,
  the console), a write under /tmp, a nested sandbox check it cannot
  start (`sandbox-exec`), or a sandbox probe that skips when sandboxed
  is a finding, unless the then names that skip as the expected result
  and the check proves it per the next bullet.
- A scenario whose then expects a skip has a check that runs `go test
  -v` and greps the skip line, such as `--- SKIP: TestName`, and the
  skip message. A bare `go test` exits 0 whether or not the test
  skipped, so it is a finding.

## In code
Check that the tests exist and assert behavior, not implementation. A
mock anywhere but at a cut point is a finding. A test that would pass with the
code removed is a finding.
