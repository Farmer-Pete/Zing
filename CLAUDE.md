# Zing

Go web service with a Datastar frontend. Module path `zing`, entrypoint `cmd/zing`, domain packages under `internal/`.

- Before writing Go, load the `golang` skill. Before writing `data-*` attributes, SSE handlers, or Rocket components, load the `datastar` skill.
- When reviewing Go, or before committing generated Go, read `.claude/skills/golang/llm-pitfalls.md`: the mistakes generated code makes most, each with the linter that catches it and the rule to follow instead. When a linter finding or a review question maps to one of the 100 Go mistakes, look it up by number in `.claude/skills/golang/mistakes.md` for the fix and the detector.
- Before committing, run `make fmt lint` and `go build ./...`. The lefthook pre-commit hook runs the lint and build checks the same way, so a failure there fails for the same reason; its format step only rewrites staged files, unlike `make fmt`, which rewrites the whole tree. `make ci` runs everything CI's checks job runs; the secrets job additionally runs a full-history gitleaks scan.
- While iterating, `make test-short` (`go test -short ./...`) skips the slow end-to-end flows for fast feedback; `make test`, `make test-race`, and `make ci` always run the full suite.
- New clones need `make hooks-install` once.

## Dogfood the console

The owner runs Zing from the web console, so every owner action goes through it, and using it is how it gets tested.

- When driving Zing (reading questions, answering, approving gates, deciding review and perimeter items, picking up issues, merging), act through the console with real clicks and typing. The database and logs are for reading state and diagnosis only.
- When the console can't do a step, or does it badly, that is a console bug: file it and fix the console. A single logged workaround may unblock that one step; name the bug it works around.
- When building an owner action, put it in one function its console route calls. Owner actions get no CLI verb.

## Style

- Standard library first. Every new dependency is named in the plan with one line of why.
- Generics for containers only. The value of the type system is what appears after you type a dot.
- Small interfaces, defined where they are used. Methods on the type they belong to.
- Layered APIs: the simple case takes one call. The complex case is possible with more.
- Strategy over visitor when a pattern is warranted at all. Three repetitions before an abstraction.
- Name the parts of a compound condition. Behavior lives on the thing that does it.
- Recursive descent for any parsing. No parser generators.
- One process. No new service without a plan that names the network call it adds.
- Reproduce a bug with a failing test before fixing it, when a correct seam exists.
- Tests are integration tests at cut points. Unit tests for parsers and pure functions. Mocks only at cut points.
- Log every major branch with the ids. Wrap errors with `%w`. Never log a secret.
- Every JSON column has a JSON Schema. Every write is validated against it.
- A plan that adds a schema under `internal/store/schemas` also lists its example at the same relative path under `internal/store/examples`, and `internal/schemagen/schemagen_test.go`, whose registry count test breaks on every new schema. A new event kind also lists `internal/store/events_test.go`, whose `TestEventKinds` names every kind.
