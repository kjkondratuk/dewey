package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, dir, contents string) {
	t.Helper()
	dewey := filepath.Join(dir, ".dewey")
	if err := os.MkdirAll(dewey, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dewey, "config.yaml"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_DefaultsWhenNoFilesExist(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)

	cfg, _, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantIntermediate := ModelConfig{BaseURL: defaultBaseURL, APIKey: defaultAPIKey, Model: defaultIntermediateModel}
	if cfg.Intermediate != wantIntermediate {
		t.Errorf("Intermediate = %+v, want %+v", cfg.Intermediate, wantIntermediate)
	}
	if cfg.Simple != wantIntermediate {
		t.Errorf("Simple = %+v, want fallback to intermediate %+v", cfg.Simple, wantIntermediate)
	}
	// The remember tool writes by definition; the built-in rules say so.
	if !contains(cfg.ShellRules.AlwaysWriteTools, "remember") {
		t.Errorf("AlwaysWriteTools = %v, want it to include remember", cfg.ShellRules.AlwaysWriteTools)
	}
	if len(cfg.ShellRules.AlwaysWrite) == 0 {
		t.Error("AlwaysWrite is empty; the built-in rules did not load")
	}
}

func TestLoad_ProjectModelTierReplacesGlobalTier(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)

	writeConfig(t, home, "models:\n  simple:\n    base_url: \"http://global:1234/v1\"\n    api_key: \"global-key\"\n    model: \"global-model\"\n")
	writeConfig(t, project, "models:\n  simple:\n    model: \"project-model\"\n")

	cfg, _, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := ModelConfig{BaseURL: defaultBaseURL, APIKey: defaultAPIKey, Model: "project-model"}
	if cfg.Simple != want {
		t.Errorf("Simple = %+v, want %+v (project tier replaces global tier wholesale, defaults fill the gaps)", cfg.Simple, want)
	}
}

func TestLoad_MalformedYAMLIsAnError(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)

	writeConfig(t, project, "models: [this is not valid: yaml structure")

	if _, _, err := Load(project); err == nil {
		t.Fatal("expected an error for malformed YAML")
	}
}

func TestLoad_MemoryDefaults(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)

	cfg, _, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Memory.Enabled {
		t.Error("Memory.Enabled = false, want true by default")
	}
	if cfg.Memory.MaxTokens != defaultMemoryMaxTokens {
		t.Errorf("Memory.MaxTokens = %d, want %d", cfg.Memory.MaxTokens, defaultMemoryMaxTokens)
	}
	if cfg.Memory.Dir == "" {
		t.Error("Memory.Dir is empty, want a default under the home directory")
	}
}

func TestLoad_MemoryCanBeDisabled(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, project, "memory:\n  enabled: false\n")

	cfg, _, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Memory.Enabled {
		t.Error("Memory.Enabled = true, want false when explicitly disabled")
	}
}

// A memory section that only moves the directory must not also switch the
// feature off: `enabled` absent and `enabled: false` have to stay distinct, or
// configuring the store silently uninstalls it.
func TestLoad_MemorySectionWithoutEnabledKeepsItOn(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, project, "memory:\n  dir: /custom/memories\n  max_tokens: 1234\n")

	cfg, _, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Memory.Enabled {
		t.Error("Memory.Enabled = false, want it left on by a section that never mentions enabled")
	}
	if cfg.Memory.Dir != "/custom/memories" {
		t.Errorf("Memory.Dir = %q, want /custom/memories", cfg.Memory.Dir)
	}
	if cfg.Memory.MaxTokens != 1234 {
		t.Errorf("Memory.MaxTokens = %d, want 1234", cfg.Memory.MaxTokens)
	}
}

