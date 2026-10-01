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

	systemPrompt, recorder, provider, _ := buildMemory(t.TempDir(), cfg)

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

	systemPrompt, recorder, provider, _ := buildMemory(t.TempDir(), cfg)

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

// Org-scoped memories must name the detected org exactly, and that value comes
// from the git remote rather than from config — so if it isn't reported there
// is nothing for the user to match against.
func TestBuildMemory_ReportsTheDetectedScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	_, _, _, notices := buildMemory(t.TempDir(), cfg)
	if len(notices) == 0 {
		t.Fatal("no notices; the user cannot tell what scope memories are matched against")
	}
	joined := strings.Join(notices, "\n")
	if !strings.Contains(joined, "Memory scope") {
		t.Errorf("notices = %v, want the detected scope reported", notices)
	}
}

func TestBuildMemory_DisabledReportsNoScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Memory.Enabled = false

	_, _, _, notices := buildMemory(t.TempDir(), cfg)
	if len(notices) != 0 {
		t.Errorf("notices = %v, want none when memory is disabled", notices)
	}
}
