# Tool Safety Classification & Confirmation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every tool call the agent's model requests is classified per-invocation into a risk tier (safe/moderate/dangerous) — via config override or an LLM classifier — and moderate/dangerous calls pause the TUI for an explicit y/n confirmation before executing. Model connectivity moves from one hardcoded chat model to a tiered config (simple/intermediate/hard + reserved embedding), loaded from `~/.dewey/config.yaml` and `./.dewey/config.yaml`.

**Architecture:** Two new leaf packages (`internal/safety` for classification types + the LLM classifier, `internal/config` for tiered model config + YAML loading/merging) plug into `internal/agent`, which gates each tool call behind classification before executing it and blocks on a per-call response channel carried on a new `Event` kind. `internal/app` renders that event as an inline system turn and answers it from keypresses. `main.go` wires config → models → classifier → agent.

**Tech Stack:** Go 1.26, `github.com/cloudwego/eino` (chat model / tool interfaces), `github.com/cloudwego/eino-ext/components/model/openai` (OpenAI-compatible client, reused for every tier), `gopkg.in/yaml.v3` (already an indirect dependency, promoted to direct), stdlib `testing`.

**Spec:** `docs/superpowers/specs/2026-09-26-tool-safety-classification-design.md`

---

## File Structure

- `internal/safety/risk.go` (new) — `RiskTier`, `ParseRiskTier`, `Classification`, `RiskOverrides`
- `internal/safety/risk_test.go` (new)
- `internal/safety/classifier.go` (new) — `Classifier` interface + `llmClassifier` (LLM-backed, fail-safe)
- `internal/safety/classifier_test.go` (new)
- `internal/config/config.go` (new) — `ModelConfig`, `Config`, `Load`, merge/defaulting, `BuildChatModel`
- `internal/config/config_test.go` (new)
- `internal/agent/agent.go` (modify) — `Event` gains a `Confirm chan<- bool` field
- `internal/agent/openai.go` (modify) — `openaiAgent` gains classifier/overrides, gates tool execution
- `internal/agent/openai_test.go` (new)
- `internal/app/app.go` (modify) — renders `"confirm"` events, handles y/n/esc while one is pending
- `main.go` (modify) — loads config, builds tiered models, wires classifier into `agent.New`
- `go.mod` / `go.sum` (modify) — `gopkg.in/yaml.v3` promoted to a direct dependency

---

### Task 1: Safety package — risk tiers & classification types

**Files:**
- Create: `internal/safety/risk.go`
- Test: `internal/safety/risk_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/safety/risk_test.go
package safety

import "testing"

func TestRiskTierString(t *testing.T) {
	cases := map[RiskTier]string{
		Safe:      "safe",
		Moderate:  "moderate",
		Dangerous: "dangerous",
	}
	for tier, want := range cases {
		if got := tier.String(); got != want {
			t.Errorf("RiskTier(%d).String() = %q, want %q", int(tier), got, want)
		}
	}
}

func TestParseRiskTier(t *testing.T) {
	tests := []struct {
		in      string
		want    RiskTier
		wantErr bool
	}{
		{"safe", Safe, false},
		{"Moderate", Moderate, false},
		{"  dangerous  ", Dangerous, false},
		{"catastrophic", Dangerous, true},
		{"", Dangerous, true},
	}
	for _, tt := range tests {
		got, err := ParseRiskTier(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseRiskTier(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("ParseRiskTier(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/safety/... -run 'TestRiskTierString|TestParseRiskTier' -v`