func TestLoad_MemoryProjectOverridesGlobal(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, home, "memory:\n  enabled: false\n  dir: /global/memories\n  max_tokens: 100\n")
	writeConfig(t, project, "memory:\n  enabled: true\n  max_tokens: 200\n")

	cfg, _, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Memory.Enabled {
		t.Error("Memory.Enabled = false, want the project's true to win")
	}
	if cfg.Memory.Dir != "/global/memories" {
		t.Errorf("Memory.Dir = %q, want the global value to survive a project section that omits it", cfg.Memory.Dir)
	}
	if cfg.Memory.MaxTokens != 200 {
		t.Errorf("Memory.MaxTokens = %d, want the project's 200", cfg.Memory.MaxTokens)
	}
}

func TestBuildChatModel(t *testing.T) {
	cm, err := BuildChatModel(context.Background(), ModelConfig{BaseURL: defaultBaseURL, APIKey: defaultAPIKey, Model: "test-model"})
	if err != nil {
		t.Fatalf("BuildChatModel() error = %v", err)
	}
	if cm == nil {
		t.Fatal("BuildChatModel() returned a nil model")
	}
}

func TestDiscoverContextWindow_LMStudioSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model","max_context_length":32768,"loaded_context_length":16384}]}`))
	}))
	defer srv.Close()

	got := DiscoverContextWindow(context.Background(), ModelConfig{BaseURL: srv.URL + "/v1", Model: "test-model"})
	if got != 16384 {
		t.Errorf("DiscoverContextWindow() = %d, want 16384 (loaded_context_length preferred)", got)
	}
}

func TestDiscoverContextWindow_PrefersMaxWhenLoadedAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model","max_context_length":32768}]}`))
	}))
	defer srv.Close()

	got := DiscoverContextWindow(context.Background(), ModelConfig{BaseURL: srv.URL + "/v1", Model: "test-model"})
	if got != 32768 {
		t.Errorf("DiscoverContextWindow() = %d, want 32768", got)
	}
}

func TestDiscoverContextWindow_FallsBackToConfigField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	got := DiscoverContextWindow(context.Background(), ModelConfig{BaseURL: srv.URL + "/v1", Model: "test-model", ContextWindow: 4096})
	if got != 4096 {
		t.Errorf("DiscoverContextWindow() = %d, want 4096 (config field fallback)", got)
	}
}

func TestDiscoverContextWindow_FallsBackToDefault(t *testing.T) {
	got := DiscoverContextWindow(context.Background(), ModelConfig{BaseURL: "http://127.0.0.1:1", Model: "test-model"})
	if got != defaultContextWindow {
		t.Errorf("DiscoverContextWindow() = %d, want default %d", got, defaultContextWindow)
	}
}

