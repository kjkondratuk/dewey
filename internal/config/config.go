package config

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	defaultMemoryMaxTokens   = 4000
)

// MemoryConfig controls the long-term memory store.
type MemoryConfig struct {
	Enabled   bool
	Dir       string
	MaxTokens int
}

// builtinOverrides pins tools whose risk tier must not be left to the
// classifier's judgement.
var builtinOverrides = safety.RiskOverrides{
	// remember writes to the system prompt of every future session. A
	// classifier answering {"kind":"read","risk":"safe"} would let the model
	// save a memory with no confirmation at all, while the system prompt tells
	// it approval is required.
	"remember": safety.Moderate,
}

// ModelConfig is the connection details for one model tier.
type ModelConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
	// ContextWindow is an optional explicit context-window size, used as a
	// fallback when it can't be discovered from the provider (see
	// DiscoverContextWindow). Zero means "not set."
	ContextWindow int `yaml:"context_window"`
}

// Config is the fully-resolved, defaulted configuration for one run.
type Config struct {
	Simple       ModelConfig
	Intermediate ModelConfig
	Hard         ModelConfig
	Embedding    ModelConfig
	Overrides    safety.RiskOverrides
	Memory       MemoryConfig
}

// memoryFileConfig mirrors MemoryConfig with every field optional. Enabled is
// a pointer because a plain bool cannot distinguish `enabled: false` from a
// section that only sets `dir`, and conflating them would mean configuring
// where memories live silently switches memory off.
type memoryFileConfig struct {
	Enabled   *bool  `yaml:"enabled"`
	Dir       string `yaml:"dir"`
	MaxTokens int    `yaml:"max_tokens"`
}

type fileConfig struct {
	Models struct {
		Simple       ModelConfig `yaml:"simple"`
		Intermediate ModelConfig `yaml:"intermediate"`
		Hard         ModelConfig `yaml:"hard"`
		Embedding    ModelConfig `yaml:"embedding"`
	} `yaml:"models"`
	Overrides map[string]string `yaml:"overrides"`
	// Memory is a pointer so an absent section is distinguishable from one
	// that explicitly sets enabled: false.
	Memory *memoryFileConfig `yaml:"memory"`
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

	projectPath := filepath.Join(ProjectRoot(projectDir), configDirName, configFileName)
	project, err := readFileConfig(projectPath)
	if err != nil {
		return Config{}, err
	}

	return merge(global, project)
}

// ProjectRoot finds the directory whose .dewey/ applies to work in dir,
// searching upward the way git locates a repository.
//
// Without the walk, running dewey from a subdirectory silently lost the
// project's config and memories while `git -C` kept resolving the repo — so
// the two halves of the same project disagreed about where they were. The
// search stops at a .dewey directory, or at a .git directory, so it cannot
// wander above the repository into unrelated parents.
func ProjectRoot(dir string) string {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}

	for cur := dir; ; {
		if isDir(filepath.Join(cur, configDirName)) {
			return cur
		}
		// A repository boundary with no .dewey/ means this project simply has
		// no project-level settings; stop rather than inheriting a parent's.
		if isDir(filepath.Join(cur, ".git")) {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return dir
		}
		cur = parent
	}
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
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

	// A partially-specified simple tier inherits from intermediate, not from
	// the built-in defaults. Setting just `simple.model` is the common case —
	// one server, two models — and filling base_url from the LM Studio default
	// would silently point the classifier and summarizer at a server that
	// isn't there whenever intermediate is somewhere else. The classifier
	// fails safe, so the symptom is an approval prompt on every single tool
	// call plus failing compaction, which is a miserable thing to debug.
	simple := pickModelConfig(global.Models.Simple, project.Models.Simple)
	if simple == (ModelConfig{}) {
		simple = intermediate
	} else {
		inheritFrom(&simple, intermediate)
	}

	hard := pickModelConfig(global.Models.Hard, project.Models.Hard)
	applyModelDefaults(&hard, "")

	embedding := pickModelConfig(global.Models.Embedding, project.Models.Embedding)
	applyModelDefaults(&embedding, "")

	overrides := safety.RiskOverrides{}
	// remember writes to the system prompt of every future session, so its
	// confirmation gate cannot rest on the classifier agreeing that it is a
	// write. A classifier answering {"kind":"read","risk":"safe"} would
	// otherwise let the model save a memory with no prompt at all, while the
	// system prompt tells it approval is required. Seeded before the file
	// loops so a user who really wants it ungated can still override.
	for name, tier := range builtinOverrides {
		overrides[name] = tier
	}
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

	memory := MemoryConfig{Enabled: true, MaxTokens: defaultMemoryMaxTokens}
	if home, err := os.UserHomeDir(); err == nil {
		memory.Dir = filepath.Join(home, configDirName, "memory")
	}
	for _, fc := range []fileConfig{global, project} {
		if fc.Memory == nil {
			continue
		}
		if fc.Memory.Enabled != nil {
			memory.Enabled = *fc.Memory.Enabled
		}
		if fc.Memory.Dir != "" {
			memory.Dir = expandHome(fc.Memory.Dir)
		}
		if fc.Memory.MaxTokens > 0 {
			memory.MaxTokens = fc.Memory.MaxTokens
		}
	}

	return Config{
		Simple:       simple,
		Intermediate: intermediate,
		Hard:         hard,
		Embedding:    embedding,
		Overrides:    overrides,
		Memory:       memory,
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

// expandHome resolves a leading ~/ in a configured path. Without it the
// documented example value "~/.dewey/memory" is taken literally: the real
// profile becomes invisible and saves create a directory actually named "~"
// in whatever project the user happened to launch from.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[len("~/"):])
}

// inheritFrom fills a partially-specified tier from another resolved tier,
// so an unset field means "same as that one" rather than "same as the
// built-in default".
func inheritFrom(m *ModelConfig, from ModelConfig) {
	if m.BaseURL == "" {
		m.BaseURL = from.BaseURL
	}
	if m.APIKey == "" {
		m.APIKey = from.APIKey
	}
	if m.Model == "" {
		m.Model = from.Model
	}
	if m.ContextWindow == 0 {
		m.ContextWindow = from.ContextWindow
	}
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

const defaultContextWindow = 8192

type lmStudioModelsResponse struct {
	Data []struct {
		ID                  string `json:"id"`
		MaxContextLength    int    `json:"max_context_length"`
		LoadedContextLength int    `json:"loaded_context_length"`
	} `json:"data"`
}

// DiscoverContextWindow resolves the context window size for m, trying (in
// order): LM Studio's native API, m.ContextWindow if set, then a hardcoded
// default. LM Studio discovery failures are silent and expected for any
// other OpenAI-compatible provider — this is a best-effort convenience, not
// a required capability.
func DiscoverContextWindow(ctx context.Context, m ModelConfig) int {
	if window, ok := discoverLMStudioContextWindow(ctx, m); ok {
		return window
	}
	if m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return defaultContextWindow
}

func discoverLMStudioContextWindow(ctx context.Context, m ModelConfig) (int, bool) {
	base := strings.TrimSuffix(strings.TrimSuffix(m.BaseURL, "/"), "/v1")

	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/api/v0/models", nil)
	if err != nil {
		return 0, false
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, false
	}

	var parsed lmStudioModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0, false
	}

	for _, entry := range parsed.Data {
		if entry.ID != m.Model {
			continue
		}
		if entry.LoadedContextLength > 0 {
			return entry.LoadedContextLength, true
		}
		if entry.MaxContextLength > 0 {
			return entry.MaxContextLength, true
		}
		return 0, false
	}
	return 0, false
}