Expected: FAIL — `package safety` has no `RiskTier`/`ParseRiskTier` (build failure, `internal/safety` doesn't exist yet).

- [ ] **Step 3: Write the implementation**

```go
// internal/safety/risk.go
package safety

import (
	"fmt"
	"strings"
)

// RiskTier is the safety classification assigned to a tool call.
type RiskTier int

const (
	Safe RiskTier = iota
	Moderate
	Dangerous
)

func (r RiskTier) String() string {
	switch r {
	case Safe:
		return "safe"
	case Moderate:
		return "moderate"
	case Dangerous:
		return "dangerous"
	default:
		return "dangerous"
	}
}

// ParseRiskTier parses a config/classifier string into a RiskTier. Any
// unrecognized value returns Dangerous alongside an error, so a caller that
// ignores the error still fails safe.
func ParseRiskTier(s string) (RiskTier, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "safe":
		return Safe, nil
	case "moderate":
		return Moderate, nil
	case "dangerous":
		return Dangerous, nil
	default:
		return Dangerous, fmt.Errorf("unknown risk tier %q", s)
	}
}

// Classification is the result of evaluating a single tool call.
type Classification struct {
	// Kind is "read" or "write" — informational only, it does not affect
	// confirmation gating. Empty when the classification came from a config
	// override rather than the classifier.
	Kind string
	Risk RiskTier
}

// RiskOverrides pins specific tool names to a fixed RiskTier, bypassing the
// classifier entirely for that tool.
type RiskOverrides map[string]RiskTier
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/safety/... -run 'TestRiskTierString|TestParseRiskTier' -v`
Expected: PASS (both tests)

- [ ] **Step 5: Commit**

```bash
git add internal/safety/risk.go internal/safety/risk_test.go
git commit -m "feat(safety): add risk tier and classification types"
```

---

### Task 2: Safety package — LLM-backed classifier

**Files:**
- Create: `internal/safety/classifier.go`
- Test: `internal/safety/classifier_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/safety/classifier_test.go
package safety

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type fakeGenerateModel struct {
	content string
	err     error
}

func (f *fakeGenerateModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &schema.Message{Role: schema.Assistant, Content: f.content}, nil
}

func (f *fakeGenerateModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	panic("not used in these tests")
}

func TestLLMClassifier_ValidResponse(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: `{"kind":"read","risk":"safe"}`})

	got, err := classifier.Classify(context.Background(), "read_file", "reads a file", `{"path":"/tmp/x"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Classification{Kind: "read", Risk: Safe}
	if got != want {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestLLMClassifier_ToleratesSurroundingText(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{
		content: "Sure, here is my answer:\n```json\n{\"kind\":\"write\",\"risk\":\"dangerous\"}\n```",
	})

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{"method":"DELETE"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Classification{Kind: "write", Risk: Dangerous}
	if got != want {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestLLMClassifier_ModelErrorFailsSafe(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{err: errors.New("connection refused")})

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want fail-safe %+v", got, failSafeClassification)
	}
}

func TestLLMClassifier_MalformedJSONFailsSafe(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: "not json at all"})

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want fail-safe %+v", got, failSafeClassification)
	}
}

func TestLLMClassifier_UnrecognizedFieldsFailSafe(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: `{"kind":"maybe","risk":"safe"}`})

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want fail-safe %+v", got, failSafeClassification)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/safety/... -run TestLLMClassifier -v`
Expected: FAIL — `NewLLMClassifier`, `failSafeClassification` undefined.

- [ ] **Step 3: Write the implementation**

