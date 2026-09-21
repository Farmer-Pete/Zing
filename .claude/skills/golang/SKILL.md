---
name: golang
description: Load this whenever you write, review, or debug Go code, set up or read the linters, pick a library, or wire a Datastar handler
---

# Go in the Zing repository

Zing is a Go web service with a Datastar hypermedia frontend. This skill sets the toolchain, the approved libraries, the commands you run before committing, and the coding rules the linters cannot enforce. For the full 100 Go mistakes reference open mistakes.md. For the mistakes generated code makes most, each with the linter or hook that guards it, open llm-pitfalls.md.

## Run before you commit

Run these from the module root and fix everything they report before you commit.

```bash
golangci-lint fmt          # format with gofumpt and gci
golangci-lint run          # lint with govet, staticcheck, errcheck, gosec, and the rest
go build ./...             # compile check; fails on any hallucinated import path
go vet ./...               # cheap correctness analyzers (also run inside golangci-lint)
```

Run these before you finish a change or open a pull request. The pre-push hook runs them too.

```bash
go test -race ./...                                  # tests with the data-race detector
go mod tidy && git diff --exit-code go.mod go.sum   # module file hygiene
govulncheck ./...                                    # reachable known-vulnerability scan
```

golangci-lint and go test -race do not overlap. Static linting cannot see data races, so the race detector is a separate required step.

## Toolchain and versions

Set `go 1.27` in go.mod and omit the `toolchain` line; when omitted it tracks the `go` line. Go 1.27 gives you per-iteration loop variables (added in Go 1.22), container-aware GOMAXPROCS (Go 1.25), and `sync.WaitGroup.Go` (Go 1.25) without any extra library. The two most recent major releases are supported; Go 1.27 and Go 1.26 are current as of 2026-09-21.

Install golangci-lint from a pinned version, not `go install`, so the local binary matches the pre-commit hook. Current stable is v2.13.2.

```bash
# pin exactly, so the local binary matches the pre-commit hook:
curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b $(go env GOPATH)/bin v2.13.2
# or, while brew's stable release is still v2.13.2:
brew install golangci-lint
```

The golangci-lint v2 config lives in .golangci.yml, starts with `version: "2"`, and moves formatters into a top-level `formatters` block. That config and the lefthook hooks are separate repository files this skill relies on; it does not restate them.

## Project layout

Grow the layout with the project. Do not start from golang-standards/project-layout, which is not an official standard and is heavier than a service needs.

```
go.mod
cmd/zing/main.go          thin entrypoint that wires dependencies and starts the server
internal/                 domain packages, private to this module
  <domain>/               organize by domain (auth, booking), not by layer
  transport/              HTTP handlers and Datastar SSE endpoints at the boundary
```

The compiler enforces `internal/`: nothing outside this module can import a path under it. There is no `pkg/` directory, because this is an application, not a library other modules import. Name packages for what they provide, and never name one common, util, shared, or helpers.

## Approved libraries

Import these. Reach for an alternative only under the stated condition. Record any new library you adopt in this table with its purpose and the condition for reaching for it.

| Name | Import path | Purpose | When to use |
| --- | --- | --- | --- |
| slog | `log/slog` | structured logging | always, the default logger |
| env | `github.com/caarlos0/env/v11` | parse env vars into a config struct | service configuration |
| validator | `github.com/go-playground/validator/v10` | struct and field validation by tag | after decoding a request or Datastar signals into a struct |
| errgroup | `golang.org/x/sync/errgroup` | goroutine groups with first-error and cancel | concurrent tasks that need error propagation |
| Datastar SDK | `github.com/starfederation/datastar-go/datastar` | read client signals, stream SSE patches | every Datastar handler; needs Go 1.24+ |
| templ | `github.com/a-h/templ` | typed, compiled HTML templates | render the HTML fragments passed to PatchElements |
| testify | `github.com/stretchr/testify` | assert and require helpers, mocks | tests, optional over stdlib testing |
| go-cmp | `github.com/google/go-cmp/cmp` | deep equality with readable diffs | comparing structs in tests, over reflect.DeepEqual |
| goleak | `go.uber.org/goleak` | goroutine leak detection | tests for code that spawns goroutines |
| chi | `github.com/go-chi/chi/v5` | HTTP router with route groups | only when net/http.ServeMux cannot express the routing, such as nested groups or per-subtree middleware |

