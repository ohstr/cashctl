set shell := ["bash", "-uc"]

# cashctl is listed in the parent ohstr/go.work; GOWORK=off makes every
# recipe build against go.mod's pinned nmilat, as CI and releases do. For a
# local nmilat, use a private go.work and GOWORK=<path> (the integration
# suite reads CASHCTL_INTEGRATION_GOWORK for the binary it builds).
export GOWORK := "off"

# List all available recipes
default:
    @just --list --unsorted

# Build the cashctl binary into ./cashctl
build:
    go build -o cashctl .

# Run the test suite (skips the agent-eval suite; see below)
test:
    go test -short -race ./...

# Run the unit tests built with the crashpoints tag, which lets a test kill
# the process at a named point (internal/crashpoint); never in a release build
test-crashpoints:
    go test -short -race -tags crashpoints ./cmd/... ./internal/...

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
    cd .docs-kit && DOCS_CONTENT_DIR="{{justfile_directory()}}" DOCS_TITLE=cashctl DOCS_ACCENT_HUE=265 DOCS_FAVICON="{{justfile_directory()}}/docs/brand/favicon.svg" DOCS_LOGO="{{justfile_directory()}}/docs/brand/cashctl-mark-light.svg" DOCS_LOGO_DARK="{{justfile_directory()}}/docs/brand/cashctl-mark.svg" npm run dev
