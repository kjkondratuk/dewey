# CI/CD and Release Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Test every pull request, and publish versioned six-platform releases automatically on merge to `main`, with manual alpha/beta prereleases.

**Architecture:** Version selection is a pure Go function in `internal/release` that takes a tag list and a commit list and returns a version, so release logic is unit-tested rather than discovered by pushing tags. `cmd/relver` is a thin adapter that calls git and writes to `$GITHUB_OUTPUT`. GoReleaser builds and publishes; `test.yml` is a reusable workflow that both release workflows gate on.

**Tech Stack:** Go 1.26.2 (stdlib only for the new code), GoReleaser v2, GitHub Actions.

**Design:** `docs/superpowers/specs/2026-10-08-cicd-release-pipeline-design.md`

---

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/release/version.go` | The `Version` type: parse a tag, format one, order them |
| `internal/release/commit.go` | Conventional commit parsing and bump classification |
| `internal/release/next.go` | `Next()`: the whole release decision |
| `internal/release/version_test.go` | Tests for parse/format/order |
| `internal/release/commit_test.go` | Tests for classification and git log parsing |
| `internal/release/next_test.go` | Tests for the decision, stable and prerelease |
| `cmd/relver/main.go` | Git adapter: shell out, call `Next`, report to Actions |
| `main.go` | `--version` flag and the ldflags-injected build vars |
| `.goreleaser.yaml` | Six build targets, archives, checksums, grouped notes |
| `.github/workflows/test.yml` | Reusable: gofmt, vet, race tests, goreleaser check |
| `.github/workflows/release.yml` | Push to `main`: test, compute, tag, release |
| `.github/workflows/prerelease.yml` | Manual dispatch: same, on an alpha/beta channel |
| `README.md` | Release process and tag recovery |

Three small files in `internal/release` rather than one, split by responsibility: a version is a value type, a commit is parsing, and the decision is policy. The decision file is the one a reader will come to with a question, and it stays short enough to hold in view.

---

### Task 1: The Version type

**Files:**
- Create: `internal/release/version.go`
- Test: `internal/release/version_test.go`

- [ ] **Step 1: Write the failing test**

```go
package release

import "testing"

func TestParseVersion(t *testing.T) {
	tests := []struct {
		tag  string
		want Version
		ok   bool
	}{
		{"v1.2.3", Version{Major: 1, Minor: 2, Patch: 3}, true},
		{"v0.1.0", Version{Minor: 1}, true},
		{"v10.20.30", Version{Major: 10, Minor: 20, Patch: 30}, true},
		{"v1.3.0-alpha.2", Version{Major: 1, Minor: 3, Channel: Alpha, Counter: 2}, true},
		{"v1.3.0-beta.11", Version{Major: 1, Minor: 3, Channel: Beta, Counter: 11}, true},
		// A repository may carry tags that are not versions at all.
		{"nightly", Version{}, false},
		{"1.2.3", Version{}, false},
		{"v1.2", Version{}, false},
		{"v1.2.3-rc.1", Version{}, false},
		{"v1.2.3-alpha", Version{}, false},
		{"", Version{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			got, ok := ParseVersion(tt.tag)
			if ok != tt.ok {
				t.Fatalf("ParseVersion(%q) ok = %v, want %v", tt.tag, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("ParseVersion(%q) = %+v, want %+v", tt.tag, got, tt.want)
			}
		})
	}
}

func TestVersionString(t *testing.T) {
	tests := []struct {
		v    Version
		want string
	}{
		{Version{Major: 1, Minor: 2, Patch: 3}, "v1.2.3"},
		{Version{Minor: 1}, "v0.1.0"},
		{Version{Major: 1, Minor: 3, Channel: Alpha, Counter: 1}, "v1.3.0-alpha.1"},
		{Version{Major: 2, Channel: Beta, Counter: 7}, "v2.0.0-beta.7"},
	}
	for _, tt := range tests {
		if got := tt.v.String(); got != tt.want {
			t.Errorf("%+v String() = %q, want %q", tt.v, got, tt.want)
		}
	}
}

func TestVersionStringRoundTripsThroughParse(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "v1.2.3", "v1.3.0-alpha.2", "v2.0.0-beta.11"} {
		v, ok := ParseVersion(tag)
		if !ok {
			t.Fatalf("ParseVersion(%q) failed", tag)
		}
		if got := v.String(); got != tag {
			t.Errorf("round trip of %q gave %q", tag, got)
		}
	}
}

