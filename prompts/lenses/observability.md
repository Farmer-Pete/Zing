## In a plan
Check that the plan says what is logged at each major branch and with
which ids, that log levels and a runtime switch exist, that sensitive
data is kept out of logs by design, and that a failure could be
reproduced after the fact from what is recorded.

## In code
Check that every log line carries ticket_id and run_id where they exist,
that errors are wrapped with %w and are returned or logged and never
both, that no secret or personal data is a log field, that major
branches log, and that levels are consistent.
