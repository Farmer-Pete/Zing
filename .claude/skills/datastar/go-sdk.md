# Datastar Go SDK

The official server SDK for Datastar in Go. Use it to read incoming signals from a request and to stream SSE responses that patch DOM elements and signals back to the client. It is router agnostic and works as a plain `http.HandlerFunc`.

## Import and version

The module is `github.com/starfederation/datastar-go`; import the package `github.com/starfederation/datastar-go/datastar`. Install with `go get github.com/starfederation/datastar-go`. The latest tag is v1.2.2 (2026-06-02), which tracks Datastar core v1.0.2. The module's go.mod requires Go 1.24 or later; this repo targets Go 1.27, which is compatible.

Do not use the old path `github.com/starfederation/datastar/sdk/go`; it is a stale, superseded package. Do not use community forks such as `github.com/awryme/datastar-go`, which still use the pre-1.0 merge-fragments vocabulary. The SDK version and the browser bundle version move independently, so check https://github.com/starfederation/datastar-go/tags for a newer SDK tag rather than assuming SDK and core match.

## Reading signals

`ReadSignals(r *http.Request, signals any) error` decodes the Datastar-sent signal payload, from the query parameter for GET or the JSON body otherwise, into a Go value. Use a struct with `json` tags that mirror the signal namespace, including nested structs for dotted signal paths like `foo.bar`.

```go
type Store struct {
	Count int `json:"count"`
	Form  struct {
		Bar string `json:"bar"`
	} `json:"foo"`
}
store := &Store{}
if err := datastar.ReadSignals(r, store); err != nil {
	http.Error(w, err.Error(), http.StatusBadRequest)
	return
}
```

## Opening a stream

`NewSSE(w http.ResponseWriter, r *http.Request, opts ...SSEOption) *ServerSentEventGenerator` sets the SSE headers and returns the generator. SSE options include `WithCompression(...)` for gzip, brotli, or zstd, and `WithContext(ctx)`.

## Generator methods

Every method returns an error; check it, since a mid-stream write failure usually means the client disconnected.

| Method | Purpose |
| --- | --- |
| PatchElements(elements string, opts ...PatchElementOption) | Patch DOM elements, matched by id unless a selector option is given |
| PatchElementTempl(c TemplComponent, opts ...PatchElementOption) | Patch a rendered templ component directly |
| RemoveElement(selector string, opts ...PatchElementOption) | Remove elements matching a selector |
| PatchSignals(signalsContents []byte, opts ...PatchSignalsOption) | Merge a JSON signals payload into client state |
| MarshalAndPatchSignals(signals any, opts ...PatchSignalsOption) | Marshal a Go value to JSON and patch signals |
| ExecuteScript(scriptContents string, opts ...ExecuteScriptOption) | Run JavaScript on the client |
| Redirect(url string, opts ...ExecuteScriptOption) | Redirect the browser |
| ConsoleLog(msg string, opts ...ExecuteScriptOption) | Log to the browser console |
| DispatchCustomEvent(eventName string, detail any, opts ...DispatchCustomEventOption) | Dispatch a custom DOM event |
| IsClosed() bool | Whether the connection is closed |
| Context() context.Context | The request-derived context, cancelled on disconnect |

## Patch element options

Mode constants are `ElementPatchModeOuter` (default), `ElementPatchModeInner`, `ElementPatchModeReplace`, `ElementPatchModePrepend`, `ElementPatchModeAppend`, `ElementPatchModeBefore`, `ElementPatchModeAfter`, and `ElementPatchModeRemove`. Their DOM semantics are documented in attributes.md under the SSE wire format.

Options:

- `WithSelector(selector string)`, `WithSelectorID(id string)` for `#id`, and `WithSelectorf(format string, args ...any)`.
- `WithMode(mode ElementPatchMode)` plus one-call shortcuts `WithModeOuter()`, `WithModeInner()`, `WithModeReplace()`, `WithModePrepend()`, `WithModeAppend()`, `WithModeBefore()`, `WithModeAfter()`, `WithModeRemove()`.
- `WithUseViewTransitions(bool)`, `WithViewTransitions()`, `WithoutViewTransitions()`, and `WithViewTransitionSelector(selector string)`.
- `WithNamespace(namespace Namespace)` with `WithNamespaceHTML()`, `WithNamespaceSVG()`, `WithNamespaceMathML()`.
- `WithPatchElementsEventID(id string)` and `WithRetryDuration(d time.Duration)`, which sets the SSE retry line so the browser backs off correctly.

