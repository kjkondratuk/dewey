package safety

import "testing"

// The reported bug: a local model rates `echo hello > f.txt` as kind=write
// risk=safe, and gating on the tier alone let it run with no prompt.
func TestRequiresApproval_WriteAlwaysPromptsRegardlessOfTier(t *testing.T) {
	cases := []struct {
		name string
		c    Classification
		want bool
	}{
		{"write rated safe", Classification{Kind: "write", Risk: Safe}, true},
		{"write rated moderate", Classification{Kind: "write", Risk: Moderate}, true},
		{"write rated dangerous", Classification{Kind: "write", Risk: Dangerous}, true},
		{"read rated safe", Classification{Kind: "read", Risk: Safe}, false},
		{"read rated moderate", Classification{Kind: "read", Risk: Moderate}, true},
		{"override, no kind, safe", Classification{Risk: Safe}, false},
		{"override, no kind, moderate", Classification{Risk: Moderate}, true},
	}
	for _, c := range cases {
		if got := c.c.RequiresApproval(); got != c.want {
			t.Errorf("%s: RequiresApproval() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestShellFloor_RecognizesWrites(t *testing.T) {
	writes := []string{
		`{"command":"echo hello > f.txt"}`,
		`{"command":"echo hello >> f.txt"}`,
		`{"command":"touch f.txt"}`,
		`{"command":"rm -rf dist"}`,
		`{"command":"mv a b"}`,
		`{"command":"mkdir -p x/y"}`,
		`{"command":"git commit -m wip"}`,
		`{"command":"npm install"}`,
		`{"command":"go build ./..."}`,
		`{"command":"chmod +x script.sh"}`,
		`{"command":"/bin/rm f.txt"}`,
		`{"command":"sudo systemctl restart nginx"}`,
		`{"command":"curl -o out.bin https://example.com"}`,
		// A mutating program later in the pipeline still counts.
		`{"command":"cat a.txt | tee b.txt"}`,
		`{"command":"ls && rm f.txt"}`,
		`{"command":"grep -rn x . ; touch done"}`,
		// Env assignments and wrappers must not hide the program.
		`{"command":"FOO=1 rm f.txt"}`,
		`{"command":"env rm f.txt"}`,
	}
	for _, args := range writes {
		if got := ShellFloor(args); got.Kind != "write" {
			t.Errorf("ShellFloor(%s) = %+v, want Kind=write", args, got)
		}
	}
}

func TestShellFloor_LeavesReadsAlone(t *testing.T) {
	reads := []string{
		`{"command":"ls"}`,
		`{"command":"ls -la"}`,
		`{"command":"cat go.mod"}`,
		`{"command":"grep -rn foo ."}`,
		`{"command":"git status"}`,  // git is in the list, but see the note below
		`{"command":"ls -la 2>&1"}`, // descriptor duplication is not a file write
		`{"command":"wc -l < f.txt"}`,
		`{"command":"ps aux | head"}`,
	}
	for _, args := range reads {
		got := ShellFloor(args)
		// git is intentionally floored as a write: distinguishing `git status`
		// from `git reset --hard` needs real subcommand parsing, and over-
		// prompting is the safe direction. Assert the rest stay clean.
		if args == `{"command":"git status"}` {
			if got.Kind != "write" {
				t.Errorf("ShellFloor(%s) = %+v, want git floored as write (documented over-prompt)", args, got)
			}
			continue
		}
		if got.Kind == "write" {
			t.Errorf("ShellFloor(%s) = %+v, want no floor — this would prompt on a read", args, got)
		}
	}
}

func TestShellFloor_IgnoresUnparseableArguments(t *testing.T) {
	for _, args := range []string{``, `not json`, `{}`, `{"command":""}`, `{"other":"x"}`} {
		if got := ShellFloor(args); got.Kind != "" {
			t.Errorf("ShellFloor(%q) = %+v, want the zero value", args, got)
		}
	}
}

// The floor must only ever raise. A model that escalates past it keeps its
// higher tier; a model that rates a write "safe" gets corrected.
func TestRaiseToFloor_OnlyRaises(t *testing.T) {
	write := `{"command":"rm -rf dist"}`

	got := raiseToFloor(Classification{Kind: "read", Risk: Safe}, "run_shell_command", write)
	if got.Kind != "write" || got.Risk != Moderate {
		t.Errorf("floor did not raise a read/safe write: %+v", got)
	}

	got = raiseToFloor(Classification{Kind: "write", Risk: Dangerous}, "run_shell_command", write)
	if got.Risk != Dangerous {
		t.Errorf("floor lowered the model's tier: %+v, want Dangerous preserved", got)
	}

	// Other tools are untouched: the heuristic is shell-specific.
	got = raiseToFloor(Classification{Kind: "read", Risk: Safe}, "remember", `{"text":"x"}`)
	if got.Kind != "read" || got.Risk != Safe {
		t.Errorf("floor applied to a non-shell tool: %+v", got)
	}
}