func TestDiscoverContextWindow_NoMatchingModelFallsThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"other-model","max_context_length":32768}]}`))
	}))
	defer srv.Close()

	got := DiscoverContextWindow(context.Background(), ModelConfig{BaseURL: srv.URL + "/v1", Model: "test-model", ContextWindow: 2048})
	if got != 2048 {
		t.Errorf("DiscoverContextWindow() = %d, want 2048 (fallback since no matching model)", got)
	}
}

// "~/.dewey/memory" is the value the design doc documents. Taken literally it
// makes the real profile invisible and creates a directory named "~" in
// whatever project the user launched from.
func TestLoad_MemoryDirExpandsLeadingTilde(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, project, "memory:\n  dir: ~/.dewey/memory\n")

	cfg, _, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if strings.HasPrefix(cfg.Memory.Dir, "~") {
		t.Errorf("Memory.Dir = %q, want the tilde expanded", cfg.Memory.Dir)
	}
	if want := filepath.Join(home, ".dewey", "memory"); cfg.Memory.Dir != want {
		t.Errorf("Memory.Dir = %q, want %q", cfg.Memory.Dir, want)
	}
}

func TestLoad_MemoryDirWithoutTildeIsUnchanged(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, project, "memory:\n  dir: /opt/memories\n")

	cfg, _, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Memory.Dir != "/opt/memories" {
		t.Errorf("Memory.Dir = %q, want it untouched", cfg.Memory.Dir)
	}
}

// Running from a subdirectory must not lose the project's settings. It used to:
// config lookup did not walk up while `git -C` did, so the two halves of the
// same project disagreed about where they were.
func TestProjectRoot_WalksUpToTheDeweyDirectory(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "shell_rules:\n  always_write: [mytool]\n")
	deep := filepath.Join(root, "services", "api", "internal")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := ProjectRoot(deep); got != root {
		t.Errorf("ProjectRoot(%q) = %q, want %q", deep, got, root)
	}

	// And the config actually loads from down there.
	t.Setenv("HOME", t.TempDir())
	cfg, _, err := Load(deep)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !contains(cfg.ShellRules.AlwaysWrite, "mytool") {
		t.Errorf("project shell rules not picked up from a subdirectory: %+v", cfg.ShellRules.AlwaysWrite)
	}
}

// The search must not wander above the repository and adopt a parent's config.
func TestProjectRoot_StopsAtARepositoryBoundary(t *testing.T) {
	outer := t.TempDir()
	writeConfig(t, outer, "shell_rules:\n  always_write: [outertool]\n")

	inner := filepath.Join(outer, "vendored")
	if err := os.MkdirAll(filepath.Join(inner, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := ProjectRoot(inner); got != inner {
		t.Errorf("ProjectRoot(%q) = %q, want it to stop at the repo boundary %q", inner, got, inner)
	}
}

func TestProjectRoot_NoDeweyAnywhereReturnsTheStartingDirectory(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	// t.TempDir() lives under /var/folders on macOS, which has no .dewey or
	// .git above it, so the walk should bottom out at the filesystem root and
	// fall back to where it started.
	if got := ProjectRoot(deep); got != deep {
		t.Errorf("ProjectRoot(%q) = %q, want the starting directory back", deep, got)
	}
}

// org: in a project config is no longer read at all. A checkout's contents come
// from whoever authored the repo, so honoring it let a cloned repo decide which
// of the user's org-scoped memories were loaded.
func TestLoad_ProjectConfigCannotDeclareAnOrg(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, project, "org: acme\nmemory:\n  max_tokens: 1234\n")

	cfg, _, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	// The rest of the section still applies; only org is ignored.
	if cfg.Memory.MaxTokens != 1234 {
		t.Errorf("MaxTokens = %d, want the section still honored", cfg.Memory.MaxTokens)
	}
}

// Setting just simple.model is the common case: one server, two models. If the
// unset fields came from the built-in LM Studio defaults instead of from
// intermediate, the classifier and summarizer would point at a server that
// isn't running whenever intermediate is somewhere else — and because the
// classifier fails safe, the symptom is an approval prompt on every tool call
// plus failing compaction.
func TestLoad_PartialSimpleTierInheritsFromIntermediate(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, project, `models:
  intermediate:
    base_url: "http://localhost:7800/v1"
    api_key: "real-key"
    model: "big-model"
    context_window: 32768
  simple:
    model: "tiny-model"
`)

	cfg, _, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Simple.BaseURL != "http://localhost:7800/v1" {
		t.Errorf("Simple.BaseURL = %q, want intermediate's server", cfg.Simple.BaseURL)
	}
	if cfg.Simple.APIKey != "real-key" {
		t.Errorf("Simple.APIKey = %q, want intermediate's key", cfg.Simple.APIKey)
	}
	if cfg.Simple.Model != "tiny-model" {
		t.Errorf("Simple.Model = %q, want the explicitly configured model", cfg.Simple.Model)
	}
	if cfg.Simple.ContextWindow != 32768 {
		t.Errorf("Simple.ContextWindow = %d, want inherited 32768", cfg.Simple.ContextWindow)
	}
}

// A simple tier that names its own server must keep it.
func TestLoad_SimpleTierCanPointAtADifferentServer(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, project, `models:
  intermediate:
    base_url: "http://localhost:7800/v1"
    model: "big-model"
  simple:
    base_url: "http://localhost:1234/v1"
    model: "tiny-model"
`)

	cfg, _, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Simple.BaseURL != "http://localhost:1234/v1" {
		t.Errorf("Simple.BaseURL = %q, want its own explicit value", cfg.Simple.BaseURL)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
