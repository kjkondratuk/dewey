# Design: CI/CD and release pipeline

Date: 2026-10-08

## Problem

dewey has no CI. Tests run when someone remembers to run them, and there is no
released artifact at all: using dewey means cloning the repo and running
`go run .`. The project now has a test suite worth protecting and a binary
worth distributing, and it ships to three operating systems on two
architectures each.

Two recent incidents also argue for automation. PR #8 was merged-ready while
being a net no-op against `main`, which a test gate would not have caught but a
release gate would have made visible. And the fix in PR #9 was a latent panic
reachable only through a specific cancel-after-compaction sequence — the kind
of regression a test gate on every pull request exists to catch.

## Goals

- Run the test suite on every pull request, and block on failure.
- Produce versioned releases automatically on merge to `main`, for
  `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`,
  `windows/amd64` and `windows/arm64`.
- Version with semver, computed from conventional commits, with no manual
  version bookkeeping.
- Aggregate release notes from conventional commit subjects, grouped by type.
- Publish build-numbered `alpha` and `beta` prereleases on manual demand, from
  any branch.
- Tag every released version, stable and prerelease alike.
- Stamp the version into the binary, so a downloaded `dewey` can say what it
  is.

## Non-goals

- **Package managers.** No Homebrew tap, Scoop bucket, AUR package, or Docker
  image. Archives and checksums attached to a GitHub release are the whole
  distribution story for now.
- **A committed CHANGELOG.md.** Release notes live on the GitHub release.
  A changelog file is a second copy that goes stale.
- **Signing or notarization.** Unsigned macOS binaries will warn on first run.
  Fixing that needs an Apple Developer certificate and is separable.
- **A universal macOS binary.** Two separate darwin archives, not a `lipo`
  fat binary.
- **Cross-OS test matrix.** See Risks: the suite does not pass on Windows
  today, and making it pass is its own piece of work.

## Decisions settled before design

1. **Auto-bump, skipping releases that ship nothing.** A merge whose commits
   are all non-releasable (`docs`, `chore`, `test`, `ci`, `build`, `style`,
   `refactor`) tests and exits without releasing. The alternative — a patch
   bump on every merge — mints versions whose binaries are byte-identical to
   the previous one.
2. **Per-base prerelease counters.** `v1.3.0-alpha.1`, `.2`, then
   `v1.3.0-beta.1`, restarting when `v1.3.0` ships. Chosen over
   `github.run_number`, which never collides but produces numbers that jump
   and never reset.
3. **First release is `v0.1.0`.** There is no tag to bump from, so the tool
   seeds rather than computes.
4. **Breaking changes always bump major, including from `v0.x`.** `feat!` on
   `v0.1.0` yields `v1.0.0`. Note that `semantic-release` and similar tools
   instead treat a `0.x` breaking change as a minor bump, on the theory that
   pre-1.0 carries no stability promise. The simpler rule is chosen here
   because it is what "semver" reads as, at the cost of reaching `v1.0.0` on
   the first `feat!`.

## Approach: own the version computation

The version is computed by a small Go tool in this repo rather than by a
third-party action or by `semantic-release`.

Both settled rules above are custom. No off-the-shelf action both skips
releases that ship nothing *and* maintains per-base prerelease counters that
reset, so adopting one means fighting its defaults in exactly the two places
this design cares about. A third-party action also puts an unaudited
dependency on the release path, which is the one path where a supply-chain
problem ships to users.

The decisive reason is testability. Release logic is otherwise verified by
pushing commits to `main` and watching what happens, which is slow and
destructive — a wrong bump leaves a published tag behind. A pure function
takes a tag list and a commit list and returns a version, so `feat!` → major
and `docs:` → no release are table tests that run in milliseconds.

GoReleaser already generates conventional-commit release notes natively, so
only version computation is needed, not a changelog engine.

## Architecture

### 1. Version computation (`internal/release`, new package)

The core is a pure function. No git, no network, no clock, no environment:

```go
// Channel is Stable, Alpha or Beta.
// Next returns the version to release and whether there is anything to
// release at all.
func Next(latest Version, commits []Commit, ch Channel, existing []Version) (Version, bool)
```

