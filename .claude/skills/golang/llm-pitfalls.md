# Go pitfalls in generated code

These are the Go mistakes generated code makes most. Each row pairs a mechanical guard with the rule to follow. The guard runs inside golangci-lint or a git hook. The commands that run them are in SKILL.md; the full 100 mistakes are in mistakes.md.

Only five linters are on by default in golangci-lint v2: errcheck, govet, ineffassign, staticcheck, and unused. Guards marked opt-in must be listed under `linters.enable` in .golangci.yml. This skill assumes the repository config enables the opt-in guards named below.

## Correctness

| Pitfall | Guard | Rule |
| --- | --- | --- |
| Dropping an error return | errcheck (default) | Check every error; write `_ = f()` with a reason comment only when the drop is intended. |
| Returning nil inside an `if err != nil` block | nilerr (opt-in) | Return the error, or a wrapped error, from the failure branch. |
| Returning nil, nil for a (pointer, error) pair | nilnil (opt-in) | Return a real value, or a sentinel error, not a bare nil, nil. |
| break inside a switch that is inside a loop | staticcheck SA4011 (default) | Use a labeled break to leave the outer loop. |
| Missing return after http.Error | review | Return right after http.Error, or the handler keeps running. |
| Integer overflow on a narrowing conversion | gosec G115 (opt-in) | Validate the range before converting. |
| reflect.DeepEqual in a test | review | Compare with github.com/google/go-cmp/cmp. |

## Concurrency

| Pitfall | Guard | Rule |
| --- | --- | --- |
| Loop variable captured by a goroutine | govet loopclosure (default) | Go 1.22+ fixes this at the language level; drop stale `v := v` copies (copyloopvar, opt-in). |
| wg.Add called inside the goroutine | staticcheck SA2000 (default) | Add before the go statement, or use wg.Go on Go 1.25+. |
| Copying a struct that holds a sync.Mutex | govet copylocks (default) | Pass such structs by pointer. |
| Mixing atomic and plain access to one variable | go test -race | Access a shared variable through one mechanism only, either sync/atomic (prefer the typed wrappers) or a mutex. |
| Shared state without synchronization | go test -race | Guard with a mutex, use sync/atomic, or hand off ownership on a channel; static linting cannot see data races. |

## Context and resources

| Pitfall | Guard | Rule |
| --- | --- | --- |
| A new context.Background() mid-call-chain | contextcheck (opt-in) | Thread the caller's ctx through; it is the first parameter. |
| An HTTP call built without a context | noctx (opt-in) | Use http.NewRequestWithContext. |
| context.Context stored in a struct field | containedctx (opt-in) | Pass ctx to each method instead. |
| Unclosed http.Response.Body | bodyclose (opt-in) | `defer resp.Body.Close()` and drain the body. |
| Unchecked error from Close, Flush, or Remove | review | The std-error-handling exclusion preset silences errcheck for these calls, so on a writer capture the Close error in a deferred closure and assign it to a named return. |
| Unclosed sql.Rows or sql.Stmt | sqlclosecheck (opt-in) | `defer rows.Close()`, and check rows.Err() (rowserrcheck). |
| defer inside a loop | revive defer or gocritic deferInLoop (opt-in) | Extract the loop body into a function so defer fires each iteration. |

## Idiom and style

| Pitfall | Guard | Rule |
| --- | --- | --- |
| interface{} instead of any | modernize any or revive use-any (opt-in) | Write any. |
| Deprecated io/ioutil | staticcheck SA1019 (default) | Use the os and io equivalents, such as os.ReadFile. |
| fmt.Println or fmt.Printf for logging | forbidigo (opt-in, default pattern) | Log with slog. |
| Returning an interface from a constructor | ireturn (opt-in) | Return the concrete type. |
| Octal file mode written as 0644 | gocritic octalLiteral (opt-in) | Write 0o644. |
| Unused function parameter | unparam or revive unused-parameter (opt-in) | Remove it, or rename to `_` when an interface requires the signature. |
| Shadowed err in an inner block | govet shadow (opt-in, best-effort) | Rename the inner variable, or assign with `=` to reuse the outer err. |
| String built with `+=` in a loop | review | Use strings.Builder, or strings.Join for a []string. |
| time.Sleep in a test | review (custom forbidigo pattern) | Poll, synchronize via a channel, or use testing/synctest. |

## Hallucinated packages and APIs

`go build ./...` fails hard on any import path that does not resolve, and `go mod tidy` will not add a module that does not exist. golangci-lint's typecheck pass, which always runs and cannot be disabled, reports undefined symbols on a real package before other linters run. Verify a package exists before relying on it. To block a real but disallowed module, use depguard for per-import rules or gomodguard for per-module rules. govulncheck does not detect hallucinated packages; it only scans modules that already resolved and built.
