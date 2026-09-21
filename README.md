# Zing

A Go web service with a [Datastar](https://data-star.dev) frontend.

## Setup

Install the toolchain and the tools the git hooks call, then wire the hooks into this clone.

```sh
brew install go golangci-lint lefthook gitleaks govulncheck
make hooks-install
```

## Commands

The Makefile is the single source of truth. The git hooks and CI call these same targets.

| Command | What it does |
| --- | --- |
| `make fmt` | Rewrite files with gofumpt and gci through `golangci-lint fmt` |
| `make fmt-check` | Fail if any file is unformatted (what CI runs) |
| `make lint` | Verify `.golangci.yml` and run the linter set |
| `make build` | Compile everything |
| `make vet` | Run `go vet` |
| `make test` | Run the unit tests |
| `make test-race` | Run the unit tests with the race detector |
| `make tidy-check` | Fail if `go.mod` or `go.sum` are not tidy |
| `make vuln` | Scan for reachable known vulnerabilities |
| `make pre-commit` | What the pre-commit hook's lint and build steps check, plus a whole-tree `fmt-check`; excludes the staged secret scan and the hook's own staged-only format rewrite |
| `make pre-push` | What the pre-push hook checks: test-race, tidy-check, vuln |
| `make ci` | What the CI checks job runs: pre-commit, vet, pre-push |
| `go run ./cmd/zing` | Start the server on `:8080` (override with `ZING_ADDR`) |

## Git hooks

`lefthook.yml` defines them. Pre-commit formats staged Go files, lints, builds, and scans the staged diff for secrets. Pre-push runs the race tests, the tidy check, and the vulnerability scan. CI runs the same targets plus a full-history secret scan.

## Agent skills

`.claude/skills/golang` and `.claude/skills/datastar` hold the coding rules, the 100 Go mistakes with the linter that catches each, and the Datastar attribute, Rocket, and Go SDK references.

## License

MIT. See LICENSE.
