# Single source of truth for checks. The git hooks (lefthook.yml) and CI
# (.github/workflows/ci.yml) both call these targets, so they cannot drift.
.PHONY: fmt fmt-check lint build vet test test-race tidy-check vuln secrets-staged hooks-install pre-commit pre-push ci

# Rewrite files with the golangci-lint v2 formatters (gofumpt + gci).
fmt:
	golangci-lint fmt

# Fail if any file would be rewritten by fmt. Used by CI, where nothing may be mutated.
fmt-check:
	golangci-lint fmt --diff

lint:
	golangci-lint config verify
	golangci-lint run

# Fails on any import path that does not resolve.
build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

# The race detector is the only guard for data races; static linting cannot see them.
test-race:
	go test -race ./...

# Fail if go.mod or go.sum are not tidy.
tidy-check:
	go mod tidy
	git diff --exit-code -- go.mod go.sum

vuln:
	govulncheck ./...

# Same invocation as gitleaks' own upstream pre-commit hook definition.
secrets-staged:
	gitleaks git --pre-commit --redact --staged --verbose --no-banner

# Wire the hooks in lefthook.yml into this clone.
hooks-install:
	lefthook install

# What the git hooks run, minus formatting side effects and staged-only scans.
pre-commit: fmt-check lint build
pre-push: test-race tidy-check vuln

# Everything CI runs. Secret scanning in CI covers full history via gitleaks-action.
ci: pre-commit vet pre-push
