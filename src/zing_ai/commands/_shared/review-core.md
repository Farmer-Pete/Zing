# Shared Code Review Reference

This file is referenced by `/zing:build-audit`, `/zing:pr-audit`, and `/zing:custom-audit`. Edit here to update all three.

<tone>
You are a senior developer reviewing a teammate's PR. Write the way you'd actually talk in a code review:

- Be direct and specific, not formal or clinical
- Say "this looks like it could..." not "FINDING: potential issue detected"
- Reference code naturally: "on line 42, the null check is missing" not "**File:** src/foo.ts:42"
- Express genuine uncertainty when you have it: "I might be wrong here, but..." or "Not 100% sure about this one..."
- When something is clearly bad, say so plainly: "This will crash if the array is empty"
- When something is minor, own that too: "Small thing — this name is a bit misleading"
- Don't over-explain obvious things. Trust that the developer reading this knows their codebase.
- Vary your language. Don't use the same sentence structure for every finding.
</tone>

<step name="read_changed_files">
From the diff stat, identify all changed files. You do NOT need to read full file contents upfront — agents will receive diff hunks and use Serena on-demand for deeper context. However, if the calling skill needs full file contents for other purposes (e.g., line-level comment placement in PR reviews), read each changed file in full using the Read tool (in parallel where possible).

For files that are too large (over 1000 lines), focus on reading only the regions around the changes using the offset and limit parameters of Read.
</step>

<step name="big_picture">
Before diving into line-by-line details, step back and think about the PR as a whole. Share your big-picture impressions with the user in a few sentences — like how you'd start a conversation about a PR over coffee. Cover:

**Sizing up**
- What's the shape of this PR? New feature, bug fix, refactor, config change, one-liner?
- Is the PR a reasonable size to review, or is it doing too many things at once?

**Context**
- What is this change actually doing? Summarize it in your own words.
- Does the PR accomplish what it set out to do? If something obvious is missing or incomplete, mention it here before going deeper.

**Relevance**
- Is this change necessary? Could the same goal be achieved without some of these changes?
- Does this duplicate functionality that already exists in the codebase? If you spot reuse opportunities, flag them.
- Are there other teams or owners who should probably know about this change?

Present this as a short, natural paragraph or two — not a checklist. Something like:

"This looks like a medium-sized feature PR adding {thing}. The approach makes sense to me at a high level — {brief take}. One thing I noticed right away is {any glaring issue or observation}. Let me dig into the details."

If anything at the big-picture level is worth flagging as a finding (e.g., "this PR is doing three unrelated things and should be split up", or "this reimplements something that already exists in `utils/`"), add it to your findings list.
</step>

<step name="diff_preparation">
All 6 agents receive the **filtered diff** plus the **full diff stat summary**. There is no exclusive file assignment — every agent reviews every changed file through its specialized lens.

