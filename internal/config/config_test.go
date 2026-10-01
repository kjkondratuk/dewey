package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjkondratuk/dewey/internal/safety"
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

	cfg, err := Load(project)
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
	if len(cfg.Overrides) != 0 {
		t.Errorf("Overrides = %+v, want empty", cfg.Overrides)
	}
}

func TestLoad_ProjectOverridesGlobalPerKey(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)

	writeConfig(t, home, "overrides:\n  read_file: safe\n  web_request: dangerous\n")
	writeConfig(t, project, "overrides:\n  web_request: moderate\n")

	cfg, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Overrides["read_file"] != safety.Safe {
		t.Errorf("read_file override = %v, want Safe (from global)", cfg.Overrides["read_file"])
	}
	if cfg.Overrides["web_request"] != safety.Moderate {
		t.Errorf("web_request override = %v, want Moderate (project wins)", cfg.Overrides["web_request"])
	}
}

func TestLoad_ProjectModelTierReplacesGlobalTier(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)

	writeConfig(t, home, "models:\n  simple:\n    base_url: \"http://global:1234/v1\"\n    api_key: \"global-key\"\n    model: \"global-model\"\n")
	writeConfig(t, project, "models:\n  simple:\n    model: \"project-model\"\n")

	cfg, err := Load(project)
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

	if _, err := Load(project); err == nil {
		t.Fatal("expected an error for malformed YAML")
	}
}

func TestLoad_InvalidOverrideValueIsAnError(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)

	writeConfig(t, project, "overrides:\n  read_file: extremely-safe\n")

	if _, err := Load(project); err == nil {
		t.Fatal("expected an error for invalid override value")
	}
}

func TestLoad_MemoryDefaults(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := Load(project)
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

	cfg, err := Load(project)
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

	cfg, err := Load(project)
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

	cfg, err := Load(project)
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

func TestLoad_ProjectOrgIsRead(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, project, "org: acme\n")

	cfg, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Org != "acme" {
		t.Errorf("Org = %q, want acme", cfg.Org)
	}
}

// The org names which organization's work this checkout is, so it is a
// property of the project and a global default would apply it everywhere.
func TestLoad_GlobalOrgIsIgnored(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, home, "org: globex\n")

	cfg, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Org != "" {
		t.Errorf("Org = %q, want empty: a global org would scope every project to it", cfg.Org)
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