`Commit` carries only what the decision needs — the subject line and the body
— so the caller does the git work and the decision stays testable.

**Parsing.** A subject matches `type(scope)!: description`, where `(scope)`
and `!` are optional. A commit is breaking if `!` is present or if the body
contains a `BREAKING CHANGE:` or `BREAKING-CHANGE:` footer.

**Precedence**, highest wins across all commits in range:

| Signal | Bump |
| --- | --- |
| breaking (`!` or footer) | major |
| `feat` | minor |
| `fix`, `perf`, `revert` | patch |
| anything else | none |

`revert` is releasable because reverting a shipped feature changes shipped
behaviour. `refactor` is not, because by definition it does not.

**Range.** Commits since the most recent *stable* tag. Prerelease tags are
ignored when choosing the base, so publishing `v1.3.0-alpha.1` does not move
the base for `v1.3.0-alpha.2`.

**Seeding.** With no stable tag, the result is `v0.1.0` if any commit in the
range is releasable, and no release otherwise.

**Prereleases.** The base is computed exactly as a stable release would be,
then the counter is one more than the highest existing counter for that base
and channel, or `1` if there is none. If a manual prerelease is dispatched
when no commit is releasable, the base falls back to a patch bump rather than
failing: a manual dispatch is an explicit intent to ship something for
testing, and refusing it would defeat the purpose of the pipeline.

### 2. Git adapter (`cmd/relver`, new binary)

`cmd/` exists in the working tree but is empty and untracked, so it is a free
home. `relver` is deliberately thin: it shells out to `git tag --list` and
`git log`, calls `Next`, and writes `version=` and `release=` to
`$GITHUB_OUTPUT`. It holds no policy, which is why it needs no tests beyond
the parsing of git's output format.

### 3. Workflows (`.github/workflows/`)

| File | Trigger | Steps |
| --- | --- | --- |
| `test.yml` | `pull_request`; `workflow_call` | `gofmt -l` (fails if output is non-empty), `go vet ./...`, `go test -race ./...`, `goreleaser check` |
| `release.yml` | `push` to `main` | test via `workflow_call` → `relver` → if `release=true`: annotated tag, then GoReleaser |
| `prerelease.yml` | `workflow_dispatch`, input `channel` ∈ {`alpha`, `beta`} | test via `workflow_call` → `relver --channel=$channel` → annotated tag, then GoReleaser marking the release as a prerelease |

**Why `pull_request` and not also `push`.** Configuring both makes every push
to a branch with an open pull request run the suite twice. `pull_request`
already fires on `synchronize`, so pushing to a PR branch re-runs the tests —
which is the stated requirement — without the duplicate. A push to a branch
with no pull request open runs nothing, which is the deliberate trade.

**Why the release workflows call the tests rather than trusting them.**
`test.yml` and `release.yml` triggering independently on the same push would
run in parallel, and a release could publish while the suite was still running
or already red. Branch protection would mostly prevent that, but it is a
repository setting rather than a property of this repo's contents, so it can
be changed or forgotten without any file here changing. Making `test.yml` a
reusable workflow and having both release workflows `needs:` it puts the gate
in version control, and keeps the test steps defined once.

`actions/setup-go` uses `go-version-file: go.mod`, so CI tracks the Go version
without a second place to update it.

Both release workflows need `permissions: contents: write` to push tags and
create releases. `GITHUB_TOKEN` is sufficient; no personal access token is
required.

Both carry `concurrency: {group: release, cancel-in-progress: false}` so two
merges landing close together serialize rather than racing to create the same
tag.

### 4. `.goreleaser.yaml`

- `CGO_ENABLED=0`, verified to build clean today, so cross-compilation needs
  no C toolchains and the six targets are a plain matrix.
- `-ldflags` injecting version, commit and build date.
- `tar.gz` archives for linux and darwin, `zip` for windows.
- SHA256 checksums file.
- `changelog` grouping by conventional type — Features, Fixes, Performance —
  and excluding the non-releasable types, so release notes describe what
  changed for a user rather than listing CI tweaks.

### 5. Version in the binary (`main.go`)

```go
var version = "dev" // overridden via -ldflags at release time
```