Alternatives to hold in reserve. Use zerolog or zap only if profiling proves slog is a logging bottleneck. Use viper only if config later needs multi-format files or a remote store. `go.uber.org/automaxprocs` is unnecessary on Go 1.25 and later, which sets GOMAXPROCS from the container quota already.

With validator, decode a request body into a typed struct first, then call `validate.Struct`, never validate raw JSON. Reuse one package-level `validator.New()` instance across requests; it is safe for concurrent use.

## Coding rules the linters do not catch

These are judgment calls. The linters stay silent, so you own them. Each maps to a numbered entry in mistakes.md.

Interfaces and types. Return concrete structs and accept interfaces. Declare an interface only when a concrete caller needs polymorphism now, and declare it in the consumer package, not the producer. Reserve `any` for values that are genuinely any type; prefer a concrete type or a type parameter. Add generics only to remove real duplication across types. Embed a type only when you intend to promote its exported methods into your own public API.

Construction. For a constructor with many optional parameters, use functional options: `func WithTimeout(d time.Duration) Option` closures and a variadic `New(required, ...Option)`. Do setup in a constructor that can return an error, not in `init()`.

Slices and maps. Test emptiness with `len(s) == 0`, never `s == nil`. Pre-size with `make([]T, 0, n)` or `make(map[K]V, n)` when you know the final size. Isolate a shared backing array with a full slice expression `s[low:high:max]` or a `copy` into a fresh slice. When you retain a small sub-slice or substring of a large one long term, copy it out with `strings.Clone` or a fresh slice so the large backing array can be freed.

Errors. Wrap with `%w` when the caller should match the cause with errors.Is or errors.As; use `%v` to deliberately decouple from the underlying error. Handle each error once: either log it or return it, never both.

Concurrency. Default to unbuffered channels; size a buffer precisely and say why. Use `chan struct{}` for signal-only channels. Use a mutex to guard shared mutable state and a channel to coordinate or hand off ownership. Give every goroutine a stop signal, usually `ctx.Done()`. Size CPU-bound worker pools to `runtime.GOMAXPROCS(0)` and I/O-bound pools to the downstream system's capacity. Never let a map or slice guarded by a mutex escape the critical section; return a copy. Benchmark before assuming a concurrent version is faster.

Context. Pass `context.Context` as the first parameter and never store it in a struct field. For work that must outlive the request, derive a detached context with `context.WithoutCancel(ctx)`.

HTTP. Always `return` right after `http.Error`. Build an `http.Client` with an explicit Timeout, and an `http.Server` with ReadHeaderTimeout, ReadTimeout, and IdleTimeout, rather than the package defaults. Always close a response body, draining it first with `io.Copy(io.Discard, resp.Body)` so the connection can be reused.

Time. Build durations from the time unit constants, as in `500 * time.Millisecond`. Compare instants with `t1.Equal(t2)`, not `==`. Reuse one `time.NewTimer` with Reset instead of calling `time.After` inside a loop.

Testing. Drive cases from a table with `t.Run` subtests. Pass the current time in as a parameter so tests can fix it. Replace `time.Sleep` with polling or channel synchronization, or use `testing/synctest` virtual time. Gate slow tests behind a build tag or `testing.Short`.

## Reference files

Open mistakes.md when you want the full 100 Go mistakes table with the fix and detector for each, grouped by chapter. Open llm-pitfalls.md when you write or review generated Go and want the mistakes LLMs make most, each paired with the linter or hook that catches it and the rule to follow instead.
