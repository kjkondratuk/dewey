package config

import (
	"context"
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

func TestBuildChatModel(t *testing.T) {
	cm, err := BuildChatModel(context.Background(), ModelConfig{BaseURL: defaultBaseURL, APIKey: defaultAPIKey, Model: "test-model"})
	if err != nil {
		t.Fatalf("BuildChatModel() error = %v", err)
	}
	if cm == nil {
		t.Fatal("BuildChatModel() returned a nil model")
	}
}
