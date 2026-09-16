set shell := ["bash", "-uc"]

# cashctl isn't a member of the shared ohstr/go.work workspace one directory
# up, so Go's auto-detection of that file (it walks up from cwd) would
# otherwise block every command below outright. This also makes go.mod's
# own `replace github.com/ohstr/nmilat => ...` (local dev, see go.mod's own
# comment on it) actually take effect instead of being silently shadowed by
# workspace mode. Harmless to leave once that replace is eventually dropped.
export GOWORK := "off"

# List all available recipes
default:
    @just --list --unsorted

# Build the cashctl binary into ./cashctl
build:
    go build -o cashctl .

# Run the test suite (skips the integration/agent-eval suites; see below)
test:
    go test -short -race ./...

# Run the integration suite against a real, already-running lokihub
# instance (needs integration/config.local.yaml — see integration/README.md);
# not run in CI.
test-integration:
    go test -tags integration -v ./integration/...

# Run the live-agent eval suite (real, billed Claude sessions against the
# published Docker image — see integration/agent-eval/README.md); not run
# in CI.
eval *args:
    ./integration/agent-eval/bin/run.sh {{args}}

# Vet all packages
vet:
    go vet ./...

# Tidy go.mod / go.sum
tidy:
    go mod tidy

# Run vet + test together (local pre-push check)
check: vet test

# Run cashctl straight from source
dev *args:
    go run . {{args}}

# Run the docs site locally with hot reload — README.md, AGENTS.md, and
# CHANGELOG.md changes sync automatically. http://localhost:4321/
#
# The docs site app itself lives in ohstr/docs-kit, shared across ohstr
# projects (see that repo's README) rather than vendored here — this just
# clones/updates a local cache of it under .docs-kit/ (gitignored) and runs
# it against this repo's own content.
docs-dev:
    [ -d .docs-kit/.git ] && git -C .docs-kit pull --quiet || git clone --quiet https://github.com/ohstr/docs-kit .docs-kit
    cd .docs-kit && [ -d node_modules ] || npm install
    cd .docs-kit && DOCS_CONTENT_DIR="{{justfile_directory()}}" DOCS_TITLE=cashctl DOCS_ACCENT_HUE=142 DOCS_FAVICON_GLYPH='$' npm run dev
