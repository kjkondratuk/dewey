package tools

import (
	"context"
	"strings"
	"testing"
)

func TestBash_RefusesWhatASpecificToolCovers(t *testing.T) {
	cases := []struct {
		command  string
		wantTool string
	}{
		{"sed -i '' 's/old/new/' /tmp/main.go", "Edit"},
		{"cat /tmp/main.go", "Read"},
		{"grep -rn handler /tmp", "Grep"},
		{"find /tmp -name '*.go'", "Glob"},
		{"echo 'package main' > /tmp/main.go", "Write"},
		{"Glob **/*.go", "Glob"},
	}
	for _, c := range cases {
		t.Run(c.command, func(t *testing.T) {
			out, err := runShell(context.Background(), ShellCommandInput{Command: c.command})
			if err == nil {
				t.Fatalf("runShell(%q) ran; want refusal naming %s (output %q)", c.command, c.wantTool, out)
			}
			if !strings.Contains(err.Error(), c.wantTool) {
				t.Errorf("error = %q, want it to name %s", err.Error(), c.wantTool)
			}
		})
	}
}

func TestBash_StillRunsWhatOnlyTheShellCanDo(t *testing.T) {
	// The refusal must not cost the model an ability. Each of these has no
	// purpose-built equivalent, or needs something the equivalent lacks.
	cases := []string{
		"echo hello",
		"echo hello | tr a-z A-Z",
		"true && echo chained",
		"ls /tmp",
		"grep -A2 nothing /dev/null",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if _, err := runShell(context.Background(), ShellCommandInput{Command: c}); err != nil {
				t.Errorf("runShell(%q) error = %v, want it to run", c, err)
			}
		})
	}
}

func TestBash_RefusalExplainsItselfAndIsNotAnError(t *testing.T) {
	_, err := runShell(context.Background(), ShellCommandInput{Command: "sed -i '' s/a/b/ /tmp/x.go"})
	if err == nil {
		t.Fatal("want a refusal")
	}
	msg := err.Error()
	// The model has to be able to tell "use the other tool" apart from "this
	// capability is broken", or it retries the same call.
	for _, want := range []string{"Edit", "Bash is a fallback"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal = %q, want it to contain %q", msg, want)
		}
	}
}

func TestBash_ReportsExitCodeWithoutFailingTheCall(t *testing.T) {
	out, err := runShell(context.Background(), ShellCommandInput{Command: "exit 3"})
	if err != nil {
		t.Fatalf("runShell() error = %v, want a nonzero exit reported in the output", err)
	}
	if !strings.Contains(out, "exit code: 3") {
		t.Errorf("output = %q, want it to report exit code 3", out)
	}
}