func TestVersionLess(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"patch", "v1.2.3", "v1.2.4", true},
		{"minor outranks patch", "v1.2.9", "v1.3.0", true},
		{"major outranks minor", "v1.9.0", "v2.0.0", true},
		{"equal is not less", "v1.2.3", "v1.2.3", false},
		{"descending is not less", "v1.2.4", "v1.2.3", false},
		// A prerelease precedes the release it leads to.
		{"prerelease before its base", "v1.3.0-alpha.1", "v1.3.0", true},
		{"base not before its prerelease", "v1.3.0", "v1.3.0-alpha.1", false},
		{"alpha before beta", "v1.3.0-alpha.9", "v1.3.0-beta.1", true},
		{"counter orders within a channel", "v1.3.0-alpha.2", "v1.3.0-alpha.10", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := ParseVersion(tt.a)
			b, _ := ParseVersion(tt.b)
			if got := a.Less(b); got != tt.want {
				t.Errorf("%s.Less(%s) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestVersionBaseAndIsZero(t *testing.T) {
	v, _ := ParseVersion("v1.3.0-alpha.2")
	if base := v.Base(); base != (Version{Major: 1, Minor: 3}) {
		t.Errorf("Base() = %+v, want v1.3.0", base)
	}
	if !(Version{}).IsZero() {
		t.Error("zero Version should report IsZero")
	}
	if v.IsZero() {
		t.Error("v1.3.0-alpha.2 should not report IsZero")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/release/ -run Version`
Expected: FAIL — `undefined: Version`, `undefined: ParseVersion`, `undefined: Alpha`

- [ ] **Step 3: Write minimal implementation**

```go
// Package release computes the version to release from conventional commits.
// Every decision lives in a pure function, so release logic is verified by
// tests rather than by pushing tags and seeing what happens -- a wrong bump
// leaves a published tag behind.
package release

import (
	"fmt"
	"regexp"
	"strconv"
)

// Channel distinguishes a stable release from a prerelease line.
type Channel string

const (
	Stable Channel = ""
	Alpha  Channel = "alpha"
	Beta   Channel = "beta"
)

// Version is a semver version, optionally on a prerelease channel. Counter is
// the per-base prerelease number and is zero for a stable version.
type Version struct {
	Major, Minor, Patch int
	Channel             Channel
	Counter             int
}

// versionPattern deliberately admits only the two prerelease channels this
// project publishes. A tag like v1.2.3-rc.1 is not a version dewey produced,
// so it must not be mistaken for one when continuing a counter.
var versionPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-(alpha|beta)\.(\d+))?$`)

// ParseVersion reads a tag like v1.2.3 or v1.3.0-alpha.2. The bool reports
// whether the tag was a version at all, since a repository may carry tags
// that are not.
func ParseVersion(tag string) (Version, bool) {
	m := versionPattern.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, false
	}
	v := Version{Major: atoi(m[1]), Minor: atoi(m[2]), Patch: atoi(m[3])}
	if m[4] != "" {
		v.Channel = Channel(m[4])
		v.Counter = atoi(m[5])
	}
	return v, true
}

// atoi is safe without an error check because versionPattern has already
// proven every captured group is a run of digits.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (v Version) String() string {
	if v.Channel == Stable {
		return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	}
	return fmt.Sprintf("v%d.%d.%d-%s.%d", v.Major, v.Minor, v.Patch, v.Channel, v.Counter)
}

// Base strips the prerelease channel, leaving the version it leads up to.
func (v Version) Base() Version {
	return Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch}
}

// IsZero reports the absence of a version, which is how "no tags yet"
// travels through this package. v0.0.0 is never produced, so the zero value
// is unambiguous.
func (v Version) IsZero() bool {
	return v == Version{}
}

// Less orders versions by semver precedence, with a prerelease sorting below
// the release it leads to.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	if v.Patch != o.Patch {
		return v.Patch < o.Patch
	}
	if (v.Channel == Stable) != (o.Channel == Stable) {
		return o.Channel == Stable
	}
	if v.Channel != o.Channel {
		return v.Channel < o.Channel
	}
	return v.Counter < o.Counter
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/release/ -run Version -v`
Expected: PASS for all four test functions

- [ ] **Step 5: Commit**

```bash
git add internal/release/version.go internal/release/version_test.go
git commit -m "feat(release): add the Version type"
```

---

### Task 2: Conventional commit classification

**Files:**
- Create: `internal/release/commit.go`
- Test: `internal/release/commit_test.go`

- [ ] **Step 1: Write the failing test**

```go
package release

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		c    Commit
		want Bump
	}{
		{"feat is minor", Commit{Subject: "feat: add Glob"}, BumpMinor},
		{"feat with scope", Commit{Subject: "feat(tools): add Glob"}, BumpMinor},
		{"fix is patch", Commit{Subject: "fix(agent): roll back"}, BumpPatch},
		{"perf is patch", Commit{Subject: "perf: cache the encoder"}, BumpPatch},
		// Undoing a shipped feature is a behaviour change.
		{"revert is patch", Commit{Subject: "revert: feat(tools) add Glob"}, BumpPatch},
		{"bang is major", Commit{Subject: "feat!: drop --no-session"}, BumpMajor},
		{"bang with scope is major", Commit{Subject: "feat(cli)!: drop --no-session"}, BumpMajor},
		{"bang on a fix is major", Commit{Subject: "fix!: change the default"}, BumpMajor},
		{
			"breaking footer is major",
			Commit{Subject: "feat: new config", Body: "BREAKING CHANGE: shell_rules moved"},
			BumpMajor,
		},
		{
			"hyphenated breaking footer is major",
			Commit{Subject: "feat: new config", Body: "BREAKING-CHANGE: shell_rules moved"},
			BumpMajor,
		},
		{
			"breaking footer after other body lines",
			Commit{Subject: "fix: tighten parsing", Body: "Some context.\n\nBREAKING CHANGE: nope"},
			BumpMajor,
		},
		// By definition a refactor changes nothing observable.
		{"refactor is none", Commit{Subject: "refactor: extract helper"}, BumpNone},
		{"docs is none", Commit{Subject: "docs: fix a typo"}, BumpNone},
		{"chore is none", Commit{Subject: "chore: bump deps"}, BumpNone},
		{"test is none", Commit{Subject: "test: cover the parser"}, BumpNone},
		{"ci is none", Commit{Subject: "ci: add a workflow"}, BumpNone},
		{"build is none", Commit{Subject: "build: tidy go.mod"}, BumpNone},
		{"style is none", Commit{Subject: "style: gofmt"}, BumpNone},
		// Not conventional at all, which is expected and not an error.
		{"merge commit is none", Commit{Subject: "Merge pull request #9 from x/y"}, BumpNone},
		{"prose subject is none", Commit{Subject: "fixed the thing"}, BumpNone},
		{"missing space after colon is none", Commit{Subject: "feat:no space"}, BumpNone},
		{"empty is none", Commit{}, BumpNone},
		// A breaking footer needs a conventional subject to count; the spec
		// defines the footer as part of that format.
		{
			"footer without a conventional subject is none",
			Commit{Subject: "random change", Body: "BREAKING CHANGE: x"},
			BumpNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.c); got != tt.want {
				t.Errorf("Classify(%+v) = %v, want %v", tt.c, got, tt.want)
			}
		})
	}
}

func TestHighestBump(t *testing.T) {
	tests := []struct {
		name    string
		commits []Commit
		want    Bump
	}{
		{"empty", nil, BumpNone},
		{
			"highest wins over order",
			[]Commit{{Subject: "fix: a"}, {Subject: "feat: b"}, {Subject: "docs: c"}},
			BumpMinor,
		},
		{
			"one breaking commit carries the set",
			[]Commit{{Subject: "docs: a"}, {Subject: "fix!: b"}, {Subject: "feat: c"}},
			BumpMajor,
		},
		{
			"all non-releasable",
			[]Commit{{Subject: "docs: a"}, {Subject: "chore: b"}, {Subject: "refactor: c"}},
			BumpNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HighestBump(tt.commits); got != tt.want {
				t.Errorf("HighestBump() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseLog(t *testing.T) {
	// The format is git log --format=%s%x00%b%x1e: NUL between subject and
	// body, record separator between commits.
	out := "feat: one\x00body one\x1e\nfix: two\x00\x1e\ndocs: three\x00multi\n\nline body\x1e\n"
	got := ParseLog(out)
	want := []Commit{
		{Subject: "feat: one", Body: "body one"},
		{Subject: "fix: two", Body: ""},
		{Subject: "docs: three", Body: "multi\n\nline body"},
	}
	if len(got) != len(want) {
		t.Fatalf("ParseLog() returned %d commits, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("commit %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseLogEmptyOutput(t *testing.T) {
	// No commits in range is the ordinary "nothing to release" case, not an
	// error, and must not yield a phantom empty commit.
	for _, out := range []string{"", "\n", "   \n"} {
		if got := ParseLog(out); len(got) != 0 {
			t.Errorf("ParseLog(%q) = %+v, want no commits", out, got)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/release/ -run 'Classify|HighestBump|ParseLog'`
Expected: FAIL — `undefined: Commit`, `undefined: Bump`, `undefined: Classify`

- [ ] **Step 3: Write minimal implementation**

```go
package release

import (
	"regexp"
	"strings"
)

// Commit is the part of a git commit that affects the version decision.
type Commit struct {
	Subject string
	Body    string
}

// Bump is the size of version increase a set of commits calls for. The order
// matters: HighestBump compares these numerically.
type Bump int

const (
	BumpNone Bump = iota
	BumpPatch
	BumpMinor
	BumpMajor
)

// conventionalPattern matches "type(scope)!: description". The trailing space
// is required by the conventional commits format and keeps a subject like
// "fix:no space" from being read as a release.
var conventionalPattern = regexp.MustCompile(`^([a-zA-Z]+)(\([^)]*\))?(!)?: `)

// patchTypes change shipped behaviour without adding to it. revert is here
// because undoing a shipped feature is a behaviour change; refactor is absent
// because by definition it changes nothing observable.
var patchTypes = map[string]bool{"fix": true, "perf": true, "revert": true}

// Classify reports the bump a single commit calls for. A subject that is not
// a conventional commit yields BumpNone rather than an error: merge commits
// and hand-written subjects are expected, and they simply release nothing.
func Classify(c Commit) Bump {
	m := conventionalPattern.FindStringSubmatch(c.Subject)
	if m == nil {
		return BumpNone
	}
	if m[3] == "!" || hasBreakingFooter(c.Body) {
		return BumpMajor
	}
	t := strings.ToLower(m[1])
	if t == "feat" {
		return BumpMinor
	}
	if patchTypes[t] {
		return BumpPatch
	}
	return BumpNone
}

// hasBreakingFooter looks for the conventional commits breaking marker. Both
// spellings are in the wild and the spec allows either.
func hasBreakingFooter(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "BREAKING CHANGE:") || strings.HasPrefix(t, "BREAKING-CHANGE:") {
			return true
		}
	}
	return false
}

// HighestBump returns the largest bump any commit in the set calls for, which
// is the bump the release takes.
func HighestBump(commits []Commit) Bump {
	highest := BumpNone
	for _, c := range commits {
		if b := Classify(c); b > highest {
			highest = b
		}
	}
	return highest
}

// ParseLog reads the output of
//
//	git log --format=%s%x00%b%x1e
//
// into commits. The NUL and record separators let a body contain blank lines
// and a subject contain anything, which a line-based format cannot survive.
func ParseLog(out string) []Commit {
	var commits []Commit
	for _, rec := range strings.Split(out, "\x1e") {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		subject, body, _ := strings.Cut(rec, "\x00")
		commits = append(commits, Commit{
			Subject: strings.TrimSpace(subject),
			Body:    strings.TrimSpace(body),
		})
	}
	return commits
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/release/ -run 'Classify|HighestBump|ParseLog' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/release/commit.go internal/release/commit_test.go
git commit -m "feat(release): classify conventional commits into version bumps"
```

---

### Task 3: The release decision

**Files:**
- Create: `internal/release/next.go`
- Test: `internal/release/next_test.go`

- [ ] **Step 1: Write the failing test**

```go
package release

import "testing"

// versions is a test helper: tags in, parsed versions out.
func versions(t *testing.T, tags ...string) []Version {
	t.Helper()
	var out []Version
	for _, tag := range tags {
		v, ok := ParseVersion(tag)
		if !ok {
			t.Fatalf("test setup: %q is not a version", tag)
		}
		out = append(out, v)
	}
	return out
}

func TestLatestStable(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want string // "" means the zero Version
	}{
		{"none", nil, ""},
		{"single", []string{"v1.2.3"}, "v1.2.3"},
		{"highest of several, order independent", []string{"v1.2.3", "v2.0.0", "v1.9.9"}, "v2.0.0"},
		// Prereleases must not move the base, or publishing an alpha would
		// change what the next alpha counts from.
		{"prereleases ignored", []string{"v1.2.3", "v1.3.0-alpha.1", "v1.3.0-beta.2"}, "v1.2.3"},
		{"only prereleases means none", []string{"v1.3.0-alpha.1"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LatestStable(versions(t, tt.tags...))
			if tt.want == "" {
				if !got.IsZero() {
					t.Errorf("LatestStable() = %s, want the zero Version", got)
				}
				return
			}
			if got.String() != tt.want {
				t.Errorf("LatestStable() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNextStable(t *testing.T) {
	tests := []struct {
		name    string
		latest  string // "" means no tags yet
		commits []Commit
		want    string // "" means no release
	}{
		{
			"feat bumps minor",
			"v1.2.3", []Commit{{Subject: "feat: add Glob"}}, "v1.3.0",
		},
		{
			"fix bumps patch",
			"v1.2.3", []Commit{{Subject: "fix: roll back"}}, "v1.2.4",
		},
		{
			"breaking bumps major and zeroes the rest",
			"v1.2.3", []Commit{{Subject: "feat!: drop a flag"}}, "v2.0.0",
		},
		{
			"minor zeroes patch",
			"v1.2.9", []Commit{{Subject: "feat: add Glob"}}, "v1.3.0",
		},
		{
			"nothing releasable means no release",
			"v1.2.3", []Commit{{Subject: "docs: typo"}, {Subject: "chore: deps"}}, "",
		},
		{
			"no commits means no release, which makes a re-run safe",
			"v1.2.3", nil, "",
		},
		{
			"first release seeds at v0.1.0 regardless of bump size",
			"", []Commit{{Subject: "feat!: everything"}}, "v0.1.0",
		},
		{
			"first release still needs something releasable",
			"", []Commit{{Subject: "docs: readme"}}, "",
		},
		{
			"breaking from v0.x goes to v1.0.0",
			"v0.1.0", []Commit{{Subject: "feat!: drop a flag"}}, "v1.0.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var latest Version
			if tt.latest != "" {
				latest = versions(t, tt.latest)[0]
			}
			got, ok := Next(latest, tt.commits, Stable, nil)
			if tt.want == "" {
				if ok {
					t.Errorf("Next() = %s, true; want no release", got)
				}
				return
			}
			if !ok {
				t.Fatalf("Next() returned no release, want %s", tt.want)
			}
			if got.String() != tt.want {
				t.Errorf("Next() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNextPrerelease(t *testing.T) {
	tests := []struct {
		name     string
		latest   string
		commits  []Commit
		channel  Channel
		existing []string
		want     string
	}{
		{
			"first alpha of a minor bump",
			"v1.2.3", []Commit{{Subject: "feat: add Glob"}}, Alpha,
			[]string{"v1.2.3"}, "v1.3.0-alpha.1",
		},
		{
			"counter continues past existing alphas",
			"v1.2.3", []Commit{{Subject: "feat: add Glob"}}, Alpha,
			[]string{"v1.2.3", "v1.3.0-alpha.1", "v1.3.0-alpha.2"}, "v1.3.0-alpha.3",
		},
		{
			"beta counts separately from alpha",
			"v1.2.3", []Commit{{Subject: "feat: add Glob"}}, Beta,
			[]string{"v1.2.3", "v1.3.0-alpha.1", "v1.3.0-alpha.2"}, "v1.3.0-beta.1",
		},
		{
			"counter restarts for a new base once the old one shipped",
			"v1.3.0", []Commit{{Subject: "feat: another"}}, Alpha,
			[]string{"v1.2.3", "v1.3.0-alpha.1", "v1.3.0-alpha.2", "v1.3.0"}, "v1.4.0-alpha.1",
		},
		{
			"a prerelease of a different base does not advance this counter",
			"v1.2.3", []Commit{{Subject: "feat: add Glob"}}, Alpha,
			[]string{"v1.2.3", "v9.0.0-alpha.7"}, "v1.3.0-alpha.1",
		},
		{
			"breaking commits give a major base",
			"v1.2.3", []Commit{{Subject: "feat!: drop a flag"}}, Beta,
			[]string{"v1.2.3"}, "v2.0.0-beta.1",
		},
		{
			// A manual dispatch is an explicit intent to ship something for
			// testing, so it falls back to a patch rather than refusing.
			"nothing releasable falls back to a patch base",
			"v1.2.3", []Commit{{Subject: "refactor: tidy"}}, Alpha,
			[]string{"v1.2.3"}, "v1.2.4-alpha.1",
		},
		{
			"no commits at all also falls back to a patch base",
			"v1.2.3", nil, Alpha,
			[]string{"v1.2.3"}, "v1.2.4-alpha.1",
		},
		{
			"first prerelease of an untagged repo seeds at the seed version",
			"", []Commit{{Subject: "feat: first"}}, Alpha,
			nil, "v0.1.0-alpha.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var latest Version
			if tt.latest != "" {
				latest = versions(t, tt.latest)[0]
			}
			got, ok := Next(latest, tt.commits, tt.channel, versions(t, tt.existing...))
			if !ok {
				t.Fatalf("Next() returned no release, want %s", tt.want)
			}
			if got.String() != tt.want {
				t.Errorf("Next() = %s, want %s", got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/release/ -run 'LatestStable|NextStable|NextPrerelease'`
Expected: FAIL — `undefined: LatestStable`, `undefined: Next`

- [ ] **Step 3: Write minimal implementation**

```go
package release

// seed is the first version a repository with no tags releases. There is no
// base to bump from, so this is chosen rather than computed, and the size of
// the bump the commits call for does not change it.
var seed = Version{Minor: 1}

// LatestStable returns the highest stable version in versions, or the zero
// Version when there is none. Prereleases are skipped so that publishing
// v1.3.0-alpha.1 does not move the base the next prerelease counts from.
func LatestStable(versions []Version) Version {
	var latest Version
	for _, v := range versions {
		if v.Channel != Stable {
			continue
		}
		if latest.IsZero() || latest.Less(v) {
			latest = v
		}
	}
	return latest
}

// Next returns the version to release and whether there is anything to
// release at all.
//
// latest is the newest stable version, or the zero Version for a repository
// with no tags. commits are those since latest. ch selects the channel.
// existing is every version tag in the repository, used only to continue a
// prerelease counter.
func Next(latest Version, commits []Commit, ch Channel, existing []Version) (Version, bool) {
	bump := HighestBump(commits)

	if ch == Stable {
		// A merge carrying only docs, chore, test, ci, build, style or
		// refactor commits ships nothing, so it must not mint a version whose
		// binary is identical to the last one. An empty range lands here too,
		// which is what makes re-running the release workflow on an already
		// tagged commit safe.
		if bump == BumpNone {
			return Version{}, false
		}
		if latest.IsZero() {
			return seed, true
		}
		return apply(latest, bump), true
	}

	// A manual prerelease is an explicit intent to ship something for
	// testing. Refusing it because the range holds only a refactor would
	// defeat the purpose of having the pipeline.
	if bump == BumpNone {
		bump = BumpPatch
	}
	base := seed
	if !latest.IsZero() {
		base = apply(latest, bump)
	}
	return Version{
		Major:   base.Major,
		Minor:   base.Minor,
		Patch:   base.Patch,
		Channel: ch,
		Counter: nextCounter(existing, base, ch),
	}, true
}

// apply increments v by b, zeroing the components below the one that moved.
func apply(v Version, b Bump) Version {
	switch b {
	case BumpMajor:
		return Version{Major: v.Major + 1}
	case BumpMinor:
		return Version{Major: v.Major, Minor: v.Minor + 1}
	default:
		return Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch + 1}
	}
}

// nextCounter continues the prerelease sequence for one base and channel.
// Counters are scoped to the base, so v1.3.0-alpha.2 is followed by
// v1.4.0-alpha.1 once v1.3.0 has shipped.
func nextCounter(existing []Version, base Version, ch Channel) int {
	highest := 0
	for _, v := range existing {
		if v.Channel == ch && v.Base() == base && v.Counter > highest {
			highest = v.Counter
		}
	}
	return highest + 1
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/release/ -count=1 -v`
Expected: PASS for every test in the package

- [ ] **Step 5: Commit**

```bash
git add internal/release/next.go internal/release/next_test.go
git commit -m "feat(release): decide the next version from tags and commits"
```

---

### Task 4: The relver command

**Files:**
- Create: `cmd/relver/main.go`

No test file. Every decision this binary could get wrong lives in
`internal/release` and is tested there; what remains is shelling out to git
and appending to a file, which a test would only restate.

- [ ] **Step 1: Write the implementation**

```go
// Command relver computes the version to release and reports it to GitHub
// Actions. The decisions live in internal/release; this binary only talks to
// git and to $GITHUB_OUTPUT, so it can be run by hand to see what a release
// would do.
//
//	go run ./cmd/relver                  # stable
//	go run ./cmd/relver --channel=alpha  # prerelease
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/kjkondratuk/dewey/internal/release"
)

func main() {
	channel := flag.String("channel", "", "prerelease channel: alpha or beta; empty for a stable release")
	flag.Parse()

	ch := release.Channel(*channel)
	switch ch {
	case release.Stable, release.Alpha, release.Beta:
	default:
		fatal(fmt.Errorf("unknown channel %q: want alpha, beta, or an empty value", *channel))
	}

	existing, err := existingVersions()
	if err != nil {
		fatal(err)
	}
	latest := release.LatestStable(existing)

	commits, err := commitsSince(latest)
	if err != nil {
		fatal(err)
	}

	version, ok := release.Next(latest, commits, ch, existing)
	if err := report(version, ok); err != nil {
		fatal(err)
	}
}

// existingVersions reads every tag in the repository that is a version,
// ignoring any that are not.
func existingVersions() ([]release.Version, error) {
	out, err := git("tag", "--list")
	if err != nil {
		return nil, err
	}
	var versions []release.Version
	for _, tag := range strings.Fields(out) {
		if v, ok := release.ParseVersion(tag); ok {
			versions = append(versions, v)
		}
	}
	return versions, nil
}

// commitsSince reads the commits after latest, or the whole history when the
// repository has no stable tag yet.
func commitsSince(latest release.Version) ([]release.Commit, error) {
	args := []string{"log", "--format=%s%x00%b%x1e"}
	if !latest.IsZero() {
		args = append(args, latest.String()+"..HEAD")
	}
	out, err := git(args...)
	if err != nil {
		return nil, err
	}
	return release.ParseLog(out), nil
}

// report writes the decision to stdout for the workflow log, and to
// $GITHUB_OUTPUT for later steps. Outside Actions that variable is unset and
// only stdout is written, which is what makes this runnable by hand.
func report(v release.Version, ok bool) error {
	lines := fmt.Sprintf("release=%t\n", ok)
	if ok {
		lines += fmt.Sprintf("version=%s\n", v)
	}
	fmt.Print(lines)

	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(lines); err != nil {
		return err
	}
	return f.Close()
}

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "relver:", err)
	os.Exit(1)
}
```

- [ ] **Step 2: Verify it builds and vets**

Run: `go build ./... && go vet ./...`
Expected: no output

Note: `report` calls `f.Close()` both in a defer and at the end, so the
second close returns an error the defer discards. This is intentional: the
explicit close surfaces a write-flush failure, which a deferred close would
swallow.

- [ ] **Step 3: Run it against this repository**

Run: `go run ./cmd/relver`
Expected: `release=true` and a `version=v0.1.0` line, because the repository
has no tags yet and its history contains `feat:` commits.

Run: `go run ./cmd/relver --channel=alpha`
Expected: `release=true` and `version=v0.1.0-alpha.1`

Run: `go run ./cmd/relver --channel=rc`
Expected: exit status 1 and `relver: unknown channel "rc": want alpha, beta, or an empty value`

- [ ] **Step 4: Commit**

```bash
git add cmd/relver/main.go
git commit -m "feat(release): add relver, the git adapter for version selection"
```

---

### Task 5: Report the version from the binary

**Files:**
- Modify: `main.go` (add build vars and a `--version` flag)
- Test: `main_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `main_test.go`:

```go
func TestVersionLine_ReportsInjectedBuildInfo(t *testing.T) {
	// Saved and restored because these are package-level vars that other
	// tests in this package may observe.
	origVersion, origCommit, origDate := version, commit, date
	t.Cleanup(func() { version, commit, date = origVersion, origCommit, origDate })

	version, commit, date = "v1.2.3", "abc1234", "2026-10-08T12:00:00Z"
	want := "dewey v1.2.3 (commit abc1234, built 2026-10-08T12:00:00Z)"
	if got := versionLine(); got != want {
		t.Errorf("versionLine() = %q, want %q", got, want)
	}
}

func TestVersionLine_DefaultsAreHonestForALocalBuild(t *testing.T) {
	// A binary built with plain `go build` has no ldflags, and must say so
	// rather than claim a version it does not have.
	if version != "dev" {
		t.Errorf("default version = %q, want %q", version, "dev")
	}
	if commit != "none" || date != "unknown" {
		t.Errorf("default commit/date = %q/%q, want %q/%q", commit, date, "none", "unknown")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestVersionLine`
Expected: FAIL — `undefined: version`, `undefined: versionLine`

- [ ] **Step 3: Write minimal implementation**

Add to `main.go`, immediately above `func main()`:

```go
// Build information, overridden at release time via -ldflags. The defaults
// are what a local `go build` honestly reports.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func versionLine() string {
	return fmt.Sprintf("dewey %s (commit %s, built %s)", version, commit, date)
}
```

In `func main()`, add a fourth flag after the existing three (currently
`main.go:36-38`):

```go
	showVersion := flag.Bool("version", false, "print version information and exit")
```

And immediately after `flag.Parse()` (currently `main.go:39`), before any
other work:

```go
	if *showVersion {
		fmt.Println(versionLine())
		return
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run TestVersionLine -v`
Expected: PASS

Then confirm the flag end to end:

Run: `go run . --version`
Expected: `dewey dev (commit none, built unknown)`

Then confirm ldflags injection, which is what the release actually relies on:

```bash
go build -ldflags "-X main.version=v9.9.9" -o /tmp/dewey-vtest . \
  && /tmp/dewey-vtest --version \
  && rm /tmp/dewey-vtest
```

Expected: `dewey v9.9.9 (commit none, built unknown)`

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat(cli): add --version reporting injected build information"
```

---

### Task 6: GoReleaser configuration

**Files:**
- Create: `.goreleaser.yaml`

- [ ] **Step 1: Write the configuration**

```yaml
# Six targets: linux, macOS and Windows on amd64 and arm64. darwin/arm64 is
# Apple silicon. CGO is off, so cross-compilation needs no C toolchains.
version: 2

builds:
  - id: dewey
    main: .
    binary: dewey
    env:
      - CGO_ENABLED=0
    goos:
      - linux
      - darwin
      - windows
    goarch:
      - amd64
      - arm64
    ldflags:
      - -s -w
      - -X main.version={{ .Version }}
      - -X main.commit={{ .Commit }}
      - -X main.date={{ .Date }}

archives:
  - id: dewey
    formats:
      - tar.gz
    # Windows users are far likelier to have a zip tool than a tar one.
    format_overrides:
      - goos: windows
        formats:
          - zip
    name_template: "dewey_{{ .Version }}_{{ .Os }}_{{ .Arch }}"
    files:
      - README.md

checksum:
  name_template: checksums.txt
  algorithm: sha256

changelog:
  use: git
  sort: asc
  groups:
    - title: Features
      regexp: '^feat(\([^)]*\))?!?: '
      order: 0
    - title: Fixes
      regexp: '^fix(\([^)]*\))?!?: '
      order: 1
    - title: Performance
      regexp: '^perf(\([^)]*\))?!?: '
      order: 2
    - title: Reverts
      regexp: '^revert(\([^)]*\))?!?: '
      order: 3
  filters:
    # The non-releasable types. These never trigger a release on their own, so
    # they would only pad the notes of a release they did not cause.
    exclude:
      - '^docs(\([^)]*\))?!?: '
      - '^chore(\([^)]*\))?!?: '
      - '^test(\([^)]*\))?!?: '
      - '^ci(\([^)]*\))?!?: '
      - '^build(\([^)]*\))?!?: '
      - '^style(\([^)]*\))?!?: '
      - '^refactor(\([^)]*\))?!?: '
      - '^Merge '

release:
  # A tag carrying -alpha. or -beta. is published as a prerelease, so the
  # repository's "latest release" keeps pointing at the newest stable version.
  prerelease: auto
```

- [ ] **Step 2: Verify the configuration is valid**

Install GoReleaser if it is not present (`brew install goreleaser`), then run:

Run: `goreleaser check`
Expected: `config is valid`

If `formats:` is rejected, the installed GoReleaser predates v2.5; either
upgrade it or change both `formats:` lists to the singular `format:` with a
scalar value, and pin the same major in the workflows in Task 8.

- [ ] **Step 3: Verify a real cross-platform build**

Run: `goreleaser build --snapshot --clean --single-target`
Expected: a `dist/` directory containing a `dewey` binary

Run: `goreleaser build --snapshot --clean && find dist -name dewey -o -name dewey.exe | sort`
Expected: six paths, one per target, including one `dewey.exe`

- [ ] **Step 4: Commit**

```bash
git add .goreleaser.yaml
git commit -m "build: add goreleaser configuration for six targets"
```

---

### Task 7: The reusable test workflow

**Files:**
- Create: `.github/workflows/test.yml`

- [ ] **Step 1: Write the workflow**

```yaml
# Triggered on pull requests, and callable by the release workflows so the
# test steps are defined once and a release cannot publish past a red suite.
#
# Deliberately not triggered on `push`: pull_request already fires on
# synchronize, so pushing to a branch with an open PR re-runs this. Adding
# push would run the whole suite twice for every such push.
name: test

on:
  pull_request:
  workflow_call:

permissions:
  contents: read

jobs:
  test:
    # ubuntu only. The suite does not pass on Windows yet: internal/tools
    # shells out to `sh -c` with /tmp paths, and internal/safety parses POSIX
    # shell syntax. Tracked as follow-up work in the design document.
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: gofmt
        run: |
          unformatted=$(gofmt -l .)
          if [ -n "$unformatted" ]; then
            echo "These files are not gofmt'd:"
            echo "$unformatted"
            exit 1
          fi

      - name: go vet
        run: go vet ./...

      - name: go test
        run: go test -race ./...

      - name: goreleaser check
        uses: goreleaser/goreleaser-action@v6
        with:
          version: '~> v2'
          args: check
```

- [ ] **Step 2: Verify the steps locally**

Run each step's command by hand, since a workflow cannot be run locally
without extra tooling:

```bash
test -z "$(gofmt -l .)" && echo "gofmt clean"
go vet ./...
go test -race ./...
goreleaser check
```

Expected: `gofmt clean`, no vet output, all packages `ok`, `config is valid`

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/test.yml
git commit -m "ci: run gofmt, vet, race tests and goreleaser check on pull requests"
```

---

### Task 8: The release and prerelease workflows

**Files:**
- Create: `.github/workflows/release.yml`
- Create: `.github/workflows/prerelease.yml`

- [ ] **Step 1: Write the release workflow**

`.github/workflows/release.yml`:

```yaml
name: release

on:
  push:
    branches: [main]

permissions:
  contents: write

# Two merges landing close together must not race to create the same tag.
# cancel-in-progress is false because a half-finished release is worse than a
# queued one.
concurrency:
  group: release
  cancel-in-progress: false

jobs:
  test:
    uses: ./.github/workflows/test.yml

  release:
    needs: test
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          # relver reads every tag and the full commit range since the last
          # one, neither of which a shallow clone has.
          fetch-depth: 0

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Compute the version
        id: version
        run: go run ./cmd/relver

      - name: Tag the release
        if: steps.version.outputs.release == 'true'
        run: |
          git config user.name "github-actions[bot]"
          git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
          git tag -a "${{ steps.version.outputs.version }}" \
            -m "${{ steps.version.outputs.version }}"
          git push origin "${{ steps.version.outputs.version }}"

      - name: Publish the release
        if: steps.version.outputs.release == 'true'
        uses: goreleaser/goreleaser-action@v6
        with:
          version: '~> v2'
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

- [ ] **Step 2: Write the prerelease workflow**

`.github/workflows/prerelease.yml`:

```yaml
# Manual only. Runs on whichever branch is selected when dispatched, so a
# build can be shipped from a feature branch for testing.
name: prerelease

on:
  workflow_dispatch:
    inputs:
      channel:
        description: Prerelease channel
        required: true
        type: choice
        options:
          - alpha
          - beta

permissions:
  contents: write

# Shares the release group, so a prerelease and a merge cannot tag at once.
concurrency:
  group: release
  cancel-in-progress: false

jobs:
  test:
    uses: ./.github/workflows/test.yml

  prerelease:
    needs: test
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Compute the version
        id: version
        run: go run ./cmd/relver --channel=${{ inputs.channel }}

      - name: Tag the prerelease
        if: steps.version.outputs.release == 'true'
        run: |
          git config user.name "github-actions[bot]"
          git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
          git tag -a "${{ steps.version.outputs.version }}" \
            -m "${{ steps.version.outputs.version }}"
          git push origin "${{ steps.version.outputs.version }}"

      - name: Publish the prerelease
        if: steps.version.outputs.release == 'true'
        uses: goreleaser/goreleaser-action@v6
        with:
          version: '~> v2'
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

- [ ] **Step 3: Verify the workflow files parse**

Run: `python3 -c "import yaml,sys; [yaml.safe_load(open(f)) for f in sys.argv[1:]]; print('yaml ok')" .github/workflows/*.yml`
Expected: `yaml ok`

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/release.yml .github/workflows/prerelease.yml
git commit -m "ci: release on merge to main, with manual alpha and beta prereleases"
```

---

### Task 9: Document the release process

**Files:**
- Modify: `README.md` (new section before `## Development`)

- [ ] **Step 1: Write the section**

Insert immediately before the `## Development` heading:

```markdown
## Releases

Merging to `main` releases. The version is computed from the conventional
commit subjects since the last tag: a `feat` bumps the minor, a `fix`, `perf`
or `revert` bumps the patch, and a `!` or a `BREAKING CHANGE:` footer bumps
the major. A merge whose commits are all `docs`, `chore`, `test`, `ci`,
`build`, `style` or `refactor` ships nothing, so it tests and stops without
minting a version.

To see what a release would do without releasing anything:

```bash
go run ./cmd/relver                  # the stable version a merge would cut
go run ./cmd/relver --channel=alpha  # the next alpha
```

Each release publishes six archives — linux, macOS and Windows on amd64 and
arm64 — with a `checksums.txt`, and release notes grouped by commit type.
`dewey --version` reports what a downloaded binary is; a locally built one
says `dev`.

Alpha and beta builds come from the **prerelease** workflow, run by hand from
the Actions tab against any branch. They are numbered per target version, so
`v1.3.0-alpha.1` and `.2` lead up to `v1.3.0` and the counter restarts for
`v1.4.0`. GitHub marks them as prereleases, so the latest-release link keeps
pointing at the newest stable build.

### When a release half-fails

The tag is pushed before the archives are built, so a GoReleaser failure
leaves a tag with no release attached. This is not cleaned up automatically,
because deleting tags from CI is more dangerous than the problem it solves.
Recover by hand:

```bash
git push --delete origin v1.3.0
git tag -d v1.3.0
```

Then re-run the workflow. A release workflow re-run on an already tagged
commit is harmless on its own: the commit range since the tag is empty, so
nothing is releasable and the job succeeds without doing anything.
```

- [ ] **Step 2: Verify the links and the table of contents**

The README opens with a `## Contents` list. Add `- [Releases](#releases)`
immediately before the `- [Development](#development)` entry.

Run: `grep -n "Releases" README.md`
Expected: two lines — the contents entry and the heading

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document the release process and tag recovery"
```

---

### Task 10: Open the pull request

- [ ] **Step 1: Run the full suite one more time**

Run: `gofmt -l . && go vet ./... && go test ./... -count=1 && goreleaser check`
Expected: no gofmt output, no vet output, every package `ok`, `config is valid`

- [ ] **Step 2: Push and open the pull request**

```bash
git push -u origin feat/cicd-release-pipeline
gh pr create --base main \
  --title "feat(ci): test pull requests and release six platforms from main" \
  --body "Implements docs/superpowers/specs/2026-10-08-cicd-release-pipeline-design.md

Version selection is a pure function in \`internal/release\`, so the release
rules are unit-tested rather than discovered by pushing tags. \`cmd/relver\`
only talks to git and \`\$GITHUB_OUTPUT\`.

- \`test.yml\` runs gofmt, vet, race tests and \`goreleaser check\` on every PR
- \`release.yml\` computes a version on merge to main, tags, and publishes six archives
- \`prerelease.yml\` does the same by hand on an alpha or beta channel
- A merge that ships nothing (docs, chore, test, ci, build, style, refactor) does not release

The \`test.yml\` run on this PR is the first proof it works. The release path
is first exercised by merging this PR, which should cut \`v0.1.0\`."
```

- [ ] **Step 3: Confirm the test workflow ran**

Run: `gh pr checks --watch`
Expected: the `test` check passes. This is the first live proof of the
pipeline; the release path is first exercised by merging.

---

## Notes for the implementer

**Work on a branch.** `git checkout -b feat/cicd-release-pipeline main` before
Task 1. Do not commit to `main`.

**The first merge cuts `v0.1.0`.** The repository has no tags, and its history
contains `feat:` commits, so the seed applies. Confirm with
`go run ./cmd/relver` after Task 4.

**Two known gaps, both deliberate and recorded in the design:**

1. A `windows/amd64` binary ships without having been tested on Windows.
   `safety.PreferredTool` in particular encodes POSIX assumptions.
2. Every qualifying merge releases, so versions move at merge pace and a
   mistake reaches a published release rather than sitting on `main`.