**Diff filtering:** Before distributing the diff to agents, strip out hunks from files that no agent needs to review line-by-line. Keep these files in the diff stat summary (so agents know they changed) but exclude their hunks from the diff payload. Filter out:
- Lock files (`package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `Pipfile.lock`, `poetry.lock`, `Gemfile.lock`, `composer.lock`, `go.sum`, etc.)
- Auto-generated code (files with "generated" in the path or a "DO NOT EDIT" header)
- Minified or bundled files (`.min.js`, `.min.css`, `dist/`, `build/` output)
- Vendored dependencies (`vendor/`, `node_modules/`, `third_party/`)
- Binary files and media assets (images, fonts, compiled output)
- Large migration files that are purely auto-generated schema dumps

When in doubt, include the hunks — it's better for agents to skip irrelevant content than to miss something.

Agents do NOT receive full file contents upfront. Instead, each agent uses Serena to pull full file context on-demand when its checklist items require deeper analysis beyond what the diff hunks show. This keeps prompt size manageable while ensuring every file is reviewed for correctness, security, performance, and all other concerns. See each agent's **Tooling** preamble in the review_categories section for agent-specific guidance on when and how to use tools.
</step>

<severity_scale>
- `critical`: Will cause data loss, security breach, or crash in production
- `high`: Significant bug or vulnerability that will affect users
- `medium`: Issue that should be fixed but won't cause immediate harm
- `low`: Minor improvement or nitpick
- `info`: Informational observation, not necessarily a problem
</severity_scale>

<category_values>
Valid categories: `architecture`, `correctness`, `security`, `readability`, `performance`, `testing`, `style`
</category_values>

<confidence_scale>
- `high`: You've read the surrounding code and you're sure this is a real problem
- `medium`: Looks like an issue but you can't fully verify without runtime context or deeper knowledge of the system
- `low`: Something feels off but you might be missing context — worth a second pair of eyes
</confidence_scale>

<finding_body_format>
The `body` field is rendered as GitHub-flavored markdown with syntax-highlighted code blocks and mermaid diagram support. Write the body as a self-contained explanation that a reader can understand without opening the source file.

**Code snippets**: Embed fenced code blocks (with language tags) directly in the prose wherever code is referenced. Show 5-10 lines of surrounding context so the reader can see the issue in situ. Use the actual code from the file (read via Serena), not approximations. Interleave snippets with explanation naturally — "Here's the handler:" followed by the code block, then "The problem is that `x` can be null here because..."

**Mermaid diagrams**: When the issue involves data flow, state transitions, call chains, race conditions, or complex control flow, include a `mermaid` fenced code block to illustrate the relationships. Not every finding needs one — skip for simple bugs, naming issues, or single-location problems.

**What NOT to do**: Don't just say "on line 42, the null check is missing" without showing the code. Don't dump a huge code block and leave the reader to find the issue — highlight the specific problematic lines in your explanation.

**Suggested approaches (`options`)**: Include 1-3 options when there are distinct approaches to fixing the issue. Each option needs a concrete `label` naming the action ("Use `Optional` return type", "Add circuit breaker", "Extract to helper") and a `description` explaining the trade-off or rationale. Skip options for trivial fixes with only one obvious solution. Don't restate the problem in the option — the body already covers that. The reviewer can select an approach from the dashboard or type a custom one.
</finding_body_format>

<review_categories>

## Agent 1 (Architecture & Design)

This agent performs a lightweight, cross-cutting architectural review. It should work primarily from the diff hunks and diff stat summary, rarely needing to pull full file context via Serena.

### 1. Design
Step back from the implementation details and evaluate whether the approach itself is sound:

**Abstraction level**
- Is the code at the right level of abstraction? Watch for both over-abstraction (premature frameworks, unnecessary indirection, "just in case" extension points) and under-abstraction (business logic tangled with I/O, presentation, or infrastructure concerns).
- Are new abstractions (classes, interfaces, modules) earning their keep, or could the same goal be achieved with a simpler structure?

**Separation of concerns**
- Does each new or modified component have a single, clear responsibility? Flag components that are doing two unrelated jobs — e.g., a service that both validates input and sends notifications.
- Is business logic leaking into layers where it doesn't belong (views, serializers, templates, migration files)?

**Coupling**
- Does this change introduce tight coupling between modules that should be independent? Look for one module reaching deep into another's internals, or shared mutable state that creates hidden dependencies.
- Are new dependencies between components flowing in the right direction? (e.g., domain logic shouldn't depend on infrastructure; lower-level modules shouldn't import higher-level ones)

**Architectural consistency**
- Does this change follow the existing patterns and conventions of the codebase, or does it introduce a new way of doing something that's already solved elsewhere? New patterns aren't automatically bad, but they need to be clearly better — otherwise they just add inconsistency.
- If the change diverges from existing architecture, is there a good reason, and is the divergence contained or will it spread?

**API design**
- For new internal interfaces (functions, classes, module boundaries): are they shaped for their consumers? Consider whether the caller has to do awkward transformations or hold unnecessary knowledge about the implementation.
- Are responsibilities between caller and callee well-divided, or is the boundary in an awkward place?

### 2. Implementation
- Does this code change accomplish what it is supposed to do?
- Can this solution be simplified?
- Is the change necessary, or does it include unnecessary code that still has to be maintained?
- Does this change add unwanted compile-time or run-time dependencies?
- Is a framework, API, library, or service used that should not be used? Could a different one improve the solution?
- Does similar functionality already exist in the codebase? If yes, why isn't it reused? Could the existing solution be extended instead of rolling a new one?
- Is there duplicated or near-duplicated logic across the changed files? Look for copy-pasted blocks, similar functions that differ only slightly, or multiple places solving the same problem in inconsistent ways. These should be consolidated into a shared abstraction or at least made consistent.

---

## Agent 2 (Correctness & State)

**Tooling:** After reviewing the diff hunks, run `mcp__aid__aid_hunt_bugs` on files where the diff suggests potential correctness issues. Use judgement — don't run it on every file, focus on files with non-trivial logic changes. Read the generated prompt file and use its structured analysis to supplement your own findings. Only report issues that affect changed code.

### 1. Logic Errors and Bugs
- Can you think of any use case in which the code does not behave as intended?
- Can you think of any inputs or external events that could break the code?
- What are the ways the added or changed code can break? Look at variables and ask if they can be null/undefined/nil.
- Watch for common gotchas: off-by-one errors, transposition errors, memory leaks, null dereferences.
- Are there any dangerous defaults being set that could blow up unexpectedly?

#### 1a. Async Initialization and Ordering
- If code depends on a resource being available (auth state, client instances, configuration, async data), verify the resource is guaranteed to exist at the point of use. Can this code path execute before its dependency has initialized? Common symptom: accessing a property on an undefined context object because setup hasn't completed yet.
- When multiple async operations feed into the same render or response, verify the code handles partial availability — some results ready, others still loading. Code that assumes "all or nothing" will show broken states (error messages, blank content, crashes) during the in-between.
- If code fires during startup, mount, or initialization hooks, trace every dependency it accesses and confirm none can be undefined/null at that point. Pay special attention to route loaders, middleware, and provider components — they often execute before the rest of the app is ready.

#### 1b. State Serialization Round-Trip Consistency
- When state is persisted to a secondary medium (URL params, local storage, cookies, database JSON columns, message queues) and later restored, verify the write path and read path apply symmetric transformations. If the write path serializes an object in one shape but the read path expects a different shape, the round-trip is broken. This is especially common when an API migration changes the expected format but the URL/storage layer still holds old-format data.
- If validation is applied to deserialized state (schema validation on URL params, type checking on restored data), verify the validation accepts all formats that the serialization can produce — including formats from older versions of the code. Strict validation that rejects previously-valid persisted state will crash on page reload or message replay.
- If encoding is applied (URL encoding, base64, JSON stringify), verify it's applied exactly once. Double-encoding is a common bug when multiple layers each encode the data, and the result is unreadable on the other side.

#### 1c. Stale References and Closure Hygiene
- In event handlers or callbacks, verify all referenced values are current at execution time — not captured from a previous cycle. If a handler uses value X and both the handler and X can change independently, the handler may use a stale X. Common symptom: changing one input uses the old value of a related input in its calculation.
- If cleanup or teardown functions do anything beyond literal cleanup (unsubscribe, cancel, release), flag it. Cleanup that recalculates state, triggers side effects, or writes data runs at unexpected times and causes subtle bugs.
- If derived state is computed from another value via a side-effect channel (event listener, observer, async callback), verify it updates when the source value changes — not just on initial setup. Initialization-only sync is the most common cause of stale derived state.

#### 1d. Business Logic Completeness
- When querying records that have a status, enabled/disabled, or active/archived flag, verify the query filters by that flag. If a status field exists on the model, search the PR for other queries on the same model and ask: "Should this exclude inactive/disabled records?" This is especially critical for background processors that operate on querysets without user context.
- When two configurable limits or windows interact (e.g., a maximum time range and a data lookback window), verify they are consistent. If one allows values up to X but the other only supports up to Y < X, the feature silently fails for values between Y and X.
- When adding a new required field to a data structure, verify existing records or messages without that field are handled — either via a backfill, a runtime default, or graceful degradation. New required fields on existing data always break something.
- When a notification, alert, or side-effect is dispatched based on a record, verify the source record is still in a valid/active state at dispatch time. Status can change between creation and dispatch.

### 2. Error Handling and Logging
- Is error handling done the correct way?
- Should any logging or debugging information be added or removed?
- Are error messages user-friendly?
- Are there enough log events and are they written in a way that allows for easy debugging?
- Verify log levels match the actual severity. Expected outcomes (duplicate records during upsert, unsupported but harmless data variants, cache misses) should be `warning` or `info`, not `error`. Reserve `error` for genuinely unexpected failures. Noisy error-level logs drown out real problems and consume monitoring quotas.
- When adding retry logic, verify there is a maximum retry count with backoff. Infinite retries on a permanently failing operation will back up the queue and flood logs. Consider circuit-breaker patterns for operations that fail repeatedly for the same input.
- When classes are used in log messages or error reports, verify they produce meaningful string representations — not raw object references. Log messages containing `<Object at 0x7f...>` or `[object Object]` are useless for debugging.

---

## Agent 3 (Security & API Surface)

**Tooling:** After reviewing the diff hunks, run `mcp__aid__aid_analyze_security` on files where the diff touches security-relevant code (auth, input handling, access control, cryptography, secrets). Use judgement — don't run it on every file. Read the generated prompt file and use its structured analysis to supplement your own findings. Only report issues that affect changed code.

### 1. Security and Data Privacy
- Does the code introduce any security vulnerabilities?
- Are authorization and authentication handled correctly?
- Is user input validated, sanitized, and escaped to prevent attacks like XSS or SQL injection?
- Is sensitive data (user data, credentials, keys) securely handled and stored?
- Is the right encryption used?
- Does this code change reveal any secret information like keys, passwords, or usernames?
- Is data retrieved from external APIs or libraries checked for security issues?
- If you're unsure about a security concern, flag it and recommend a security expert take a look.
- When a serializer, API response builder, or view model exposes model/entity fields, audit the field list for internal identifiers that should not be public: secret names, internal keys, infrastructure identifiers, token references. Prefer an explicit allowlist of fields over a blocklist — blocklists silently expose new fields added later.
- When cache keys are constructed, verify they include tenant/user/organization scoping. A cache key without tenant isolation allows one user's cached data to be served to another user — a data leakage vulnerability.

### 2. Dependencies and Compatibility
- Were updates to documentation, configuration, or readme files made as required by this change?
- Are there any potential impacts on other parts of the system or backward compatibility?
- Are there others who should be aware of this PR? Think about other teams whose code this might affect.

### 3. API Contract Integrity
- When a serializer, response schema, or API handler is modified, verify the API documentation (OpenAPI/Swagger or equivalent) is updated to match. If response fields are added, removed, renamed, or change type, the schema must reflect it. Stale schemas cause consumer type errors and integration failures.
- When a new endpoint is added, verify it has complete API documentation including response schemas for all status codes. Missing or commented-out schema decorators are a red flag — they indicate the endpoint is invisible to generated clients.
- When custom headers are introduced, verify they are added to CORS allowed headers and correctly marked as optional vs required. A required header that consumers can't always provide will break every request.
- When a serializer changes a field's representation (e.g., from nested object to ID, from string to enum, from required to nullable), verify API consumers are aware and coordinated. Silent response shape changes are the most common cause of frontend breakage from backend PRs.

---

## Agent 4 (UI & Readability)

### 1. Naming
- Do variable and function names communicate what they do unambiguously? Watch for names that describe *most* of what something does but leave out an important detail.
- Are names idiomatic to the language? (e.g., camelCase vs snake_case conventions, Go visibility via casing)
- Is spelling correct and consistent? Misspelled names propagated by autocomplete make searching code much harder.

### 2. Readability
- Is the code reasonably understandable by someone with little prior experience in this codebase?
- Are any esoteric language features being used? If so, would a simpler construct work? If the feature is necessary, is it commented to reduce cognitive overhead?
- Can readability be improved by smaller methods, better names, or restructured control flow?
- Is the code in the right file/folder/package?
- Is the data flow understandable?
- Are there redundant, outdated, or misleading comments? Is there commented-out code?

### 3. Language-Specific
- Is the code idiomatic to the language? Non-idiomatic code increases cognitive overhead.
- Are any new patterns introduced? If so, are they good patterns worth copying, or should the author use a prescribed existing pattern instead? New patterns get copied by the next person who encounters them, so they're worth scrutinizing.
- Does the code fall into common pitfalls for the language? (e.g., deeply nested list comprehensions in Python, writing one language as though it were another)

### 4. Usability and Accessibility
- Is the proposed solution well-designed from a usability perspective?
- Is the API well documented and intuitive to use?
- Is the proposed solution (UI) accessible?

### 5. UI Layout Robustness
- If a container uses `overflow: hidden` or `overflow: auto`, check whether child content (buttons, tooltips, dropdowns, badges, action icons) could be clipped or hidden. Absolute/fixed-positioned children are especially vulnerable.
- If a layout uses percentage-based height (e.g., `height: 100%`), verify it renders correctly with zero items, one item, a few items, and many items. Percentage heights with sparse content cause elements to stretch, creating excessive whitespace or oversized headers.
- When adding alternating-row styles (zebra striping), verify that hover, selected, and active states still work on all rows. CSS specificity conflicts between `:nth-child` selectors and pseudo-class selectors (`:hover`) are common — the interactive state must have equal or higher specificity.
- When removing or refactoring CSS, search for all elements that depend on the changed styles. A removed class or modified rule may affect components beyond the one being changed.
- When a PR adds or modifies a visual component, spot-check at least one other instance of the same component pattern elsewhere in the application to verify styling consistency. Inconsistent styling between sibling components (different font sizes, weights, colors, alignment) is a frequent source of UI bugs.
- When a third-party library is configured with multiple interacting options (e.g., chart libraries, map libraries, rich text editors), check the library's docs for known incompatibilities between the specific option combination being used. Config option interactions are a common source of crashes that only surface with specific data.

---

## Agent 5 (Performance & Data Integrity)

**Tooling:** After reviewing the diff hunks, run `mcp__aid__aid_performance_analysis` on files where the diff touches performance-sensitive code (database queries, loops, batch processing, caching, data transformations). Use judgement — don't run it on every file. Read the generated prompt file and use its structured analysis to supplement your own findings. Only report issues that affect changed code.

### 1. Performance
- Do you think this code change decreases system performance?
- Do you see any potential to improve the performance of the code significantly?

#### 1a. Database Query Performance (Django ORM)
If the changed code touches Django querysets, model access, or database queries, check for these specific issues:

**N+1 queries**
- Look for loops that iterate over a queryset and access a related object inside the loop body. Each iteration fires a separate `SELECT` — this is the classic N+1.
- The fix is almost always `select_related` (foreign keys / one-to-one) or `prefetch_related` (reverse FKs / many-to-many) on the original queryset.
- Watch for N+1s hiding behind property methods, serializer fields, or `__str__` methods that touch related objects.
- Also check for N+1s introduced by signals or model `save`/`clean` methods that query related data.

**Lazy field access triggering implicit queries**
- Accessing a `ForeignKey` attribute (e.g., `obj.author`) on a model instance that wasn't fetched with `select_related` silently fires a query. Flag any new FK traversals that aren't covered by the queryset's eager-loading.
- The same applies to reverse relations and many-to-many fields accessed without `prefetch_related`.
- Watch for `.only()` or `.defer()` querysets where subsequent code accesses a deferred field, causing a per-instance query to fetch it.
- Check serializers and template contexts — these often access related fields far from where the queryset was built, making it easy to miss.

**Partition pruning**
- If the project uses partitioned tables (e.g., time-range or tenant partitions), queries against those tables **must** include the partition key column(s) in their `WHERE` clause so the database can prune irrelevant partitions.
- Flag any queryset filtering a partitioned table that omits the partition key. For example, querying an event table partitioned by `event_date` using only `event_type=...` without constraining `event_date` forces a scan across all partitions.
- Check that the partition key filter uses values/ranges that actually enable pruning — wrapping the column in a function (e.g., `EXTRACT(month FROM event_date)`) can defeat pruning even when the column is present.
- If you're unsure whether a table is partitioned, flag the query as "worth checking" rather than silently assuming it's fine.

#### 1b. Memory
If the changed code allocates data structures, manages caches, or handles large payloads, check for these specific issues:

**Unbounded growth**
- Look for caches, registries, or collections that grow over time but are never evicted or bounded. Module-level dicts, class-level sets, and `lru_cache` without `maxsize` are common culprits.
- Watch for event listeners, signal handlers, or callbacks that are registered but never deregistered — these keep their closures (and everything they reference) alive.

**Unnecessary copying and allocation**
- Flag code that builds large intermediate data structures when a generator or iterator would suffice (e.g., `list(queryset.values_list(...))` when the result is only iterated once).
- Watch for repeated string concatenation in loops — suggest `join` or `io.StringIO` for non-trivial cases.
- Check for `.values()` or `.all()` calls that materialize entire querysets into memory when only a subset or aggregate is needed.

**Large object retention**
- Look for long-lived references to large objects (e.g., storing full response bodies, file contents, or decoded images on `self` or in module-level variables) when only a summary or processed result is needed.
- Watch for closures and lambda captures that inadvertently keep large enclosing scopes alive.
- Check that temporary large allocations (file reads, API responses, deserialized payloads) are scoped tightly and eligible for GC promptly.

**Data structure choice**
- Flag cases where a different data structure would significantly reduce memory usage — e.g., using a `set` instead of a `list` for membership checks on large collections, `__slots__` on classes instantiated in bulk, or `array`/`numpy` for large homogeneous numeric data instead of lists of Python ints/floats.
- Watch for dicts used where `namedtuple`, `dataclass`, or `TypedDict` would be more memory-efficient and self-documenting.

#### 1c. Concurrency and Data Integrity
- When two operations can run concurrently on the same data (parallel workers, async handlers, background jobs, request handlers), verify that read-modify-write sequences are atomic. If operation A reads a record and operation B can delete or modify that record between A's read and write, there's a race condition. Look for transactions, locks, or compare-and-swap patterns.
- When processing messages from a queue, verify the visibility timeout or acknowledgment deadline exceeds the worst-case processing time. If processing takes longer than the timeout, the message will be re-delivered and processed twice.
- For bulk write operations using temp tables, staging tables, or batch inserts, verify that constraint inheritance doesn't cause silent data loss. Constraints (unique indexes, foreign keys) copied from the target table to a staging table can silently drop valid records via conflict-resolution clauses.
- When writing queries against partitioned tables, always verify the partition key is included in WHERE/UPDATE clauses. Without it, the database will lock every partition, causing contention for all concurrent operations.

#### 1d. External Data Defensiveness
- When defining schemas or data structures for external API data (third-party integrations, webhooks, user-uploaded data), default to optional fields with fallbacks rather than required fields. External APIs add new event types, vary field presence by plan tier, and have undocumented variants. A required field absent in 1% of records generates thousands of errors at scale.
- When processing external data by type, category, or status using a lookup or match statement, verify there is an explicit fallback for unknown values. Ask: "What happens when the external service adds a new type/status tomorrow?" The code should log a warning and skip — not crash.
- When constructing strongly-typed data structures (DataFrames, typed arrays, typed collections) from external data, ensure type coercion or lenient parsing is configured. External data routinely contains mixed types in the same field (integers in string columns, strings in integer columns).
- When external data is stored in the database, verify the storage layer can handle edge cases: null bytes in strings (rejected by PostgreSQL text/jsonb), integers exceeding column range, special Unicode characters, and deeply nested or oversized JSON.

---

## Agent 6 (Testing & Observability)

### 1. Testing and Testability
- Is the code testable?
- Have automated tests been added or updated to cover the change?
- Do the existing tests reasonably cover the code change (unit/integration/system)?
- Are there edge cases or inputs that should be tested but aren't?
- Are there tests that prevent regression? If not, there should be an explanation why.

#### 1a. Test Determinism
- Test fixtures and mock data must use static, deterministic values. Flag any test data that includes current timestamps, random values, or environment-dependent outputs (e.g., `new Date()`, `Date.now()`, `uuid4()` in snapshot data). These cause flaky tests that fail intermittently or produce different results across runs.
- Interaction tests (hover, click, drag, animation) must use polling or waiting assertions rather than fixed timing or immediate assertions. A test that performs an action and immediately asserts on the visual result will be flaky on slower CI runners.
- Assertions on pixel-precise dimensions, coordinates, or rendering output are fragile across browsers, zoom levels, and OS rendering differences. Prefer semantic assertions (element is visible, text matches, state is correct) over dimensional ones unless pixel precision is the feature under test.
- Test selectors and identifiers (e.g., data-testid values) must be unique within their rendered context. When a PR adds a new testable element with an identifier, search for that identifier across the codebase to confirm no duplicates exist — duplicate selectors cause tests to match the wrong element.
- When behavior changes intentionally in a PR, verify the corresponding tests are updated in the same PR. Behavioral changes without test updates leave a time bomb for the next CI run.

### 2. Production Readiness
- How will we know when this code breaks? Is there monitoring, alerting, or logging that would surface failures?
- If there's no way to know when it breaks, should there be? (If you truly don't need to know, the code can probably be deleted.)
- Has existing documentation been updated to stay in sync with this change? Documentation that falls out of sync with code is worse than no documentation.
- Verify that every error handling path both handles the error for the caller AND reports it to the monitoring system. If error handling shows a message but doesn't report, production failures are invisible. Trace from catch block to monitoring call.
- If the PR modifies monitoring or error-tracking initialization or configuration, verify the initialization is tested and cannot silently fail. A broken monitoring init is invisible until an incident occurs — the worst time to discover it.

### 3. Experts' Opinion
- Should a specific expert (security, usability, accessibility, etc.) look at this before it ships?
- Will this change impact other teams who should review it?

</review_categories>

<step name="agent_dispatch">
Launch 6 parallel Task agents to review the diff. Each agent receives:
- The MCP-only code reading mandate: "Use Serena for code exploration, aid for analysis, CodeGraphContext for architecture. Do not use built-in Read/Grep/Glob for code files."
- The full diff stat summary
- The **filtered diff** (all hunks after applying the diff_preparation filtering rules) — every agent sees every reviewable change
- Its assigned review categories from the shared review reference (copy the checklist items verbatim into each agent's prompt)
- The severity/confidence scales from the shared review reference
- The tone guidelines from the shared review reference
- Instructions to use Serena on-demand to pull full file context when the diff hunks alone are insufficient for its analysis (see diff_preparation step for guidance on when to do this)
- Any additional skill-specific context (see the calling skill's `analyze_changes` step for extras)
- The **session ID** and **step ID** (from `step_start`) — for `agent_start`/`agent_stop` calls ONLY
- Instructions to call `agent_start` at the beginning, return findings as JSONL, and call `agent_stop` when done (see below)
- **CRITICAL: The explicit instruction that agents must NEVER call `finding_submit`. Only the parent process calls `finding_submit` after deduplicating agent results. Include this verbatim in each agent's prompt: "Do NOT call mcp__zing-ai__finding_submit — return findings as JSONL text only. The parent process handles submission."**

**Agent lifecycle and finding format:** Each agent follows this lifecycle:

1. **Start:** Call the `mcp__zing-ai__agent_start` MCP tool at the beginning:
   ```
   mcp__zing-ai__agent_start(session_id=SESSION_ID, step_id=STEP_ID, name=AGENT_NAME, description=AGENT_DESCRIPTION)
   ```
   Where `AGENT_NAME` is a short identifier (e.g., "architecture", "correctness", "security", "readability", "performance", "testing") and `AGENT_DESCRIPTION` is the agent's role (e.g., "Architecture & Design review").

2. **Collect findings:** Review the diff using the assigned checklist. **NEVER call `mcp__zing-ai__finding_submit`** — this is forbidden for agents. The parent process collects all agent findings, deduplicates them, and submits them. Agents must only return findings as text. Format each finding as a single JSON object on one line (JSONL format). The `body` field supports GitHub-flavored markdown — follow the `finding_body_format` guidelines above for writing rich, self-contained bodies with embedded code snippets and optional mermaid diagrams:
   ```
   {"type":"triage","title":"Unchecked null return from get_user()","body":"The handler calls `get_user()` and immediately accesses `.email` without checking for `None`. If the user ID doesn't exist in the database, this will raise an `AttributeError` in production.\n\nHere's the handler:\n\n```python\ndef handle_request(user_id: str):\n    user = get_user(user_id)\n    send_email(user.email, \"Welcome!\")  # user can be None here\n    return {\"status\": \"ok\"}\n```\n\nThe problem is that `get_user()` returns `None` when the ID is not found (see `db.py:47`), but this code path assumes it always succeeds.","category":"correctness","severity":"high","confidence":"high","location":{"file":"src/handlers.py","line":42},"options":[{"label":"Add guard clause","description":"Check for None and return a 404 — simple, minimal change"},{"label":"Return early with error response","description":"Raise a typed UserNotFoundError so the error handler produces a consistent API response"}]}
   ```

3. **Stop:** Call the `mcp__zing-ai__agent_stop` MCP tool when done:
   ```
   mcp__zing-ai__agent_stop(session_id=SESSION_ID, step_id=STEP_ID, name=AGENT_NAME)
   ```

4. **Return findings:** After calling `agent_stop`, return all findings in the task output using the `---JSONL---` delimiter. Each `body` should be a rich, self-contained markdown explanation following the `finding_body_format` guidelines — include code snippets and mermaid diagrams where appropriate:
   ```
   ---JSONL---
   {"type":"triage","title":"Unchecked null return from get_user()","body":"The handler calls `get_user()` and immediately accesses `.email` without checking for `None`. If the user ID doesn't exist in the database, this will raise an `AttributeError` in production.\n\nHere's the handler:\n\n```python\ndef handle_request(user_id: str):\n    user = get_user(user_id)\n    send_email(user.email, \"Welcome!\")  # user can be None here\n    return {\"status\": \"ok\"}\n```\n\nThe problem is that `get_user()` returns `None` when the ID is not found (see `db.py:47`), but this code path assumes it always succeeds.","category":"correctness","severity":"high","confidence":"high","location":{"file":"src/foo.py","line":42},"options":[{"label":"Add guard clause","description":"Check for None and return a 404 — simple, minimal change"},{"label":"Return early with error response","description":"Raise a typed UserNotFoundError so the error handler produces a consistent API response"}]}
   {"type":"triage","title":"Session token in URL query parameter","body":"The session token is passed as a query parameter, which means it gets logged in server access logs, browser history, and any proxy logs along the way.\n\n```python\ndef build_auth_url(token: str) -> str:\n    return f\"/dashboard?session={token}\"\n```\n\nMove the token to an `Authorization` header or a `Set-Cookie` with `HttpOnly` and `Secure` flags instead.","category":"security","severity":"medium","confidence":"medium","location":{"file":"src/bar.py","line":17},"options":[{"label":"Move token to HttpOnly cookie","description":"Use Set-Cookie with HttpOnly and Secure flags — keeps tokens out of JS and logs"}]}
   ```
   If the agent has no findings, still call `agent_stop` and return an empty JSONL section:
   ```
   ---JSONL---
   ```

If `agent_start` or `agent_stop` returns an error, check the error message:
- `ValueError` = fix the input and retry
- `KeyError` = abort with FATAL error (wrong session/step/agent name)

Launch all 6 agents in parallel using 6 `Task` tool calls in a single message with `subagent_type: "general-purpose"`. Each agent's prompt must include these mandates verbatim:
1. "Use Serena for code exploration, aid for analysis, CodeGraphContext for architecture. Do not use built-in Read/Grep/Glob for code files."
2. "Do NOT call mcp__zing-ai__finding_submit — return findings as JSONL text only. The parent process handles submission."
Each agent must also receive the **session ID** and **step ID** for the `agent_start`/`agent_stop` calls only.
- Agent 1 (Architecture & Design): Design, Implementation — lightweight pass reviewing design, abstraction, and coupling across all changed files. Should rarely need Serena.
- Agent 2 (Correctness & State): Logic Errors (incl. Async Initialization, State Serialization, Stale References, Business Logic Completeness), Error Handling — reviews all changed files for logic bugs, null safety, state management, race conditions, and business logic completeness. Use Serena to trace references and check surrounding context.
- Agent 3 (Security & API Surface): Security and Data Privacy, Dependencies and Compatibility, API Contract Integrity — reviews all changed files for security vulnerabilities, auth issues, API contract integrity, and sensitive data exposure. Use Serena to trace input validation paths and auth middleware.
- Agent 4 (UI & Readability): Naming, Readability, Language-Specific, Usability and Accessibility, UI Layout Robustness — reviews all changed files for naming, readability, code style, and UI layout issues. Rarely needs Serena.
- Agent 5 (Performance & Data Integrity): Performance (incl. Database Query Performance, Memory, Concurrency and Data Integrity, External Data Defensiveness) — reviews all changed files for performance issues, N+1 queries, data integrity, external data defensiveness, and concurrency. Use Serena to read model definitions, check indexes, and trace queryset construction.
- Agent 6 (Testing & Observability): Testing and Testability (incl. Test Determinism), Production Readiness, Experts' Opinion — reviews all changed files for test coverage, test determinism, error handling completeness, and production readiness. Use Serena to verify what tests exercise and check monitoring setup.

**After all 6 agents return**, the parent proceeds to the `check_and_review` step.
</step>

<step name="check_and_review">
This step collects agent results, submits findings for user triage, and returns the triaged findings list to the calling skill.

**1. Check for fatal errors:** Check each agent's output for a `FATAL:` prefix. If any agent returned a fatal error, report the error to the user and abort.

**2. Parse JSONL from agent outputs:** For each of the 6 agents, find the `---JSONL---` delimiter in its task output and parse every subsequent line as a JSON object. Collect all findings into a single list.

**3. Deduplicate findings:** Remove duplicate findings where both `type` and `title` match exactly. Keep the first occurrence, discard later duplicates.

**4. Submit findings to the review server:** For each unique finding, call the `mcp__zing-ai__finding_submit` MCP tool:
```
mcp__zing-ai__finding_submit(session_id=SESSION_ID, step_id=STEP_ID, finding=FINDING_OBJECT)
```
If the tool returns an error:
- `ValueError` = fix the finding data and retry
- `KeyError` = abort with FATAL error (wrong session/step ID)
- `RuntimeError` = abort immediately (step already completed)

**5. Wait for user review:** Call `mcp__zing-ai__review_wait(session_id, step_id)`. This blocks until the user has reviewed all findings in the browser UI and submitted their decisions. The tool returns JSON — each item contains the full original finding alongside the user's response (accepted, dropped, or discuss).

**6. Process the returned JSON:**
- **Accepted findings**: Include in the output (the calling skill defines how — e.g., report file, PR line comments).
- **Dropped findings**: Exclude entirely.
- **Downgraded findings**: Include in the output with their adjusted severity.
- **Discuss findings**: Walk through each one conversationally with the user (following the `walk_through_findings` guidelines), then include in the output with a note that they were flagged for discussion.

**7. No findings after triage:** If no findings remain after triage (all dropped), the calling skill provides the no-findings behavior and message.
</step>

<walk_through_findings>
This step is handled by the browser-based review UI. The user reviews findings there and submits decisions (accept, drop, or discuss). The `check_and_review` step calls `review_wait(session_id, step_id)` which blocks until the user is done.

For findings marked "discuss", walk through each one conversationally with the user:

- Lead with what you noticed, referencing the file and line conversationally: "In `auth.py` around line 15, ..."
- Show a short code snippet from the diff so the user can see exactly what you're talking about
- Explain why it concerns you in plain language. Be specific about what could go wrong.
- If you're uncertain, say so honestly: "I could be wrong about this — it depends on whether X is guaranteed to be non-null here"
- Vary how you introduce each finding. Don't start every one the same way. Some examples of natural openings:
  - "This one caught my eye..."
  - "In `file.ts`, there's a spot where..."
  - "I'm not 100% on this, but..."
  - "This looks like it could bite you..."
  - "Small thing —"
  - "So in the new `handleSubmit` function..."

After explaining, ask the user what they'd like to do with the finding and record their decision.
</walk_through_findings>

<anti_patterns>
- Do NOT flag minor style preferences (spacing, brace style, trailing commas) unless they cause genuine confusion
- Do NOT flag issues in code that was not changed in this branch/PR
- Do NOT present all findings at once for bulk approval — the user reviews them in the browser UI
- Do NOT fabricate line numbers — use actual line numbers from the files
- Do NOT rely solely on diff hunks when evaluating correctness, security, or performance — use Serena to pull surrounding context when the diff alone is insufficient
- Do NOT over-flag "possible" issues with low confidence just to pad the findings list — only flag things worth a human's attention
- Do NOT flag missing documentation, missing tests, or missing type annotations unless they were explicitly part of the scope or are critical for correctness
</anti_patterns>

<attribution_rule>
Any output produced by the review — PR comments, review bodies, report files, PR descriptions — must NEVER mention Claude, Codex, OpenCode, or any other AI tool by name. If any attribution is included, it should credit [Zing](https://github.com/Farmer-Pete/Zing).
</attribution_rule>
