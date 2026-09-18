# Cutting a release candidate

`cashctl` can ship a release candidate (e.g. `v0.3.0-rc.1`) ahead of a stable
version, using the same tag/CHANGELOG-driven `release.yml` workflow as a
normal release -- no separate RC workflow. `.goreleaser.yaml` already guards
the stable-only surfaces so an RC never gets mistaken for the latest stable
build. This mirrors the pattern set up for `ncli` -- see that repo's
`.goreleaser.yaml` if you want the original write-up.

## What's already in place

- `release.prerelease: auto` -- any tag with a semver prerelease suffix
  (`-rc.1`, `-beta.2`, ...) is published as a GitHub "Pre-release", never
  "Latest release".
- Docker: each arch's `dockers:` entry is split into a `-versioned` half
  (always pushed) and a `-latest` half (`skip_push: auto`, stable only).
  `docker_manifests:` is split the same way. An RC gets its own
  `ghcr.io/ohstr/cashctl:X.Y.Z-rc.N` image; `:latest` never moves for it.
- Homebrew: a second `cashctl-rc` formula exists alongside `cashctl`, with
  its `skip_upload` inverted (`'{{ if .Prerelease }}false{{ else }}true{{ end }}'`
  -- uploads only on a prerelease tag). `brew install/upgrade cashctl`
  never resolves to an RC; `brew install ohstr/tap/cashctl-rc` gets the
  latest one. Both formulas declare `conflicts_with` each other since they
  install the same `cashctl` binary name.

None of this needs touching to cut an RC -- it's already wired up.

## Steps

1. Set `CHANGELOG.md`'s top heading to the RC version, e.g.
   `## [0.3.0-rc.1]`, with real notes under it (the release workflow
   resolves the version and release notes straight from this heading).
2. Open a PR with that change (plus whatever's actually shipping), get CI
   green, merge to `main`.
3. Trigger the release: `gh workflow run release.yml -f version=0.3.0-rc.1`
   (or leave `version` blank -- it defaults to the top CHANGELOG heading).
4. Verify:
   - `gh api repos/ohstr/cashctl/releases/tags/v0.3.0-rc.1 --jq '{prerelease,draft}'`
     -- `prerelease: true`, `draft: false`.
   - `docker manifest inspect ghcr.io/ohstr/cashctl:0.3.0-rc.1` resolves;
     `ghcr.io/ohstr/cashctl:latest`'s digest is unchanged from before the run.
   - `gh api repos/ohstr/homebrew-tap/commits --jq '.[0].commit.message'`
     shows a `cashctl-rc` formula update, not `cashctl`.
   - Download the release archive and run `cashctl version` -- it should
     report `0.3.0-rc.1+<shortcommit>`.

## Promoting to stable

Cut the real release the same way, just without the `-rc.N` suffix (e.g.
`## [0.3.0]`, tag `v0.3.0`). `prerelease: auto` and both `skip_push`/
`skip_upload` guards flip automatically -- that run updates `:latest`, the
`cashctl` formula, and marks the GitHub Release as "Latest", with no config
changes needed.
