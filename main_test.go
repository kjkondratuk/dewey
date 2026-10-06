package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/kjkondratuk/dewey/internal/config"
	"github.com/kjkondratuk/dewey/internal/files"
	"github.com/kjkondratuk/dewey/internal/session"
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

	names := toolNames(t, tools2.NewToolFactory(recorder, files.NewTracker()).DefaultInvokableToolset())
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

	names := toolNames(t, tools2.NewToolFactory(recorder, files.NewTracker()).DefaultInvokableToolset())
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
	cfg, _, err := config.Load(t.TempDir())
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
	cfg, _, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Memory.Enabled = false

	_, _, _, notices := buildMemory(t.TempDir(), cfg)
	if len(notices) != 0 {
		t.Errorf("notices = %v, want none when memory is disabled", notices)
	}
}

// sessionsDir derives the sessions directory from $HOME so it's testable the
// same way buildMemory is: no real home directory should be touched, and a
// missing one must be a reported notice rather than a crash.
func TestSessionsDir_ErrorsWhenHomeUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME is not consulted for os.UserHomeDir on windows")
	}
	t.Setenv("HOME", "")

	if _, err := sessionsDir(); err == nil {
		t.Error("sessionsDir() error = nil, want an error when $HOME is unset")
	}
}

func TestSessionsDir_UnderHomeDeweySessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir, err := sessionsDir()
	if err != nil {
		t.Fatalf("sessionsDir() error = %v", err)
	}
	want := filepath.Join(home, ".dewey", "sessions")
	if dir != want {
		t.Errorf("sessionsDir() = %q, want %q", dir, want)
	}
}

// A fresh run must get a session file with a populated header, and any write
// problem must come back as a notice rather than breaking the caller.
func TestBuildSession_CreatesHeaderAndSessionFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Config{
		Intermediate: config.ModelConfig{Model: "big-model"},
		Simple:       config.ModelConfig{Model: "small-model"},
	}

	store, notices := buildSession("/work/project", cfg, 8192)
	if store == nil {
		t.Fatal("buildSession() store = nil, want a store")
	}
	defer store.Close()

	if len(notices) != 0 {
		t.Errorf("notices = %v, want none for a clean session start", notices)
	}
	if store.ID() == "" {
		t.Error("store.ID() is empty")
	}
	if _, err := os.Stat(store.Path()); err != nil {
		t.Errorf("session file not created: %v", err)
	}

	msgs, header, err := session.Load(store.Path())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("msgs = %v, want none yet", msgs)
	}
	if header.IntermediateModel != "big-model" || header.SimpleModel != "small-model" {
		t.Errorf("header = %+v, want the configured models recorded", header)
	}
	if header.ContextWindow != 8192 {
		t.Errorf("header.ContextWindow = %d, want 8192", header.ContextWindow)
	}
}

// Session recording is instrumentation, not a hard requirement: an
// undiscoverable home directory must disable recording with a notice, never
// fail the run.
func TestBuildSession_DisabledWhenHomeDirUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME is not consulted for os.UserHomeDir on windows")
	}
	t.Setenv("HOME", "")

	store, notices := buildSession("/work/project", config.Config{}, 8192)
	if store != nil {
		t.Errorf("store = %#v, want nil when the home directory cannot be determined", store)
	}
	if len(notices) == 0 {
		t.Error("notices is empty, want the user told recording is disabled")
	}
}

