package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"gopkg.in/yaml.v3"

	"github.com/kjkondratuk/dewey/internal/safety"
)

const (
	defaultBaseURL           = "http://localhost:1234/v1"
	defaultAPIKey            = "lm-studio"
	defaultIntermediateModel = "gemma-4-31b-it-turboquant-mlx"
	configDirName            = ".dewey"
	configFileName           = "config.yaml"
)

// ModelConfig is the connection details for one model tier.
type ModelConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
}

// Config is the fully-resolved, defaulted configuration for one run.
type Config struct {
	Simple       ModelConfig
	Intermediate ModelConfig
	Hard         ModelConfig
	Embedding    ModelConfig
	Overrides    safety.RiskOverrides
}

type fileConfig struct {
	Models struct {
		Simple       ModelConfig `yaml:"simple"`
		Intermediate ModelConfig `yaml:"intermediate"`
		Hard         ModelConfig `yaml:"hard"`
		Embedding    ModelConfig `yaml:"embedding"`
	} `yaml:"models"`
	Overrides map[string]string `yaml:"overrides"`
}

// Load reads ~/.dewey/config.yaml and <projectDir>/.dewey/config.yaml,
// merges project values over global per-key, applies defaults, and returns
// the resolved Config. A missing file is not an error; malformed YAML or an
// invalid override value is.
func Load(projectDir string) (Config, error) {
	globalPath := ""
	if home, err := os.UserHomeDir(); err == nil {
		globalPath = filepath.Join(home, configDirName, configFileName)
	}

	global, err := readFileConfig(globalPath)
	if err != nil {
		return Config{}, err
	}

	projectPath := filepath.Join(projectDir, configDirName, configFileName)
	project, err := readFileConfig(projectPath)
	if err != nil {
		return Config{}, err
	}

	return merge(global, project)
}

func readFileConfig(path string) (fileConfig, error) {
	if path == "" {
		return fileConfig{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileConfig{}, nil
		}
		return fileConfig{}, fmt.Errorf("reading %s: %w", path, err)
	}

	var fc fileConfig
	if err := yaml.Unmarshal(data, &fc); err != nil {
		return fileConfig{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return fc, nil
}

func merge(global, project fileConfig) (Config, error) {
	intermediate := pickModelConfig(global.Models.Intermediate, project.Models.Intermediate)
	applyModelDefaults(&intermediate, defaultIntermediateModel)

	simple := pickModelConfig(global.Models.Simple, project.Models.Simple)
	if simple == (ModelConfig{}) {
		simple = intermediate
	} else {
		applyModelDefaults(&simple, intermediate.Model)
	}

	hard := pickModelConfig(global.Models.Hard, project.Models.Hard)
	applyModelDefaults(&hard, "")

	embedding := pickModelConfig(global.Models.Embedding, project.Models.Embedding)
	applyModelDefaults(&embedding, "")

	overrides := safety.RiskOverrides{}
	for name, raw := range global.Overrides {
		tier, err := safety.ParseRiskTier(raw)
		if err != nil {
			return Config{}, fmt.Errorf("global override for %q: %w", name, err)
		}
		overrides[name] = tier
	}
	for name, raw := range project.Overrides {
		tier, err := safety.ParseRiskTier(raw)
		if err != nil {
			return Config{}, fmt.Errorf("project override for %q: %w", name, err)
		}
		overrides[name] = tier
	}

	return Config{
		Simple:       simple,
		Intermediate: intermediate,
		Hard:         hard,
		Embedding:    embedding,
		Overrides:    overrides,
	}, nil
}

// pickModelConfig returns project if it sets anything, else global. A tier
// is treated as a single unit: project setting any field for a tier
// replaces the whole tier rather than merging individual fields.
func pickModelConfig(global, project ModelConfig) ModelConfig {
	if project != (ModelConfig{}) {
		return project
	}
	return global
}

func applyModelDefaults(m *ModelConfig, defaultModel string) {
	if m.BaseURL == "" {
		m.BaseURL = defaultBaseURL
	}
	if m.APIKey == "" {
		m.APIKey = defaultAPIKey
	}
	if m.Model == "" {
		m.Model = defaultModel
	}
}

// BuildChatModel constructs an eino chat model from a resolved ModelConfig.
func BuildChatModel(ctx context.Context, m ModelConfig) (model.ToolCallingChatModel, error) {
	cm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		APIKey:  m.APIKey,
		BaseURL: m.BaseURL,
		Model:   m.Model,
	})
	if err != nil {
		return nil, err
	}
	return cm, nil
}
