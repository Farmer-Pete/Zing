# Zing

Go web service with a Datastar frontend. Module path `zing`, entrypoint `cmd/zing`, domain packages under `internal/`.

- Before writing Go, load the `golang` skill. Before writing `data-*` attributes, SSE handlers, or Rocket components, load the `datastar` skill.
- When reviewing Go, or before committing generated Go, read `.claude/skills/golang/llm-pitfalls.md`: the mistakes generated code makes most, each with the linter that catches it and the rule to follow instead. When a linter finding or a review question maps to one of the 100 Go mistakes, look it up by number in `.claude/skills/golang/mistakes.md` for the fix and the detector.
- Before committing, run `make fmt lint` and `go build ./...`. The lefthook pre-commit hook runs the lint and build checks the same way, so a failure there fails for the same reason; its format step only rewrites staged files, unlike `make fmt`, which rewrites the whole tree. `make ci` runs everything CI's checks job runs; the secrets job additionally runs a full-history gitleaks scan.
- New clones need `make hooks-install` once.
