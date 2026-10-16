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
