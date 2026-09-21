# The 100 Go mistakes

Every mistake from the 100 Go Mistakes book, grouped by its chapter, with a one-line fix and the detector that catches it. In the detector column, a linter name is a golangci-lint linter; `review` means no tool catches it and you self-check. The judgment calls (the `review` rows) are expanded into actionable prose in SKILL.md. The mistakes generated code makes most are in llm-pitfalls.md with their guards.

## Code and project organization

| # | Title | Fix | Detector |
| --- | --- | --- | --- |
| 1 | Unintended variable shadowing | Give inner-scope variables distinct names; do not re-`:=` a name you mean to reuse. | govet shadow (opt-in, best-effort) |
| 2 | Unnecessary nested code | Return early; drop the else after a returning if. | gocritic elseif, ifElseChain, nestingReduce |
| 3 | Misusing init functions | Initialize in a constructor that can return an error. | gochecknoinits |
| 4 | Overusing getters and setters | Access fields directly unless the accessor adds behavior. | review |
| 5 | Interface pollution | Declare an interface only for a current polymorphism need. | review |
| 6 | Interface on the producer side | The producer returns concrete types; the consumer declares the interface. | review |
| 7 | Returning interfaces | Return concrete types. | ireturn |
| 8 | any says nothing | Use any only for genuinely any-type values. | review |
| 9 | Confusion about when to use generics | Add type parameters only to remove real duplication. | review |
| 10 | Type embedding problems | Embed deliberately; it promotes exported methods into your public API. | review |
| 11 | Not using the functional options pattern | Use WithX closures with a variadic New. | review |
| 12 | Project misorganization | Organize packages by domain; avoid premature sub-packaging. | depguard (once a scheme is set) |
| 13 | Creating utility packages | Name packages for what they provide; never common, util, or shared. | review |
| 14 | Ignoring package name collisions | Alias imports so a package name never collides with a local variable. | gocritic importShadow |
| 15 | Missing code documentation | Document each exported identifier with a sentence starting with its name. | review (revive's exported rule is not enabled here) |
| 16 | Not using linters | Run golangci-lint. | this is the setup |

## Data types

| # | Title | Fix | Detector |
| --- | --- | --- | --- |
| 17 | Octal literals confusion | Write octal with a 0o prefix, as 0o644. | gocritic octalLiteral |
| 18 | Neglecting integer overflow | Validate ranges before arithmetic; Go wraps silently. | gosec G115 (conversions only) |
| 19 | Not understanding floating point | Compare within a delta, never with ==. | staticcheck SA4012 (NaN only) |
| 20 | Slice length vs capacity | len is usable elements; cap is backing-array size from the slice start. | review |
| 21 | Inefficient slice initialization | Use make([]T, 0, n) when the final length is known. | prealloc |
| 22 | Confusing nil and empty slice | Do not rely on nil-ness; nil and []T{} differ only for json and reflection. | review |
| 23 | Not properly checking if a slice is empty | Use len(s) == 0, never s == nil. | review |
| 24 | Inaccurate slice copies | copy copies min(len(dst), len(src)); pre-size dst. | staticcheck S1001 (adjacent) |
| 25 | Unexpected side effects using append | Use a full slice expression s[low:high:max] or copy to a fresh slice. | gocritic appendAssign |
| 26 | Slices and memory leaks | Copy needed elements out so the backing array can be freed. | review |
| 27 | Inefficient map initialization | Use make(map[K]V, n) when the size is roughly known. | review |
| 28 | Maps and memory leaks | Rebuild a long-lived map that shrinks; buckets never shrink. | review |
| 29 | Comparing values incorrectly | == fails on slices, maps, and funcs; use go-cmp in tests. | review |

## Control structures

| # | Title | Fix | Detector |
| --- | --- | --- | --- |
| 30 | Elements are copied in range loops | Index into the slice to mutate the source. | gocritic rangeValCopy |
| 31 | How range arguments are evaluated | The range expression is evaluated once; use an indexed loop if you mutate during iteration. | review |
| 32 | Pointer elements in range loops | Go 1.22+ fixes this; otherwise copy the loop variable before taking its address. | govet loopclosure; copyloopvar |
| 33 | Wrong assumptions during map iteration | Never rely on iteration order; sort keys for determinism. | review |
| 34 | Misunderstanding break scope | Use a labeled break to leave an outer loop from inside a switch. | staticcheck SA4011 |
| 35 | Using defer inside a loop | Extract the loop body into a function so defer fires each iteration. | staticcheck SA9001, SA5003; gocritic deferInLoop |

## Strings

| # | Title | Fix | Detector |
| --- | --- | --- | --- |
| 36 | Not understanding the concept of rune | Use utf8.RuneCountInString for a character count, not len. | review |
| 37 | Inaccurate string iteration | Range the value (for i, r := range s) for proper rune decoding. | staticcheck S1029, SA6003 |
| 38 | Misusing trim functions | TrimPrefix and TrimSuffix remove a literal; TrimLeft and TrimRight strip a cutset. | staticcheck SA1024, S1017 |
| 39 | Under-optimized string concatenation | Use strings.Builder, or strings.Join for a []string. | review |
| 40 | Useless string and []byte conversions | Operate on []byte with the bytes package instead of converting. | gocritic stringXbytes |
| 41 | Substring and memory leaks | Copy a retained substring with strings.Clone. | review |

## Functions and methods

| # | Title | Fix | Detector |
| --- | --- | --- | --- |
| 42 | Not knowing which receiver type to use | Choose by mutation, non-copyable fields, size, and consistency across the method set. | review |
| 43 | Never using named result parameters | Use named results where they document intent. | review |
| 44 | Side effects with named result parameters | Avoid a naked return after a defer sets a named result. | review |
| 45 | Returning a nil receiver | Return an untyped nil interface, not a nil concrete pointer stored in an interface. | staticcheck SA4023 |
| 46 | Using a filename as a function input | Accept io.Reader or io.Writer instead of a filename. | review |
| 47 | How defer arguments are evaluated | defer evaluates its arguments immediately; wrap in a closure to defer evaluation. | revive defer (partial) |

## Error management

| # | Title | Fix | Detector |
| --- | --- | --- | --- |
| 48 | Panicking | Reserve panic for unrecoverable conditions; return errors otherwise. | forbidigo (configurable) |
| 49 | Ignoring when to wrap an error | Use %w to preserve the chain for Is and As; use %v to decouple. | errorlint errorf; review (wrapcheck is not enabled here) |
| 50 | Comparing an error type inaccurately | Use errors.As, not a type assertion or switch. | errorlint asserts |
| 51 | Comparing an error value inaccurately | Use errors.Is, not ==. | errorlint comparison |
| 52 | Handling an error twice | Handle once: log it or return it, not both. | review |
| 53 | Not handling an error | Check every error; discard with an explicit `_ =` only when intended. | errcheck |
| 54 | Not handling defer errors | Capture and propagate Close or Rollback errors into a named return. | errcheck |

## Concurrency: foundations

| # | Title | Fix | Detector |
| --- | --- | --- | --- |
| 55 | Mixing up concurrency and parallelism | Design the structure first; parallelism depends on GOMAXPROCS. | review |
| 56 | Thinking concurrency is always faster | Benchmark sequential against concurrent; add a size threshold. | go test -bench |
| 57 | When to use channels or mutexes | Mutex for shared-state sync; channel for coordination and hand-off. | review (copylocks is adjacent) |
| 58 | Not understanding race problems | Guard shared memory; run go test -race. | go test -race |
| 59 | Concurrency impact of the workload type | Size CPU-bound pools to GOMAXPROCS, I/O-bound pools to the external system. | review |
| 60 | Misunderstanding Go contexts | Pass ctx as the first argument; call every cancel function. | govet lostcancel; containedctx; contextcheck |

## Concurrency: practice

| # | Title | Fix | Detector |
| --- | --- | --- | --- |
| 61 | Propagating an inappropriate context | Use context.WithoutCancel for detached work; never store ctx in a struct. | containedctx, contextcheck, fatcontext |
| 62 | Starting a goroutine without a stop condition | Select on ctx.Done() or a done channel. | goleak (tests) |
| 63 | Goroutines and loop variables | Go 1.22+ fixes this; otherwise pass the loop variable as an argument. | govet loopclosure |
| 64 | Expecting deterministic select behavior | Do not rely on case order; nest a non-blocking select for priority. | review |
| 65 | Not using notification channels | Use chan struct{} and close to broadcast. | review |
| 66 | Not using nil channels | Set a channel to nil to disable its select case. | review |
| 67 | Being puzzled about channel size | Default to unbuffered; size a buffer precisely and document it. | review |
| 68 | Side effects with string formatting | A String method reading mutable state must take the writers' lock. | go test -race |
| 69 | Creating data races with append | Guard shared-slice appends, or merge private slices after the join. | go test -race |
| 70 | Mutexes with slices and maps | Do not let a locked reference escape the critical section; return a copy. | go test -race |
| 71 | Misusing sync.WaitGroup | Add before go and defer Done first; or use wg.Go on Go 1.25+. | staticcheck SA2000; goleak |
| 72 | Forgetting about sync.Cond | Call Wait inside for !condition; never copy a Cond. | govet copylocks (copies only) |
| 73 | Not using errgroup | Use errgroup.WithContext for error propagation and cancel. | review |
| 74 | Copying a sync type | Pass structs that hold a mutex by pointer. | govet copylocks |

## Standard library

| # | Title | Fix | Detector |
| --- | --- | --- | --- |
| 75 | Providing a wrong time duration | Build durations from the time unit constants. | durationcheck |
| 76 | time.After and memory leaks in loops | Reuse one time.NewTimer with Reset. | review |
| 77 | Common JSON handling mistakes | Name embedded time.Time fields; json numbers into any are float64. | review |
| 78 | Common SQL mistakes | Ping after Open; tune the pool; check rows.Err() after the loop. | rowserrcheck, sqlclosecheck |
| 79 | Not closing transient resources | Close and drain response bodies; close sql.Rows and files. | bodyclose, sqlclosecheck |
| 80 | Forgetting the return after http.Error | Return right after http.Error. | review |
| 81 | Using the default HTTP client and server | Set client Timeout and server ReadHeaderTimeout, ReadTimeout, IdleTimeout. | gosec G112 (server) |

## Testing

| # | Title | Fix | Detector |
| --- | --- | --- | --- |
| 82 | Not categorizing tests | Gate slow tests with a build tag or testing.Short. | review |
| 83 | Not enabling the -race flag | Run go test -race in CI. | go test -race |
| 84 | Not using test execution modes | Use t.Parallel and go test -shuffle=on. | tparallel |
| 85 | Not using table-driven tests | Drive cases from a table with t.Run subtests. | copyloopvar (redundant copy) |
| 86 | Sleeping in unit tests | Poll or synchronize via channels; use testing/synctest. | review |
| 87 | Not handling the time API in tests | Pass the current time in as a parameter. | usetesting (adjacent) |
| 88 | Not using testing utility packages | Use httptest and iotest. | review |
| 89 | Writing inaccurate benchmarks | Reset the timer, use -count with benchstat, sink results to a var. | benchstat |
| 90 | Not exploring all testing features | Use coverprofile, external _test packages, t.Cleanup, and TestMain. | thelper |

## Optimizations

| # | Title | Fix | Detector |
| --- | --- | --- | --- |
| 91 | Not understanding CPU caches | Favor contiguous, unit-stride access over pointer chasing. | pprof, benchmarks |
| 92 | False sharing | Pad hot fields written by different goroutines onto separate cache lines. | pprof, benchmarks |
| 93 | Ignoring instruction-level parallelism | Reduce data hazards between adjacent instructions. | benchmarks |
| 94 | Not being aware of data alignment | Order struct fields largest to smallest to cut padding. | review (fieldalignment is not enabled here) |
| 95 | Not understanding stack vs heap | Avoid gratuitous sharing up; inspect with go build -gcflags=-m. | go build -gcflags -m |
| 96 | Not knowing how to reduce allocations | Reuse buffers, use m[string(b)] lookups, use sync.Pool. | prealloc (partial) |
| 97 | Not relying on inlining | Keep the fast path small; move rare branches to their own function. | go build -gcflags -m |
| 98 | Not using Go diagnostics tooling | Use net/http/pprof and go tool trace. | pprof, trace |
| 99 | Not understanding how the GC works | Tune GOGC under bursty load; verify with GODEBUG=gctrace=1. | GODEBUG=gctrace=1 |
| 100 | Impacts of running Go in Docker or Kubernetes | On Go 1.25+ GOMAXPROCS is container-aware; otherwise blank-import automaxprocs. | review |
