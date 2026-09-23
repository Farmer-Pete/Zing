# Single source of truth for checks. The git hooks (lefthook.yml) and CI
# (.github/workflows/ci.yml) both call these targets, so they cannot drift.
.PHONY: fmt fmt-staged fmt-check lint build vet test test-race tidy-check vuln secrets-staged \
	hooks-install pre-commit pre-push ci templ-generate templ-check test-js

# Regenerates every *_templ.go from its .templ source, mutating files in
# place. A local convenience so build, fmt, and lint always run against
# current generated code; templ-check (below), not this target, is the gate.
templ-generate:
	go tool templ generate ./...

# Rewrite files with the golangci-lint v2 formatters (gofumpt + gci).
fmt: templ-generate
	golangci-lint fmt

# Format only the Go files staged in git. The pre-commit hook uses this so unrelated
# dirty files stay untouched; lefthook then re-stages what was rewritten.
fmt-staged:
	@git diff --cached --name-only --diff-filter=ACMR -- '*.go' | \
	while IFS= read -r f; do golangci-lint fmt "$$f" || exit 1; done

# Fail if any file would be rewritten by fmt. Used by CI, where nothing may be mutated.
fmt-check:
	golangci-lint fmt --diff

lint: templ-generate
	golangci-lint config verify
	golangci-lint run

# Fails on any import path that does not resolve.
build: templ-generate
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

# The race detector is the only guard for data races; static linting cannot see them.
test-race:
	go test -race ./...

# Fail if go.mod or go.sum are not tidy. Read-only: it never rewrites the module files,
# so it also catches a go.sum that go mod tidy would create but git has never tracked.
tidy-check:
	go mod tidy -diff

vuln:
	govulncheck ./...

# Same invocation as gitleaks' own upstream pre-commit hook definition.
secrets-staged:
	gitleaks git --pre-commit --redact --staged --verbose --no-banner

# Regenerates every *_templ.go in the worktree, then fails if that generation
# produced any change: a diff against a tracked *_templ.go, or an untracked
# one. This compares freshly generated output against the committed files
# directly, so a stale committed *_templ.go fails here the same way CI's
# clean checkout does (design section 6.2).
templ-check: templ-generate
	git diff --exit-code -- '*_templ.go'
	test -z "$$(git ls-files --others --exclude-standard -- '*_templ.go')"

# node --test over keyboard.mjs (design section 6.4). The static/ directory's
# package.json marks it and console.js as ES modules so Node loads them
# correctly.
test-js:
	node --test ./internal/console/static/console.test.js

# Wire the hooks in lefthook.yml into this clone.
hooks-install:
	lefthook install

# What the git hooks run, minus formatting side effects and staged-only scans.
pre-commit: templ-check fmt-check lint build
pre-push: test-race tidy-check vuln

# Everything CI runs. Secret scanning in CI covers full history via gitleaks-action.
ci: pre-commit vet pre-push test-js
