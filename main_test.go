package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/kjkondratuk/dewey/internal/config"
	tools2 "github.com/kjkondratuk/dewey/internal/tools"
)

func toolNames(t *testing.T, toolset []tool.InvokableTool) []string {
	t.Helper()
	names := make([]string, 0, len(toolset))
	for _, tl := range toolset {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		names = append(names, info.Name)
	}
	return names
}

func hasTool(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// Disabled memory must not reach the toolset. A nil *memory.Recorder assigned
// straight into the interface would make it non-nil, registering a remember
// tool that panics the first time the model calls it.
func TestBuildMemory_DisabledOmitsRememberTool(t *testing.T) {
	cfg := config.Config{Memory: config.MemoryConfig{Enabled: false}}

	systemPrompt, recorder, provider := buildMemory(t.TempDir(), cfg)

	if recorder != nil {
		t.Errorf("recorder = %#v, want a nil interface so the remember tool is omitted", recorder)
	}
	if provider != nil {
		t.Errorf("provider = %#v, want nil when memory is disabled", provider)
	}

	names := toolNames(t, tools2.NewToolFactory(recorder).DefaultInvokableToolset())
	if hasTool(names, "remember") {
		t.Errorf("toolset = %v, want no remember tool with memory disabled", names)
	}

	msg, err := systemPrompt.System(context.Background())
	if err != nil {
		t.Fatalf("System() error = %v", err)
	}
	if !strings.Contains(msg.Content, "dewey") {
		t.Errorf("Content = %q, want the base prompt even with memory disabled", msg.Content)
	}
}

func TestBuildMemory_EnabledRegistersRememberTool(t *testing.T) {
	cfg := config.Config{Memory: config.MemoryConfig{Enabled: true, Dir: t.TempDir(), MaxTokens: 4000}}

	systemPrompt, recorder, provider := buildMemory(t.TempDir(), cfg)

	if recorder == nil {
		t.Fatal("recorder = nil, want a recorder so memories can be saved")
	}
	if provider == nil {
		t.Fatal("provider = nil, want a provider so startup notices can be collected")
	}
	if systemPrompt == nil {
		t.Fatal("systemPrompt = nil, want the memory-backed prompt")
	}

	names := toolNames(t, tools2.NewToolFactory(recorder).DefaultInvokableToolset())
	if !hasTool(names, "remember") {
		t.Errorf("toolset = %v, want a remember tool with memory enabled", names)
	}
}