plus a `--version` flag alongside the existing `-resume`, `-sessions` and
`-no-session`, matching the standard `flag` package usage already there. A
locally built binary reports `dev`, which is the honest answer.

## Data flow

**Pull request.** Push → `test.yml` → gofmt, vet, race tests, goreleaser
config check. No tags, no releases, no side effects.

**Merge to main.** Push to `main` → `relver` reads tags and `git log` since
the latest stable tag → computes bump → if nothing releasable, the job
succeeds and stops. Otherwise it creates an annotated tag and GoReleaser
builds six archives, writes checksums, generates grouped notes, and publishes
a GitHub release.

**Manual prerelease.** Dispatch with a channel → `relver --channel` computes
base and counter → annotated tag → GoReleaser publishes flagged as a
prerelease, so the repository's "latest release" keeps pointing at the newest
stable version.

## Failure modes and edge cases

- **No tags yet.** Seeds `v0.1.0`; covered by a test.
- **Nothing releasable on merge.** Job succeeds without a release. This is a
  success, not a skip-with-warning, so a docs merge shows green.
- **Non-conventional commit subjects.** Treated as non-releasable rather than
  as an error. A merge commit like `Merge pull request #9 from ...` therefore
  contributes nothing, which is correct — the commits it brings in are in
  range and are parsed individually. The repository currently squash-merges
  with conventional PR titles, so this is the uncommon path either way.
- **Two merges racing.** Serialized by the concurrency group.
- **HEAD is already tagged.** A re-run of a release workflow on an unchanged
  commit must not try to create a tag that exists. If the most recent stable
  tag already points at `HEAD`, there are no commits in range, nothing is
  releasable, and the job succeeds without releasing — so idempotency falls
  out of the range calculation rather than needing a special case. Tested.
- **A tag pushed by `GITHUB_TOKEN` does not trigger other workflows.** This is
  a deliberate GitHub behaviour to prevent recursion, and it does not affect
  this design: GoReleaser runs in the same job as the tag creation rather than
  on a tag-push trigger.
- **Prerelease dispatched from a branch whose commits are not on main.**
  Permitted and intended — that is the point of being able to ship a test
  build. The computed base may later differ from what `main` eventually
  releases, which is acceptable for a prerelease.
- **GoReleaser fails after the tag is pushed.** The tag remains and points at
  a version with no release. Recovery is deleting the tag and re-running;
  documented in the README rather than automated, since automatic tag deletion
  is more dangerous than the problem it solves.

## Testing

Table-driven tests on `release.Next`, covering: each bump type; breaking via
`!` and via footer; precedence when several types are present; the
non-releasable set returning no release; seeding with no tags; prerelease
counter starting at 1; counter incrementing past existing tags; counters
restarting after a stable release; and the patch fallback for a prerelease
with nothing releasable in range.

One further case: a stable tag pointing at `HEAD` yields no release, which is
what makes a re-run of the release workflow safe.

`goreleaser check` validates the release config in `test.yml`, so a malformed
`.goreleaser.yaml` fails on the pull request rather than at release time.

The workflows themselves are verified by running them: `test.yml` proves
itself on the pull request that adds it, and the release path is exercised by
the first merge to `main`.

## Phases

1. `internal/release` with its tests.
2. `cmd/relver`.
3. `--version` in `main.go`.
4. `.goreleaser.yaml`.
5. `test.yml`.
6. `release.yml` and `prerelease.yml`.
7. README section covering the release process and tag recovery.

## Risks

- **The suite does not pass on Windows.** `internal/tools/bash_test.go` shells
  out to `sh -c` and uses `/tmp` paths, and `internal/safety/shell.go` parses
  POSIX shell syntax throughout. Tests therefore run on `ubuntu-latest` only.
  A `windows/amd64` binary will be published without having been tested on
  Windows, and `PreferredTool` in particular encodes POSIX assumptions that may
  be wrong there. Follow-up work, tracked separately.
- **Every qualifying merge releases.** Versions will move faster than a
  human-paced release cadence, and a mistake reaches a published release
  rather than sitting on `main`. This is the explicit trade for not having to
  remember to cut releases.
- **`feat!` jumps straight to `v1.0.0`.** A consequence of decision 4, worth
  re-reading once before the first breaking change lands.