// A sessions directory that cannot be created must disable recording rather
// than crash main().
func TestBuildSession_DisabledWhenSessionsDirUnwritable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Pre-create .dewey as a plain file, so MkdirAll(.dewey/sessions) fails.
	if err := os.WriteFile(filepath.Join(home, ".dewey"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, notices := buildSession("/work/project", config.Config{}, 8192)
	if store != nil {
		t.Errorf("store = %#v, want nil when the sessions directory can't be created", store)
	}
	if len(notices) == 0 {
		t.Error("notices is empty, want the user told recording is disabled")
	}
}

// --resume must load the prior transcript's history and start a new session
// file that records which session it continues, rather than appending to the
// original -- a resumed transcript interleaved with a fresh one would be
// harder to read, not easier.
func TestResumeSession_LoadsHistoryAndTracksResumedFrom(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".dewey", "sessions")

	original, err := session.NewStore(dir, session.Header{ID: "original-session-id"})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	original.Append(&schema.Message{Role: schema.User, Content: "earlier question"})
	original.Append(&schema.Message{Role: schema.Assistant, Content: "earlier answer"})
	if err := original.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	store, history, notices, err := resumeSession("/work/project", config.Config{}, 4096, "original-session-id")
	if err != nil {
		t.Fatalf("resumeSession() error = %v", err)
	}
	if len(notices) != 0 {
		t.Errorf("notices = %v, want none for a clean resume", notices)
	}
	if store == nil {
		t.Fatal("store = nil, want a new session file")
	}
	defer store.Close()

	if store.ID() == "original-session-id" {
		t.Error("resumed session reused the original's id; it must be a new file")
	}
	if len(history) != 2 || history[0].Content != "earlier question" || history[1].Content != "earlier answer" {
		t.Errorf("history = %+v, want the original transcript's two messages", history)
	}

	_, header, err := session.Load(store.Path())
	if err != nil {
		t.Fatalf("Load(new store) error = %v", err)
	}
	if header.ResumedFrom != "original-session-id" {
		t.Errorf("header.ResumedFrom = %q, want %q", header.ResumedFrom, "original-session-id")
	}
}

// A prefix is enough to resume, same as Resolve allows directly.
func TestResumeSession_AcceptsIDPrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".dewey", "sessions")

	original, err := session.NewStore(dir, session.Header{ID: "abcdef123456"})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	original.Close()

	store, _, _, err := resumeSession("/work/project", config.Config{}, 4096, "abcdef")
	if err != nil {
		t.Fatalf("resumeSession() error = %v", err)
	}
	defer store.Close()
}

// An id that matches nothing must fail loudly -- the user asked to resume a
// specific conversation, and silently starting a fresh one would hide that
// their request did not do what they asked.
func TestResumeSession_UnknownIDReturnsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if _, _, _, err := resumeSession("/work/project", config.Config{}, 4096, "nope-does-not-exist"); err == nil {
		t.Error("resumeSession() error = nil, want an error for an unknown id")
	}
}

// checkToolSupport is the gate main() applies to the intermediate tier before
// starting the TUI. A model that cannot call tools cannot do dewey's job --
// it will invent tool results instead -- so this is the definite-negative
// case that must become an error.
func TestCheckToolSupport_ErrorsWhenModelCannotCallTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"no-tools-model","capabilities":[]}]}`))
	}))
	defer srv.Close()

	err := checkToolSupport(context.Background(), config.ModelConfig{BaseURL: srv.URL + "/v1", Model: "no-tools-model"})
	if err == nil {
		t.Fatal("checkToolSupport() error = nil, want an error for a model that cannot call tools")
	}
	if !strings.Contains(err.Error(), "no-tools-model") {
		t.Errorf("error = %q, want it to name the model", err.Error())
	}
}

// Fail open: a provider that says nothing about capabilities, or doesn't
// list the model at all, must never block startup.
func TestCheckToolSupport_NilWhenUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"some-model","max_context_length":32768}]}`))
	}))
	defer srv.Close()

	err := checkToolSupport(context.Background(), config.ModelConfig{BaseURL: srv.URL + "/v1", Model: "some-model"})
	if err != nil {
		t.Errorf("checkToolSupport() error = %v, want nil when the provider says nothing about capabilities", err)
	}
}

// Fail open: an unreachable provider must never block startup either.
func TestCheckToolSupport_NilWhenProviderUnreachable(t *testing.T) {
	err := checkToolSupport(context.Background(), config.ModelConfig{BaseURL: "http://127.0.0.1:1", Model: "some-model"})
	if err != nil {
		t.Errorf("checkToolSupport() error = %v, want nil when the provider is unreachable", err)
	}
}

func TestCheckToolSupport_NilWhenModelSupportsTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"tool-model","capabilities":["tool_use"]}]}`))
	}))
	defer srv.Close()

	err := checkToolSupport(context.Background(), config.ModelConfig{BaseURL: srv.URL + "/v1", Model: "tool-model"})
	if err != nil {
		t.Errorf("checkToolSupport() error = %v, want nil when the model supports tools", err)
	}
}
