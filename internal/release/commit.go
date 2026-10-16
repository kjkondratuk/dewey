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
