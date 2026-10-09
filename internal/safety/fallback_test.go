package safety

import "testing"

func TestPreferredTool_ShellIsTheRightInstrument(t *testing.T) {
	// Each of these either has no purpose-built equivalent, or asks for a
	// capability the equivalent tool does not have. Blocking any of them
	// would cost the model a real ability.
	cases := []struct {
		command string
		why     string
	}{
		{"go test ./...", "running a build or test has no specific tool"},
		{"git status", "no specific tool wraps git"},
		{"ls -la", "Glob returns paths only; ls reports size, mode and owner"},
		{"mkdir -p internal/foo", "no specific tool creates directories"},
		{"cat main.go | wc -l", "a pipeline composes, which no specific tool does"},
		{"git log --oneline | grep fix", "grep here filters a stream, not files"},
		{"go build ./... && go vet ./...", "chaining is the shell's own job"},
		{"echo hello > /dev/null", "discarding output is not writing a file"},
		{"go test ./... > results.txt", "the point is running the tests; the redirect only captures output"},
		{"tail -f app.log", "Read cannot follow a growing file"},
		{"grep -A3 handler internal/app/app.go", "Grep does not support context lines"},
		{"grep -C2 handler app.go", "Grep does not support context lines"},
		{"sed 's/a/b/' main.go", "a transform to stdout does not modify the file"},
		{"cat", "reading stdin is not reading a file"},
		{"cat a.go b.go", "Read takes one file at a time"},
		{"find . -name '*.go' -exec gofmt -w {} ;", "Glob cannot run a command per match"},
		{"find . -type d -name vendor", "Glob returns files, not directories"},
		{"find . -newer go.mod", "Glob cannot filter by mtime comparison"},
	}
	for _, c := range cases {
		t.Run(c.command, func(t *testing.T) {
			if got := PreferredTool(c.command); got != nil {
				t.Errorf("PreferredTool(%q) = %+v, want nil (%s)", c.command, got, c.why)
			}
		})
	}
}

func TestPreferredTool_RedirectsToTheSpecificTool(t *testing.T) {
	cases := []struct {
		command string
		want    string
	}{
		{"sed -i '' 's/old/new/' main.go", "Edit"},
		{"sed -i.bak s/old/new/ main.go", "Edit"},
		{"sed --in-place s/a/b/ main.go", "Edit"},
		{"perl -i -pe 's/a/b/' main.go", "Edit"},
		{"env sed -i '' s/a/b/ main.go", "Edit"},
		{"patch -p1 < fix.diff", "Edit"},

		{"echo 'package main' > main.go", "Write"},
		{"printf 'hi' >> notes.md", "Write"},
		{"cat > notes.md", "Write"},

		{"cat main.go", "Read"},
		{"cat ./internal/app/app.go", "Read"},
		{"head -50 main.go", "Read"},
		{"tail -20 main.go", "Read"},
		{"less main.go", "Read"},

		{"find . -name '*.go'", "Glob"},
		{"find internal -iname 'app*'", "Glob"},
		{"find . -name '*.go' -type f", "Glob"},

		{"grep -rn handler internal/", "Grep"},
		{"grep pattern main.go", "Grep"},
		{"rg handler", "Grep"},
		{"grep -l TODO .", "Grep"},
	}
	for _, c := range cases {
		t.Run(c.command, func(t *testing.T) {
			got := PreferredTool(c.command)
			if got == nil {
				t.Fatalf("PreferredTool(%q) = nil, want %s", c.command, c.want)
			}
			if got.Tool != c.want {
				t.Errorf("PreferredTool(%q).Tool = %q, want %q", c.command, got.Tool, c.want)
			}
			if got.Reason == "" {
				t.Error("Reason is empty; the model needs to be told why")
			}
		})
	}
}

// A tool name used as a shell program is the model confusing the two calling
// conventions -- it happened in session 7a2a2d57 as `sh: Glob: command not
// found`, which reads as a missing capability rather than a misuse.
func TestPreferredTool_CatchesAToolNameUsedAsAProgram(t *testing.T) {
	for _, name := range []string{"Glob", "Grep", "Read", "Write", "Edit"} {
		t.Run(name, func(t *testing.T) {
			got := PreferredTool(name + " some-argument")
			if got == nil {
				t.Fatalf("PreferredTool(%q ...) = nil, want %s", name, name)
			}
			if got.Tool != name {
				t.Errorf("Tool = %q, want %q", got.Tool, name)
			}
		})
	}
}

func TestPreferredTool_EmptyCommandIsNotRedirected(t *testing.T) {
	for _, c := range []string{"", "   ", "\n"} {
		if got := PreferredTool(c); got != nil {
			t.Errorf("PreferredTool(%q) = %+v, want nil", c, got)
		}
	}
}
