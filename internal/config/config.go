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

// ModelConfig is the connection details for one model tier.
type ModelConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
	// ContextWindow is an optional explicit context-window size. When set it
	// takes precedence over anything discovered from the provider (see
	// DiscoverContextWindow), making it the escape hatch for a server that
	// misreports. Zero means "not set."
	ContextWindow int `yaml:"context_window"`
}

// Config is the fully-resolved, defaulted configuration for one run.
type Config struct {
	Simple       ModelConfig
	Intermediate ModelConfig
	Hard         ModelConfig
	Embedding    ModelConfig
	ShellRules   safety.ShellRules
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
	ShellRules safety.ShellRules `yaml:"shell_rules"`
	// Memory is a pointer so an absent section is distinguishable from one
	// that explicitly sets enabled: false.
	Memory *memoryFileConfig `yaml:"memory"`
}

// Load reads ~/.dewey/config.yaml and <projectDir>/.dewey/config.yaml,
// merges project values over global per-key, applies defaults, and returns
// the resolved Config. A missing file is not an error; malformed YAML or an
// invalid override value is.
// Load returns the resolved configuration plus any notices about rules the
// user's config weakened — removing a program from the always-write list is a
// legitimate thing to want and a thing worth being reminded of.
func Load(projectDir string) (Config, []string, error) {
	globalPath := ""
	if home, err := os.UserHomeDir(); err == nil {
		globalPath = filepath.Join(home, configDirName, configFileName)
	}

	global, err := readFileConfig(globalPath)
	if err != nil {
		return Config{}, nil, err
	}

	projectPath := filepath.Join(ProjectRoot(projectDir), configDirName, configFileName)
	project, err := readFileConfig(projectPath)
	if err != nil {
		return Config{}, nil, err
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

func merge(global, project fileConfig) (Config, []string, error) {
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

	defaults, err := safety.DefaultShellRules()
	if err != nil {
		return Config{}, nil, err
	}
	rules, ruleNotices := defaults.Merge(global.ShellRules)
	rules, projectNotices := rules.Merge(project.ShellRules)
	ruleNotices = append(ruleNotices, projectNotices...)
	if err := rules.Compile(); err != nil {
		return Config{}, nil, err
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
		ShellRules:   rules,
		Memory:       memory,
	}, ruleNotices, nil
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

// maxAssumedContextWindow caps a window size that was inferred from a model's
// architectural maximum rather than measured from a loaded instance.
const maxAssumedContextWindow = 32768

type lmStudioModelsResponse struct {
	Data []struct {
		ID                  string    `json:"id"`
		MaxContextLength    int       `json:"max_context_length"`
		LoadedContextLength int       `json:"loaded_context_length"`
		Capabilities        *[]string `json:"capabilities"`
	} `json:"data"`
}

// fetchLMStudioModels issues the shared LM Studio /api/v0/models request used
// by both context-window and tool-support discovery. The bool reports
// whether a well-formed response was obtained at all; callers that can't find
// what they're looking for in parsed.Data still get ok=true and decide for
// themselves what that means.
func fetchLMStudioModels(ctx context.Context, m ModelConfig) (lmStudioModelsResponse, bool) {
	base := strings.TrimSuffix(strings.TrimSuffix(m.BaseURL, "/"), "/v1")

	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/api/v0/models", nil)
	if err != nil {
		return lmStudioModelsResponse{}, false
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return lmStudioModelsResponse{}, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return lmStudioModelsResponse{}, false
	}

	var parsed lmStudioModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return lmStudioModelsResponse{}, false
	}
	return parsed, true
}

// ToolSupport is what a provider says about a model's ability to call tools.
type ToolSupport int

const (
	// ToolSupportUnknown means the provider did not tell us. dewey must not
	// act on this: most OpenAI-compatible servers expose no such field, and
	// treating silence as a refusal would lock users out of every provider
	// except LM Studio.
	ToolSupportUnknown ToolSupport = iota
	// ToolSupportYes means the provider advertises tool use.
	ToolSupportYes
	// ToolSupportNo means the provider enumerated this model's capabilities
	// and tool use was not among them.
	ToolSupportNo
)

// DiscoverToolSupport asks LM Studio whether m.Model can call tools.
//
// dewey cannot do its job with a model that cannot call tools: it will
// hallucinate tool results instead of running anything, and with no token
// limit it can generate until the context window fills. That is a failure
// worth catching at launch rather than halfway through a turn, and the answer
// is already in the response dewey reads for the context window.
//
// Returns ToolSupportUnknown for every provider that does not expose this,
// which is most of them.
func DiscoverToolSupport(ctx context.Context, m ModelConfig) ToolSupport {
	parsed, ok := fetchLMStudioModels(ctx, m)
	if !ok {
		return ToolSupportUnknown
	}

	// Whether any model carries the field at all decides how to read its
	// absence on the one we care about. LM Studio omits the key entirely for a
	// model it does not credit with tool use rather than sending an empty
	// list, so reading absence as "unknown" let exactly the model this check
	// exists for start anyway. A sibling entry carrying the field proves the
	// server knows how to report capabilities, which makes the omission a
	// statement rather than a silence.
	//
	// Measured, not assumed: the field is not load-state dependent — gemma
	// reports tool_use both loaded and not — so a loaded model missing it is
	// missing it on its own merits.
	serverReportsCapabilities := false
	for _, entry := range parsed.Data {
		if entry.Capabilities != nil {
			serverReportsCapabilities = true
			break
		}
	}

	for _, entry := range parsed.Data {
		if entry.ID != m.Model {
			continue
		}
		if entry.Capabilities == nil {
			// A provider that reports capabilities for nothing tells us
			// nothing. That is every OpenAI-compatible server but LM Studio,
			// and refusing them would be worse than the bug this prevents.
			if serverReportsCapabilities {
				return ToolSupportNo
			}
			return ToolSupportUnknown
		}
		for _, capability := range *entry.Capabilities {
			if capability == "tool_use" {
				return ToolSupportYes
			}
		}
		return ToolSupportNo
	}

	// Not listed at all: the model may be served by something else entirely,
	// or LM Studio may simply not have it in view yet. Not a negative.
	return ToolSupportUnknown
}

// ToolCapableModels returns the models the provider says can call tools, for
// an error message that tells the user what they can switch to rather than
// only what is wrong.
func ToolCapableModels(ctx context.Context, m ModelConfig) []string {
	parsed, ok := fetchLMStudioModels(ctx, m)
	if !ok {
		return nil
	}

	var names []string
	for _, entry := range parsed.Data {
		if entry.Capabilities == nil {
			continue
		}
		for _, cap := range *entry.Capabilities {
			if cap == "tool_use" {
				names = append(names, entry.ID)
				break
			}
		}
	}
	return names
}

// DiscoverContextWindow resolves the context window size for m, trying (in
// order): m.ContextWindow if set, LM Studio's native API, then a hardcoded
// default. LM Studio discovery failures are silent and expected for any
// other OpenAI-compatible provider — this is a best-effort convenience, not
// a required capability.
//
// An explicitly configured window is checked first because it is the escape
// hatch for a provider that misreports, and discovery is the thing being
// escaped. A discovered value that was only inferred from the model's
// architectural maximum is clamped; see discoverLMStudioContextWindow.
func DiscoverContextWindow(ctx context.Context, m ModelConfig) int {
	if m.ContextWindow > 0 {
		return m.ContextWindow
	}
	if window, exact, ok := discoverLMStudioContextWindow(ctx, m); ok {
		if exact {
			return window
		}
		return min(window, maxAssumedContextWindow)
	}
	return defaultContextWindow
}

// discoverLMStudioContextWindow asks LM Studio about m.Model. The second
// return reports whether the window was *measured* from a loaded model
// (loaded_context_length) rather than inferred from the architecture's ceiling
// (max_context_length).
//
// The distinction matters by more than an order of magnitude: an unloaded
// gemma-4-31b advertises max_context_length 262144 and the nemotron models
// advertise 1048576, while the context a server will actually serve once the
// model is loaded is typically a small fraction of that. Treating the ceiling
// as a measurement means compaction never fires and the request fails at the
// provider instead.
func discoverLMStudioContextWindow(ctx context.Context, m ModelConfig) (window int, exact, ok bool) {
	parsed, fetched := fetchLMStudioModels(ctx, m)
	if !fetched {
		return 0, false, false
	}

	for _, entry := range parsed.Data {
		if entry.ID != m.Model {
			continue
		}
		if entry.LoadedContextLength > 0 {
			return entry.LoadedContextLength, true, true
		}
		if entry.MaxContextLength > 0 {
			return entry.MaxContextLength, false, true
		}
		return 0, false, false
	}
	return 0, false, false
}

// RefreshContextWindow re-resolves the window for m after the provider has
// served a request, which implies the model is now resident and
// loaded_context_length reports what it will actually serve. The bool is
// false when there is nothing better on offer than whatever is already in
// use: the window is explicitly configured, discovery failed, or the model is
// still unloaded and only its architectural ceiling is visible.
//
// This exists because DiscoverContextWindow runs at startup, when the model
// is often not loaded yet and the best it can do is clamp a guess. Launching
// dewey against an idle server would otherwise mean running at the clamp for
// the whole session even after the server loads the model at its full size.
func RefreshContextWindow(ctx context.Context, m ModelConfig) (int, bool) {
	if m.ContextWindow > 0 {
		return 0, false
	}
	window, exact, ok := discoverLMStudioContextWindow(ctx, m)
	if !ok || !exact {
		return 0, false
	}
	return window, true
}