```go
// internal/safety/classifier.go
package safety

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Classifier assigns a Classification to a resolved tool call.
type Classifier interface {
	Classify(ctx context.Context, toolName, toolDesc, argsJSON string) (Classification, error)
}

const classifierSystemPrompt = `You are a safety classifier for an autonomous agent's tool calls.
Given a tool name, its description, and the exact arguments it is about to be called with, decide:
- "kind": "read" if the call only observes state, "write" if it can change state (files, network resources, external systems).
- "risk": "safe" for read-only or clearly inconsequential actions, "moderate" for reversible writes with limited blast radius, "dangerous" for irreversible, destructive, or wide-blast-radius actions.
Respond with ONLY a single JSON object of the form {"kind":"read"|"write","risk":"safe"|"moderate"|"dangerous"}. No other text, no markdown fences.`

// failSafeClassification is returned whenever classification cannot be
// trusted — a model error, a malformed response, or an unrecognized field
// value. It must never be downgraded.
var failSafeClassification = Classification{Kind: "write", Risk: Dangerous}

// llmClassifier calls a chat model to classify each tool call, in a message
// exchange isolated from any ongoing conversation history.
type llmClassifier struct {
	model model.BaseChatModel
}

// NewLLMClassifier builds a Classifier backed by m. m should be a small/fast
// model — classification runs once per tool call.
func NewLLMClassifier(m model.BaseChatModel) Classifier {
	return &llmClassifier{model: m}
}

func (c *llmClassifier) Classify(ctx context.Context, toolName, toolDesc, argsJSON string) (Classification, error) {
	messages := []*schema.Message{
		{Role: schema.System, Content: classifierSystemPrompt},
		{
			Role: schema.User,
			Content: fmt.Sprintf("Tool: %s\nDescription: %s\nArguments: %s",
				toolName, toolDesc, argsJSON),
		},
	}

	resp, err := c.model.Generate(ctx, messages)
	if err != nil {
		return failSafeClassification, fmt.Errorf("classifier model call failed: %w", err)
	}

	classification, err := parseClassification(resp.Content)
	if err != nil {
		return failSafeClassification, err
	}
	return classification, nil
}

func parseClassification(raw string) (Classification, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start == -1 || end == -1 || end < start {
		return Classification{}, fmt.Errorf("no JSON object found in classifier response: %q", raw)
	}

	var parsed struct {
		Kind string `json:"kind"`
		Risk string `json:"risk"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &parsed); err != nil {
		return Classification{}, fmt.Errorf("invalid classifier JSON: %w", err)
	}

	if parsed.Kind != "read" && parsed.Kind != "write" {
		return Classification{}, fmt.Errorf("unrecognized classifier kind %q", parsed.Kind)
	}

	risk, err := ParseRiskTier(parsed.Risk)
	if err != nil {
		return Classification{}, fmt.Errorf("unrecognized classifier risk: %w", err)
	}

	return Classification{Kind: parsed.Kind, Risk: risk}, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/safety/... -v`
Expected: PASS (all tests in the package)

- [ ] **Step 5: Commit**

```bash
git add internal/safety/classifier.go internal/safety/classifier_test.go
git commit -m "feat(safety): add LLM-backed tool call classifier"
```

---

### Task 3: Config package — tiered model config & risk override loading

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/config/config_test.go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/... -v`
Expected: FAIL — `internal/config` doesn't exist yet (build failure).

- [ ] **Step 3: Write the implementation**

