# Zing

Go web service with a Datastar frontend. Module path `zing`, entrypoint `cmd/zing`, domain packages under `internal/`.

- Before writing Go, load the `golang` skill. Before writing `data-*` attributes, SSE handlers, or Rocket components, load the `datastar` skill.
- When reviewing Go, or before committing generated Go, read `.claude/skills/golang/llm-pitfalls.md`: the mistakes generated code makes most, each with the linter that catches it and the rule to follow instead. When a linter finding or a review question maps to one of the 100 Go mistakes, look it up by number in `.claude/skills/golang/mistakes.md` for the fix and the detector.
- Before committing, run `make fmt lint` and `go build ./...`. The lefthook pre-commit hook runs the lint and build checks the same way, so a failure there fails for the same reason; its format step only rewrites staged files, unlike `make fmt`, which rewrites the whole tree. `make ci` runs everything CI's checks job runs; the secrets job additionally runs a full-history gitleaks scan.
- New clones need `make hooks-install` once.

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