## Patch signal options

`WithOnlyIfMissing(bool)` maps to the `onlyIfMissing` field, so a signal is applied only when absent client-side; use it to seed defaults without clobbering user edits. Also `WithPatchSignalsEventID(id string)` and `WithPatchSignalsRetryDuration(d time.Duration)`. Setting a signal value to `null` in the payload removes it.

## Handler pattern

A typical handler reads signals, opens the stream, then interleaves element and signal patches over one connection.

```go
func handler(w http.ResponseWriter, r *http.Request) {
	store := &Store{}
	if err := datastar.ReadSignals(r, store); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	store.Count++
	if err := sse.PatchElements(fmt.Sprintf(`<div id="count">%d</div>`, store.Count)); err != nil {
		return
	}
	if err := sse.MarshalAndPatchSignals(store); err != nil {
		return
	}
}
```

## Long-lived streams

Two things bite long-lived SSE handlers. Both have a definite fix.

The server write deadline kills the stream. `http.Server.WriteTimeout` sets a TCP-layer deadline the moment the request header is read, before your handler runs, and context deadlines do not affect it. A stream cut at exactly N seconds with `ERR_INCOMPLETE_CHUNKED_ENCODING` in the browser is this bug. Fix it per request by clearing the deadline before writing any events:

```go
rc := http.NewResponseController(w)
if err := rc.SetWriteDeadline(time.Time{}); err != nil {
	http.Error(w, "streaming unsupported", http.StatusInternalServerError)
	return
}
```

At scale, set `WriteTimeout: 0` on the server and apply a per-request deadline via ResponseController in a middleware on the non-streaming routes only, excluding SSE routes. `ReadHeaderTimeout` is safe to keep on SSE routes; it only bounds header reads.

The handler must exit on disconnect and shutdown. A blocking send loop leaks a goroutine when the browser navigates away or the server drains. Select on the request context, which is cancelled on both client disconnect and server Shutdown, and return promptly.

```go
for {
	select {
	case <-r.Context().Done():
		return
	case <-time.After(time.Second):
		if err := sse.PatchElements(`<div id="clock">` + time.Now().Format(time.TimeOnly) + `</div>`); err != nil {
			return
		}
	}
}
```

## Templating

The SDK has first-class templ support through `PatchElementTempl`, which takes any value satisfying `templ.Component`, so you render a component straight into a patch without a manual render-to-string. templ (`github.com/a-h/templ`) is the path with the most first-party and community documentation for Go plus Datastar, and it has an official Datastar integration guide. Prefer it for this repo.

Alternatives, in decreasing order of type safety on the attribute strings themselves:

- `github.com/Yacobolo/datastar-templ` adds compile-time-typed Datastar attribute builders on top of templ, catching typo'd signal or attribute names at compile time.
- `maragu.dev/gomponents-datastar` gives Datastar attribute helpers for the pure-Go gomponents library, for teams that prefer plain Go functions over a codegen step.
- Plain `html/template` works, since PatchElements just needs an HTML string, but it offers no attribute helpers and no compile-time checking, so `data-*` typos surface only in the browser.

For a component starting point rather than a dependency, DatastarUI (datastar-ui.com) is a copy-paste, shadcn-style component set built on Go, templ, and Datastar.

## When to consider an alternative SDK

The official SDK models each patch as a method call on one open connection, which fits the normal single-request handler. If the app needs to broadcast the same patch to many open connections, for example a live dashboard pushing to every viewer, the community package `github.com/larsartmann/go-datastar` models patches as standalone values with fan-out and ships a `datastartest` submodule for asserting on SSE responses. It requires a newer Go toolchain. Use the official SDK otherwise.
