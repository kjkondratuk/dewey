package tools

import (
	"context"
	"strings"
	"testing"
)

func TestDefaultInvokableToolset_ReturnsOneShellTool(t *testing.T) {
	toolset := NewToolFactory().DefaultInvokableToolset()
	if len(toolset) != 1 {
		t.Fatalf("DefaultInvokableToolset() returned %d tools, want 1", len(toolset))
	}

	info, err := toolset[0].Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "run_shell_command" {
		t.Errorf("tool name = %q, want %q", info.Name, "run_shell_command")
	}
}

func TestDefaultInvokableToolset_ShellCommandSuccess(t *testing.T) {
	toolset := NewToolFactory().DefaultInvokableToolset()

	result, err := toolset[0].InvokableRun(context.Background(), `{"command":"echo hello"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if !strings.Contains(result, "hello") {
		t.Errorf("result = %q, want it to contain %q", result, "hello")
	}
	if !strings.Contains(result, "exit code: 0") {
		t.Errorf("result = %q, want it to contain %q", result, "exit code: 0")
	}
}

func TestDefaultInvokableToolset_ShellCommandNonZeroExitIsNotAToolError(t *testing.T) {
	toolset := NewToolFactory().DefaultInvokableToolset()

	result, err := toolset[0].InvokableRun(context.Background(), `{"command":"exit 3"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil (non-zero exit is not a tool error)", err)
	}
	if !strings.Contains(result, "exit code: 3") {
		t.Errorf("result = %q, want it to contain %q", result, "exit code: 3")
	}
}

func TestDefaultInvokableToolset_ShellCommandCapturesStderr(t *testing.T) {
	toolset := NewToolFactory().DefaultInvokableToolset()

	result, err := toolset[0].InvokableRun(context.Background(), `{"command":"echo err-output 1>&2"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if !strings.Contains(result, "err-output") {
		t.Errorf("result = %q, want it to contain stderr output %q", result, "err-output")
	}
}

func TestDefaultInvokableToolset_ShellCommandSupportsPipelines(t *testing.T) {
	toolset := NewToolFactory().DefaultInvokableToolset()

	result, err := toolset[0].InvokableRun(context.Background(), `{"command":"printf 'a\\nb\\nc\\n' | grep b"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if !strings.Contains(result, "b") {
		t.Errorf("result = %q, want it to contain %q", result, "b")
	}
}

func TestDefaultInvokableToolset_ShellCommandSpawnFailureIsAToolError(t *testing.T) {
	toolset := NewToolFactory().DefaultInvokableToolset()

	_, err := toolset[0].InvokableRun(context.Background(), `{"command":123}`)
	if err == nil {
		t.Fatal("InvokableRun() error = nil, want an error for malformed arguments")
	}
}