```go
// internal/config/config.go
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
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/config/... -v`
Expected: PASS (all tests). If `gopkg.in/yaml.v3` fails to resolve, run `go mod tidy` first (it's already in `go.sum` as an indirect dependency, this just promotes it).

- [ ] **Step 5: Commit**

```bash
go mod tidy
git add internal/config/config.go internal/config/config_test.go go.mod go.sum
git commit -m "feat(config): add tiered model config with YAML loading and merging"
```

---

### Task 4: Agent — per-invocation risk gating with confirmation

**Files:**
- Modify: `internal/agent/agent.go`
- Modify: `internal/agent/openai.go`
- Test: `internal/agent/openai_test.go`

- [ ] **Step 1: Add the `Confirm` field to `Event`**

```go
// internal/agent/agent.go
package agent

import "context"

type Event struct {
	Kind    string
	Payload string

	// Confirm is set only when Kind == "confirm". The receiver must send
	// exactly one value on it to unblock the agent: true to allow the
	// pending tool call, false to deny it. Leaving it unanswered and
	// cancelling the context instead takes the cancellation path.
	Confirm chan<- bool
}

type Agent interface {
	Run(ctx context.Context, input string) (<-chan Event, error)
}
```

- [ ] **Step 2: Write the failing test**

```go
// internal/agent/openai_test.go
package agent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"github.com/kjkondratuk/dewey/internal/safety"
)

type fakeStreamModel struct {
	responses [][]*schema.Message
	calls     int
}

func (f *fakeStreamModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	panic("not used")
}

func (f *fakeStreamModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	resp := f.responses[f.calls]
	f.calls++
	return schema.StreamReaderFromArray(resp), nil
}

func (f *fakeStreamModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return f, nil
}

type fakeClassifier struct {
	result safety.Classification
}

func (f *fakeClassifier) Classify(ctx context.Context, name, desc, argsJSON string) (safety.Classification, error) {
	return f.result, nil
}

type panicClassifier struct{}

func (panicClassifier) Classify(ctx context.Context, name, desc, argsJSON string) (safety.Classification, error) {
	panic("classifier should not be called when an override is set")
}

type echoInput struct {
	X string `json:"x"`
}

func newRecordingTool(t *testing.T, invoked *int) tool.InvokableTool {
	t.Helper()
	tl, err := utils.InferTool("do_thing", "does a thing", func(ctx context.Context, in echoInput) (string, error) {
		*invoked++
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("InferTool() error = %v", err)
	}
	return tl
}

func toolCallResponse(name, args string) []*schema.Message {
	return []*schema.Message{{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call-1",
			Type:     "function",
			Function: schema.FunctionCall{Name: name, Arguments: args},
		}},
	}}
}

func finalResponse(content string) []*schema.Message {
	return []*schema.Message{{Role: schema.Assistant, Content: content}}
}

func drain(t *testing.T, ch <-chan Event, respond func(Event) bool) []Event {
	t.Helper()
	var events []Event
	for evt := range ch {
		events = append(events, evt)
		if evt.Kind == "confirm" {
			evt.Confirm <- respond(evt)
		}
	}
	return events
}

func TestRun_SafeToolRunsWithoutConfirmation(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("done"),
	}}

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "read", Risk: safety.Safe}}, nil)

	ch, err := agt.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	if invoked != 1 {
		t.Errorf("tool invoked %d times, want 1", invoked)
	}
	for _, e := range events {
		if e.Kind == "confirm" {
			t.Fatalf("unexpected confirm event for a safe tool: %+v", e)
		}
	}
	if events[len(events)-1].Kind != "done" {
		t.Errorf("last event = %+v, want done", events[len(events)-1])
	}
}

func TestRun_OverrideSkipsClassifierAndAllowedDangerousRuns(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("done"),
	}}

	agt := New(m, []tool.InvokableTool{tl}, panicClassifier{}, safety.RiskOverrides{"do_thing": safety.Dangerous})

	ch, err := agt.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	sawConfirm := false
	events := drain(t, ch, func(e Event) bool {
		sawConfirm = true
		return true // allow
	})

	if !sawConfirm {
		t.Fatal("expected a confirm event for a dangerous tool")
	}
	if invoked != 1 {
		t.Errorf("tool invoked %d times, want 1", invoked)
	}
	if events[len(events)-1].Kind != "done" {
		t.Errorf("last event = %+v, want done", events[len(events)-1])
	}
}

func TestRun_DenialSkipsToolAndFeedsHistoryBack(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("okay, skipping that"),
	}}

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "write", Risk: safety.Dangerous}}, nil)

	ch, err := agt.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return false }) // deny

	if invoked != 0 {
		t.Errorf("tool invoked %d times, want 0 (denied)", invoked)
	}
	if events[len(events)-1].Kind != "done" {
		t.Errorf("last event = %+v, want done", events[len(events)-1])
	}

	found := false
	for _, msg := range agt.history {
		if msg.Role == schema.Tool && msg.Content == "error: user denied permission for this action" {
			found = true
		}
	}
	if !found {
		t.Error("expected a denial tool message in history")
	}
}

func TestRun_CancelDuringConfirmationDropsTheTurn(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
	}}

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "write", Risk: safety.Dangerous}}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := agt.Run(ctx, "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var kinds []string
	for evt := range ch {
		kinds = append(kinds, evt.Kind)
		if evt.Kind == "confirm" {
			cancel() // do not respond; the agent must unblock via ctx.Done()
		}
	}

	if invoked != 0 {
		t.Errorf("tool invoked %d times, want 0 (cancelled)", invoked)
	}
	if len(agt.history) != 0 {
		t.Errorf("history = %+v, want empty (turn dropped)", agt.history)
	}
	if kinds[len(kinds)-1] != "cancelled" {
		t.Errorf("last event kind = %q, want cancelled", kinds[len(kinds)-1])
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/agent/... -v`
Expected: FAIL — `New` doesn't accept a classifier/overrides yet (compile error).

- [ ] **Step 4: Write the implementation**

```go
// internal/agent/openai.go
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/kjkondratuk/dewey/internal/safety"
)

type openaiAgent struct {
	cm         model.ToolCallingChatModel
	tools      map[string]tool.InvokableTool
	descs      map[string]string
	classifier safety.Classifier
	overrides  safety.RiskOverrides
	history    []*schema.Message
}

// New builds an agent backed by cm, which must already have the tool schemas
// bound (via cm.WithTools). tools supplies the matching executable
// implementations, keyed by name, so tool calls the model requests can
// actually be run. classifier assigns a risk tier to each resolved tool
// call; overrides pins specific tool names to a fixed tier, bypassing the
// classifier for that tool.
func New(cm model.ToolCallingChatModel, tools []tool.InvokableTool, classifier safety.Classifier, overrides safety.RiskOverrides) *openaiAgent {
	byName := make(map[string]tool.InvokableTool, len(tools))
	descs := make(map[string]string, len(tools))
	for _, t := range tools {
		info, err := t.Info(context.Background())
		if err != nil {
			continue
		}
		byName[info.Name] = t
		descs[info.Name] = info.Desc
	}
	return &openaiAgent{
		cm:         cm,
		tools:      byName,
		descs:      descs,
		classifier: classifier,
		overrides:  overrides,
	}
}

func (a *openaiAgent) Run(ctx context.Context, input string) (<-chan Event, error) {
	truncateLen := len(a.history)
	a.history = append(a.history, &schema.Message{
		Role:    schema.User,
		Content: input,
	})

	ch := make(chan Event)
	go func() {
		defer close(ch)

		// return an error if the loop panics
		defer func() {
			if r := recover(); r != nil {
				ch <- Event{Kind: "error", Payload: fmt.Sprintf("panic: %v", r)}
			}
		}()

		// Each iteration is one full model turn: stream a response, then run
		// any tool calls it asked for. Loops back around with the tool
		// results appended to history until the model stops calling tools.
		for {
			sr, err := a.cm.Stream(ctx, a.history)
			if err != nil {
				ch <- Event{Kind: "error", Payload: err.Error()}
				return
			}

			var chunks []*schema.Message
			for {
				msg, err := sr.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if errors.Is(err, context.Canceled) {
					sr.Close()
					// drop the user turn too, as if the question was never asked
					a.history = a.history[:len(a.history)-1]
					ch <- Event{Kind: "cancelled"}
					return
				}
				if err != nil {
					sr.Close()
					ch <- Event{Kind: "error", Payload: err.Error()}
					return
				}
				chunks = append(chunks, msg)
				if msg.Content != "" {
					ch <- Event{Kind: "chunk", Payload: msg.Content}
				}
			}
			sr.Close()

			full, err := schema.ConcatMessages(chunks)
			if err != nil {
				ch <- Event{Kind: "error", Payload: err.Error()}
				return
			}
			a.history = append(a.history, full)

			if len(full.ToolCalls) == 0 {
				ch <- Event{Kind: "done"}
				return
			}

			// resolve all tool calls
			for _, tc := range full.ToolCalls {
				t, ok := a.tools[tc.Function.Name]
				if !ok {
					a.history = append(a.history, schema.ToolMessage(
						"error: unknown tool "+tc.Function.Name, tc.ID))
					continue
				}

				classification := a.classify(ctx, tc.Function.Name, tc.Function.Arguments)
				if classification.Risk != safety.Safe {
					allowed, cancelled := a.awaitConfirmation(ctx, ch, tc.Function.Name, classification, tc.Function.Arguments)
					if cancelled {
						a.history = a.history[:truncateLen]
						ch <- Event{Kind: "cancelled"}
						return
					}
					if !allowed {
						a.history = append(a.history, schema.ToolMessage(
							"error: user denied permission for this action", tc.ID))
						continue
					}
				}

				ch <- Event{Kind: "tool", Payload: tc.Function.Name}
				result, err := t.InvokableRun(ctx, tc.Function.Arguments)
				if err != nil {
					result = "error: " + err.Error()
				}
				a.history = append(a.history, schema.ToolMessage(result, tc.ID))
			}
			// loop back around and stream again with tool results in history
		}
	}()

	return ch, nil
}

// classify resolves the risk classification for a tool call: a config
// override always wins and skips the classifier; otherwise the classifier
// is consulted, which itself fails safe to Dangerous/write on any error.
func (a *openaiAgent) classify(ctx context.Context, name, argsJSON string) safety.Classification {
	if tier, ok := a.overrides[name]; ok {
		return safety.Classification{Risk: tier}
	}
	classification, _ := a.classifier.Classify(ctx, name, a.descs[name], argsJSON)
	return classification
}

// awaitConfirmation emits a "confirm" event and blocks until the caller
// responds on the event's Confirm channel, or ctx is cancelled.
func (a *openaiAgent) awaitConfirmation(ctx context.Context, ch chan<- Event, name string, c safety.Classification, argsJSON string) (allowed, cancelled bool) {
	resp := make(chan bool, 1)
	ch <- Event{Kind: "confirm", Payload: formatConfirmMessage(name, c, argsJSON), Confirm: resp}

	select {
	case allowed = <-resp:
		return allowed, false
	case <-ctx.Done():
		return false, true
	}
}

func formatConfirmMessage(name string, c safety.Classification, argsJSON string) string {
	args := argsJSON
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(argsJSON), "", "  "); err == nil {
		args = buf.String()
	}

	kind := ""
	if c.Kind != "" {
		kind = " (" + strings.ToUpper(c.Kind) + ")"
	}

	return fmt.Sprintf("%s wants to run%s [risk: %s]\n%s\nAllow? [y/n]", name, kind, c.Risk, args)
}
```

Note: the pre-existing `context.Canceled` branch inside the streaming `Recv` loop is left exactly as it was — it's an unrelated existing code path, not part of this feature. The new `truncateLen`-based rollback is only used by the new confirmation-cancellation path added above.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/agent/... -v`
Expected: PASS (all four `TestRun_*` tests)

- [ ] **Step 6: Commit**

```bash
git add internal/agent/agent.go internal/agent/openai.go internal/agent/openai_test.go
git commit -m "feat(agent): gate tool calls behind risk classification and confirmation"
```

---

### Task 5: App (TUI) — render confirmation prompts and handle y/n/esc

**Files:**
- Modify: `internal/app/app.go`

No new automated test — this task changes Bubbletea `Update`/rendering wiring, which is verified manually in Task 7. `internal/app` currently has no test file and no test harness for driving `tea.Msg` sequences; adding one is out of scope for this plan.

- [ ] **Step 1: Add the `pendingConfirm` field to `App`**

Find this in `internal/app/app.go`:

```go
type App struct {
	Agent         agent.Agent
	Conversation  viewport.Model
	Entry         textinput.Model
	agentChan     <-chan agent.Event
	cancel        context.CancelFunc
	renderer      *glamour.TermRenderer
	turns         []turn
	streaming     bool
	width, height int
}
```

Replace it with:

```go
type App struct {
	Agent          agent.Agent
	Conversation   viewport.Model
	Entry          textinput.Model
	agentChan      <-chan agent.Event
	cancel         context.CancelFunc
	renderer       *glamour.TermRenderer
	turns          []turn
	streaming      bool
	pendingConfirm chan<- bool
	width, height  int
}
```

- [ ] **Step 2: Intercept y/n/esc while a confirmation is pending**

Find this in `internal/app/app.go`:

```go
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "esc":
			if m.cancel != nil {
				m.cancel()
			}
			return m, nil
```

Replace it with:

```go
	case tea.KeyPressMsg:
		if m.pendingConfirm != nil {
			switch msg.String() {
			case "y":
				m.pendingConfirm <- true
				m.pendingConfirm = nil
				m.newLine("system", "Allowed.")
				return m, waitForAgentEvent(m.agentChan)
			case "n":
				m.pendingConfirm <- false
				m.pendingConfirm = nil
				m.newLine("system", "Denied.")
				return m, waitForAgentEvent(m.agentChan)
			case "esc":
				m.pendingConfirm = nil
				if m.cancel != nil {
					m.cancel()
				}
				return m, waitForAgentEvent(m.agentChan)
			}
			return m, nil
		}

		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "esc":
			if m.cancel != nil {
				m.cancel()
			}
			return m, nil
```

`esc` while a confirmation is pending does **not** send `false` on the channel — it cancels the context instead, so the agent's `select` in `awaitConfirmation` takes the `ctx.Done()` branch (the cancellation path, dropping the whole turn) rather than the denial path (which would continue the turn).

- [ ] **Step 3: Handle the `"confirm"` event kind**

Find this in `internal/app/app.go`:

```go
	case agentEventMsg:
		switch msg.Kind {
		case "cancelled":
			m.streaming = false
			m.cancel = nil
			m.newLine("system", "Cancelled")
			return m, nil
		case "chunk":
```

Replace it with:

```go
	case agentEventMsg:
		switch msg.Kind {
		case "cancelled":
			m.streaming = false
			m.cancel = nil
			m.newLine("system", "Cancelled")
			return m, nil
		case "confirm":
			m.streaming = false
			m.pendingConfirm = msg.Confirm
			m.newLine("system", msg.Payload)
			// Do not resume waitForAgentEvent yet — the agent goroutine is
			// blocked on Confirm, not on the event channel. Resumed from
			// the pendingConfirm branch in the KeyPressMsg handler above.
			return m, nil
		case "chunk":
```

- [ ] **Step 4: Build and manually sanity-check**

Run: `go build ./...`
Expected: builds with no errors.

There is no dedicated automated test for this file's Bubbletea wiring; Task 7's manual smoke test is what exercises this code path end-to-end.

- [ ] **Step 5: Commit**

```bash
git add internal/app/app.go
git commit -m "feat(app): render inline confirmation prompts and handle y/n/esc"
```

---

### Task 6: Wire main.go to config-driven models, classifier, and gating

**Files:**
- Modify: `main.go`

- [ ] **Step 1: Replace the model construction and agent wiring**

Find the entire contents of `main.go` and replace with:

```go
package main

import (
	"context"
	"log"
	"os"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/cloudwego/eino/schema"
	"github.com/kjkondratuk/dewey/internal/agent"
	"github.com/kjkondratuk/dewey/internal/app"
	"github.com/kjkondratuk/dewey/internal/config"
	"github.com/kjkondratuk/dewey/internal/safety"
	tools2 "github.com/kjkondratuk/dewey/internal/tools"
)

func main() {
	ctx := context.Background()

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("failed to determine working directory: %v", err)
	}

	cfg, err := config.Load(cwd)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	intermediateModel, err := config.BuildChatModel(ctx, cfg.Intermediate)
	if err != nil {
		log.Fatalf("failed to create intermediate model: %v", err)
	}

	simpleModel, err := config.BuildChatModel(ctx, cfg.Simple)
	if err != nil {
		log.Fatalf("failed to create simple model: %v", err)
	}
	classifier := safety.NewLLMClassifier(simpleModel)

	// register tools
	tools := tools2.NewToolFactory().DefaultInvokableToolset()
	infos := make([]*schema.ToolInfo, len(tools))
	for i, t := range tools {
		infos[i], _ = t.Info(ctx)
	}
	tm, err := intermediateModel.WithTools(infos)
	if err != nil {
		log.Fatalf("failed to bind tools: %v", err)
	}

	agt := agent.New(tm, tools, classifier, cfg.Overrides)

	prog := tea.NewProgram(initModel(agt))
	_, err = prog.Run()
	if err != nil {
		return
	}
}

func initModel(agt agent.Agent) *app.App {
	conversation := viewport.New()

	entry := textinput.New()
	entry.Prompt = "> "
	entry.SetVirtualCursor(false)

	return &app.App{Conversation: conversation, Entry: entry, Agent: agt}
}
```

This removes the hardcoded `openai.ChatModelConfig{...}` literal and the commented-out Claude scaffold — both are superseded by config-driven construction. With no `~/.dewey/config.yaml` or `./.dewey/config.yaml` present, `cfg.Intermediate` resolves to the exact same endpoint/model the hardcoded call used, so behavior is unchanged by default.

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: builds with no errors.

- [ ] **Step 3: Commit**

```bash
git add main.go
git commit -m "feat: wire config-driven tiered models and safety classifier into main"
```

---

### Task 7: Dependency tidy + whole-project verification

**Files:**
- Modify: `go.mod`, `go.sum` (if `go mod tidy` wasn't already run cleanly in Task 3)

- [ ] **Step 1: Tidy dependencies**

Run: `go mod tidy`
Expected: no errors; `gopkg.in/yaml.v3` appears as a direct (non-`// indirect`) requirement in `go.mod`.

- [ ] **Step 2: Run the full test suite**

Run: `go test ./... -v`
Expected: PASS across `internal/safety`, `internal/config`, `internal/agent` (and no other package gains tests in this plan).

- [ ] **Step 3: Vet and build**

Run: `go vet ./... && go build ./...`
Expected: no vet warnings, clean build.

- [ ] **Step 4: Manual smoke test against a local LM Studio instance**

With LM Studio (or another OpenAI-compatible server) running at `http://localhost:1234/v1`:

Run: `go run .`

In the TUI, ask the agent to do something that resolves to a moderate/dangerous `web_request` call (e.g. "POST an empty body to https://httpbin.org/post"). Confirm:
- A system turn renders showing the tool name, risk tier, and pretty-printed arguments, ending in `Allow? [y/n]`.
- Pressing `n` shows "Denied.", the request is not made, and the model's next reply acknowledges the denial.
- Asking again and pressing `y` shows "Allowed.", the request executes, and the result streams back normally.
- Asking again and pressing `esc` while the prompt is showing shows "Cancelled" and drops the turn.
- A plain read (e.g. "list the files in this directory") runs with no confirmation prompt at all.

- [ ] **Step 5: Commit** (only if `go mod tidy` changed anything not already committed in Task 3)

```bash
git add go.mod go.sum
git commit -m "chore: tidy go.mod after safety/config packages" --allow-empty-message 2>/dev/null || true
```

If `git status` shows no changes at this point, skip this commit — Task 3 already tidied and committed `go.mod`/`go.sum`.
