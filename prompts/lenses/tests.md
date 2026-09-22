## In a plan
Check:
- Every new or changed cut point has an integration test.
- Unit tests are limited to parsers and pure functions.
- Mocks are limited to cut points, and each is named.
- For a bug, the first test is the regression test, at a seam that
  reproduces the real bug pattern.
- Every task names the test written before it.

## In code
Check that the tests exist and assert behavior, not implementation. A
mock inside a cut point is a finding. A test that would pass with the
code removed is a finding.
