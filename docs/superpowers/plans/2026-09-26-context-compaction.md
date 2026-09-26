# Context Monitoring, Auto-Compaction & Tool-Call Visibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The TUI shows the actual command a tool call is running, the agent tracks how full the model's context window is getting (hybrid provider-usage/tokenizer estimate), and when utilization crosses 90% the agent automatically summarizes its history and keeps going instead of running out of context.

**Architecture:** A shared tool-invocation formatter (`internal/agent`) makes the `"tool"` event show real command text. A new `internal/compaction` package provides tokenizer-based estimation and an isolated LLM summarizer (same pattern as the existing safety classifier). `internal/config` gains LM-Studio-aware context-window discovery with config/default fallbacks. `internal/agent`'s `Run()` checks utilization at the top of every turn and compacts before proceeding if needed. `internal/app` renders the new `"compacted"` event.

**Tech Stack:** Go 1.26, `github.com/cloudwego/eino` (unchanged), `github.com/pkoukk/tiktoken-go` (new dependency — pure-Go BPE tokenizer, cl100k_base encoding; downloads its vocabulary data from a remote CDN on first use per machine, caches afterward).

**Spec:** `docs/superpowers/specs/2026-09-26-context-compaction-design.md`

---

## File Structure

- `internal/agent/openai.go` (modify) — extract `prettyPrintArgs`/add `formatToolInvocation`; add compaction fields/logic to `openaiAgent` and `New()`
- `internal/agent/openai_test.go` (modify) — update existing `New()` call sites for the 2 new params; add compaction-specific tests
- `internal/app/app.go` (modify) — fence the `"tool"` event's payload; add `"compacted"` case
- `internal/config/config.go` (modify) — add `ContextWindow` field to `ModelConfig`; add `DiscoverContextWindow`
- `internal/config/config_test.go` (modify) — add `DiscoverContextWindow` tests
- `internal/compaction/tokens.go` (new) — `EstimateTokens`
- `internal/compaction/tokens_test.go` (new)
- `internal/compaction/summarizer.go` (new) — `Summarizer` interface + `NewLLMSummarizer`
- `internal/compaction/summarizer_test.go` (new)
- `main.go` (modify) — wire context-window discovery and the summarizer into `agent.New`
- `go.mod` / `go.sum` (modify) — add `github.com/pkoukk/tiktoken-go`

---

### Task 1: Tool-call visibility — show the actual command, not just the tool name

**Files:**
- Modify: `internal/agent/openai.go`
- Modify: `internal/app/app.go`

- [ ] **Step 1: Extract a shared arg-formatting helper and add `formatToolInvocation`**

Find this in `internal/agent/openai.go`:

```go
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

Replace it with:

```go
func prettyPrintArgs(argsJSON string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(argsJSON), "", "  "); err == nil {
		return buf.String()
	}
	return argsJSON
}

func formatConfirmMessage(name string, c safety.Classification, argsJSON string) string {
	kind := ""
	if c.Kind != "" {
		kind = " (" + strings.ToUpper(c.Kind) + ")"
	}

	return fmt.Sprintf("%s wants to run%s [risk: %s]\n%s\nAllow? [y/n]", name, kind, c.Risk, prettyPrintArgs(argsJSON))
}

func formatToolInvocation(name, argsJSON string) string {
	return fmt.Sprintf("Running %s:\n%s", name, prettyPrintArgs(argsJSON))
}
```

- [ ] **Step 2: Use it in the `"tool"` event**

Find this in `internal/agent/openai.go`:

```go
				ch <- Event{Kind: "tool", Payload: tc.Function.Name}
```

Replace it with:

```go
				ch <- Event{Kind: "tool", Payload: formatToolInvocation(tc.Function.Name, tc.Function.Arguments)}
```

- [ ] **Step 3: Fence the `"tool"` case's payload in the TUI**

Find this in `internal/app/app.go`:

```go
		case "tool":
			// Close out the in-flight assistant block so the tool status
			// line renders as its own turn, and the reply that follows the
			// tool call starts a fresh block.
			m.streaming = false
			m.newLine("system", "Running "+msg.Payload+"...")
			return m, waitForAgentEvent(m.agentChan)
```

Replace it with:

```go
		case "tool":
			// Close out the in-flight assistant block so the tool status
			// line renders as its own turn, and the reply that follows the
			// tool call starts a fresh block.
			m.streaming = false
			m.newLine("system", "```\n"+msg.Payload+"\n```")
			return m, waitForAgentEvent(m.agentChan)
```

`msg.Payload` is now `formatToolInvocation`'s output (`"Running <name>:\n<pretty-printed args>"`), so this mirrors the same fenced-code-block treatment already used for the `"confirm"` case, for the same anti-markdown-corruption reason.

- [ ] **Step 4: Build and test**

Run: `export PATH="/Users/kkondratuk/.local/share/mise/installs/go/1.26.2/bin:$PATH" && go build ./... && go test ./internal/agent/... ./internal/app/... -v`
Expected: builds clean; all existing `internal/agent` tests still pass unchanged (they don't assert on `"tool"` event payload content, only `"tool"` vs other kinds).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/openai.go internal/app/app.go
git commit -m "feat(agent): show the actual command/args in tool-call status lines"
```

---

### Task 2: Config — context-window discovery

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go` (append to the existing file; keep all existing tests unchanged):

```go
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
```

Add `"net/http"` and `"net/http/httptest"` to `internal/config/config_test.go`'s import block (it currently imports `"context"`, `"os"`, `"path/filepath"`, `"testing"`, and `"github.com/kjkondratuk/dewey/internal/safety"`).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH="/Users/kkondratuk/.local/share/mise/installs/go/1.26.2/bin:$PATH" && go test ./internal/config/... -run TestDiscoverContextWindow -v`
Expected: FAIL — `DiscoverContextWindow`, `defaultContextWindow`, `ModelConfig.ContextWindow` undefined (compile error).

- [ ] **Step 3: Add the `ContextWindow` field**

Find this in `internal/config/config.go`:

```go
// ModelConfig is the connection details for one model tier.
type ModelConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
}
```

Replace it with:

```go
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
```

- [ ] **Step 4: Add `DiscoverContextWindow`**

Add to `internal/config/config.go` (new imports needed: `"encoding/json"`, `"net/http"`, `"strings"`, `"time"` — add these to the existing import block alongside `"context"`, `"fmt"`, `"os"`, `"path/filepath"`, the eino/yaml/safety imports):

```go
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
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: PASS (all tests, old and new).

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): discover model context window size (LM Studio API, config, default)"
```

---

### Task 3: `internal/compaction` — token estimation and LLM summarizer

**Files:**
- Create: `internal/compaction/tokens.go`
- Create: `internal/compaction/tokens_test.go`
- Create: `internal/compaction/summarizer.go`
- Create: `internal/compaction/summarizer_test.go`

- [ ] **Step 1: Add the tiktoken-go dependency**

Run: `export PATH="/Users/kkondratuk/.local/share/mise/installs/go/1.26.2/bin:$PATH" && go get github.com/pkoukk/tiktoken-go@v0.1.8`
Expected: `go.mod`/`go.sum` updated with the new dependency (and its transitive deps `github.com/google/uuid`, `github.com/dlclark/regexp2`).

Note: the package's import path is `github.com/pkoukk/tiktoken-go` but its declared package name is `tiktoken` (confirmed from source) — no import alias is needed, just `import "github.com/pkoukk/tiktoken-go"` and refer to `tiktoken.GetEncoding(...)` etc.

- [ ] **Step 2: Write the failing tests for `EstimateTokens`**

```go
// internal/compaction/tokens_test.go
package compaction

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestEstimateTokens_PositiveAndProportionalToContent(t *testing.T) {
	short := []*schema.Message{{Role: schema.User, Content: "hi"}}
	long := []*schema.Message{{Role: schema.User, Content: strings.Repeat("word ", 200)}}

	shortCount := EstimateTokens(short)
	longCount := EstimateTokens(long)

	if shortCount <= 0 {
		t.Fatalf("EstimateTokens(short) = %d, want > 0", shortCount)
	}
	if longCount <= shortCount {
		t.Errorf("EstimateTokens(long) = %d, want > EstimateTokens(short) = %d", longCount, shortCount)
	}
}

func TestEstimateTokens_EmptyHistory(t *testing.T) {
	if got := EstimateTokens(nil); got != 0 {
		t.Errorf("EstimateTokens(nil) = %d, want 0", got)
	}
}

func TestEstimateTokens_CountsToolCallsAndResults(t *testing.T) {
	withToolCall := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			Function: schema.FunctionCall{Name: "run_shell_command", Arguments: `{"command":"ls -la /some/long/path/here"}`},
		}}},
	}
	withoutToolCall := []*schema.Message{{Role: schema.Assistant}}

	if EstimateTokens(withToolCall) <= EstimateTokens(withoutToolCall) {
		t.Error("expected tool call name/arguments to contribute to the token estimate")
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/compaction/... -v`
Expected: FAIL — `internal/compaction` doesn't exist yet (build failure).

- [ ] **Step 4: Implement `EstimateTokens`**

```go
// internal/compaction/tokens.go
package compaction

import (
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/pkoukk/tiktoken-go"
)

// perMessageOverheadTokens approximates the fixed per-message formatting
// cost (role markers, message boundaries) that a raw content-only token
// count misses.
const perMessageOverheadTokens = 4

// EstimateTokens approximates the token count of a message history using a
// BPE tokenizer (cl100k_base encoding). This is an approximation — models
// other than OpenAI's use different vocabularies — but is far more
// consistent than a raw character-count heuristic. Used as a fallback/delta
// estimator alongside provider-reported usage (see internal/agent).
func EstimateTokens(messages []*schema.Message) int {
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		return 0
	}

	total := 0
	for _, msg := range messages {
		total += perMessageOverheadTokens
		total += len(enc.EncodeOrdinary(messageText(msg)))
	}
	return total
}

func messageText(msg *schema.Message) string {
	var b strings.Builder
	b.WriteString(string(msg.Role))
	b.WriteString(": ")
	b.WriteString(msg.Content)
	for _, tc := range msg.ToolCalls {
		b.WriteString(" ")
		b.WriteString(tc.Function.Name)
		b.WriteString(" ")
		b.WriteString(tc.Function.Arguments)
	}
	if msg.ToolName != "" {
		b.WriteString(" ")
		b.WriteString(msg.ToolName)
	}
	return b.String()
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/compaction/... -v`
Expected: PASS. Note: the first run on a fresh machine needs network access once — `tiktoken.GetEncoding("cl100k_base")` downloads and caches its BPE vocabulary data from a remote CDN the first time this encoding is used, then reuses the local cache on every subsequent run. If this test run has no network access, it will fail here; that's expected on a fully offline machine and not a code defect — retry with network access.

- [ ] **Step 6: Write the failing tests for `Summarizer`**

```go
// internal/compaction/summarizer_test.go
package compaction

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type fakeGenerateModel struct {
	content string
	err     error
	lastReq []*schema.Message
}

func (f *fakeGenerateModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	f.lastReq = in
	if f.err != nil {
		return nil, f.err
	}
	return &schema.Message{Role: schema.Assistant, Content: f.content}, nil
}

func (f *fakeGenerateModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	panic("not used in these tests")
}

func TestLLMSummarizer_ReturnsModelText(t *testing.T) {
	fake := &fakeGenerateModel{content: "summary text"}
	s := NewLLMSummarizer(fake)

	got, err := s.Summarize(context.Background(), []*schema.Message{{Role: schema.User, Content: "hello"}})
	if err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}
	if got != "summary text" {
		t.Errorf("Summarize() = %q, want %q", got, "summary text")
	}
}

func TestLLMSummarizer_IsolatedFromInput(t *testing.T) {
	fake := &fakeGenerateModel{content: "summary"}
	s := NewLLMSummarizer(fake)

	history := []*schema.Message{{Role: schema.User, Content: "hello"}}
	if _, err := s.Summarize(context.Background(), history); err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}

	if len(history) != 1 {
		t.Fatalf("input history was mutated: len = %d, want 1", len(history))
	}
	if len(fake.lastReq) != 2 {
		t.Fatalf("model received %d messages, want 2 (system + user transcript)", len(fake.lastReq))
	}
	if fake.lastReq[0].Role != schema.System {
		t.Errorf("first message role = %v, want schema.System", fake.lastReq[0].Role)
	}
	if !strings.Contains(fake.lastReq[1].Content, "hello") {
		t.Errorf("transcript missing original content: %q", fake.lastReq[1].Content)
	}
}

func TestLLMSummarizer_ModelErrorPropagates(t *testing.T) {
	fake := &fakeGenerateModel{err: errors.New("connection refused")}
	s := NewLLMSummarizer(fake)

	_, err := s.Summarize(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestLLMSummarizer_TranscribesToolCallsAndResults(t *testing.T) {
	fake := &fakeGenerateModel{content: "summary"}
	s := NewLLMSummarizer(fake)

	history := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			Function: schema.FunctionCall{Name: "run_shell_command", Arguments: `{"command":"ls"}`},
		}}},
		schema.ToolMessage("file1.txt\nfile2.txt", "call-1"),
	}

	if _, err := s.Summarize(context.Background(), history); err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}

	transcript := fake.lastReq[1].Content
	if !strings.Contains(transcript, "run_shell_command") {
		t.Errorf("transcript missing tool call name: %q", transcript)
	}
	if !strings.Contains(transcript, "file1.txt") {
		t.Errorf("transcript missing tool result: %q", transcript)
	}
}
```

- [ ] **Step 7: Run the tests to verify they fail**

Run: `go test ./internal/compaction/... -run TestLLMSummarizer -v`
Expected: FAIL — `NewLLMSummarizer` undefined.

- [ ] **Step 8: Implement `Summarizer`**

```go
// internal/compaction/summarizer.go
package compaction

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Summarizer condenses a message history into a single summary string.
type Summarizer interface {
	Summarize(ctx context.Context, messages []*schema.Message) (string, error)
}

const summarizerSystemPrompt = `You are summarizing an ongoing conversation between a user and an AI coding agent so the conversation can continue with a much shorter history.
Preserve: the user's overall goals and requests, key decisions made, important facts discovered (file paths, commands run, configuration values, error messages), the state of any in-progress work, and any open questions or next steps.
Omit conversational filler. Be thorough about anything needed to correctly continue the work, but keep the summary as concise as such thoroughness allows.`

type llmSummarizer struct {
	model model.BaseChatModel
}

// NewLLMSummarizer builds a Summarizer backed by m. m should be a small/fast
// model — summarization runs as an isolated call (a fresh message list),
// never touching or appending to the caller's conversation history.
func NewLLMSummarizer(m model.BaseChatModel) Summarizer {
	return &llmSummarizer{model: m}
}

func (s *llmSummarizer) Summarize(ctx context.Context, messages []*schema.Message) (string, error) {
	transcript := transcribe(messages)

	req := []*schema.Message{
		{Role: schema.System, Content: summarizerSystemPrompt},
		{Role: schema.User, Content: "Conversation to summarize:\n\n" + transcript},
	}

	resp, err := s.model.Generate(ctx, req)
	if err != nil {
		return "", fmt.Errorf("summarizer model call failed: %w", err)
	}
	return resp.Content, nil
}

func transcribe(messages []*schema.Message) string {
	var b strings.Builder
	for _, msg := range messages {
		switch {
		case len(msg.ToolCalls) > 0:
			for _, tc := range msg.ToolCalls {
				fmt.Fprintf(&b, "ASSISTANT called %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
			}
		case msg.Role == schema.Tool:
			fmt.Fprintf(&b, "TOOL[%s]: %s\n", msg.ToolName, msg.Content)
		default:
			fmt.Fprintf(&b, "%s: %s\n", strings.ToUpper(string(msg.Role)), msg.Content)
		}
	}
	return b.String()
}
```

- [ ] **Step 9: Run the tests to verify they pass**

Run: `go test ./internal/compaction/... -v`
Expected: PASS (all tests in the package).

- [ ] **Step 10: Commit**

```bash
go mod tidy
git add internal/compaction/tokens.go internal/compaction/tokens_test.go internal/compaction/summarizer.go internal/compaction/summarizer_test.go go.mod go.sum
git commit -m "feat(compaction): add token estimation and LLM-backed summarizer"
```

---

### Task 4: Agent — context tracking and automatic compaction

**Files:**
- Modify: `internal/agent/openai.go`
- Modify: `internal/agent/openai_test.go`

- [ ] **Step 1: Replace the entire contents of `internal/agent/openai.go`**

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

	"github.com/kjkondratuk/dewey/internal/compaction"
	"github.com/kjkondratuk/dewey/internal/safety"
)

// compactionThreshold is the fraction of the context window that triggers
// automatic compaction.
const compactionThreshold = 0.90

type openaiAgent struct {
	cm         model.ToolCallingChatModel
	tools      map[string]tool.InvokableTool
	descs      map[string]string
	classifier safety.Classifier
	overrides  safety.RiskOverrides
	history    []*schema.Message

	contextWindow int
	summarizer    compaction.Summarizer

	// lastKnownTokens/lastKnownAt track the most recent provider-reported
	// total token count and the history length at which it was recorded,
	// so estimateTokens only needs to estimate the delta since then.
	lastKnownTokens int
	lastKnownAt     int
}

// New builds an agent backed by cm, which must already have the tool schemas
// bound (via cm.WithTools). tools supplies the matching executable
// implementations, keyed by name, so tool calls the model requests can
// actually be run. classifier assigns a risk tier to each resolved tool
// call; overrides pins specific tool names to a fixed tier, bypassing the
// classifier for that tool. contextWindow is the model's context size in
// tokens (0 disables automatic compaction); summarizer condenses history
// when compaction triggers.
func New(
	cm model.ToolCallingChatModel,
	tools []tool.InvokableTool,
	classifier safety.Classifier,
	overrides safety.RiskOverrides,
	contextWindow int,
	summarizer compaction.Summarizer,
) *openaiAgent {
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
		cm:            cm,
		tools:         byName,
		descs:         descs,
		classifier:    classifier,
		overrides:     overrides,
		contextWindow: contextWindow,
		summarizer:    summarizer,
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

		if estimated := a.estimateTokens(); a.shouldCompact(estimated) {
			if err := a.compact(ctx, truncateLen); err != nil {
				ch <- Event{Kind: "error", Payload: "compaction failed: " + err.Error()}
				return
			}
			ch <- Event{Kind: "compacted", Payload: fmt.Sprintf(
				"Compacted conversation history (was ~%d/%d tokens, %.0f%%).",
				estimated, a.contextWindow, 100*float64(estimated)/float64(a.contextWindow))}
		}

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
			a.recordUsage(full)

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

				ch <- Event{Kind: "tool", Payload: formatToolInvocation(tc.Function.Name, tc.Function.Arguments)}
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

// recordUsage stores the provider-reported token total for full (if any) as
// a trusted baseline for future estimateTokens calls.
func (a *openaiAgent) recordUsage(full *schema.Message) {
	if full.ResponseMeta == nil || full.ResponseMeta.Usage == nil {
		return
	}
	if total := full.ResponseMeta.Usage.TotalTokens; total > 0 {
		a.lastKnownTokens = total
		a.lastKnownAt = len(a.history)
	}
}

// estimateTokens returns the current context usage: the last provider-
// reported total plus a tokenizer-based estimate of everything appended
// since, or a full tokenizer estimate if no provider total was ever
// recorded.
func (a *openaiAgent) estimateTokens() int {
	if a.lastKnownAt > 0 && a.lastKnownAt <= len(a.history) {
		return a.lastKnownTokens + compaction.EstimateTokens(a.history[a.lastKnownAt:])
	}
	return compaction.EstimateTokens(a.history)
}

// shouldCompact reports whether estimated tokens have crossed
// compactionThreshold of the context window. Compaction is disabled
// entirely when contextWindow <= 0.
func (a *openaiAgent) shouldCompact(estimated int) bool {
	return a.contextWindow > 0 && estimated >= int(float64(a.contextWindow)*compactionThreshold)
}

// compact summarizes a.history[:truncateLen] (everything before the
// current turn's just-appended user message) and replaces it with a single
// summary message, leaving the current user message untouched.
func (a *openaiAgent) compact(ctx context.Context, truncateLen int) error {
	if truncateLen == 0 {
		return nil
	}

	summary, err := a.summarizer.Summarize(ctx, a.history[:truncateLen])
	if err != nil {
		return err
	}

	summaryMsg := &schema.Message{
		Role:    schema.System,
		Content: "Summary of the conversation so far:\n" + summary,
	}
	a.history = append([]*schema.Message{summaryMsg}, a.history[truncateLen:]...)
	a.lastKnownTokens = 0
	a.lastKnownAt = 0
	return nil
}

func prettyPrintArgs(argsJSON string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(argsJSON), "", "  "); err == nil {
		return buf.String()
	}
	return argsJSON
}

func formatToolInvocation(name, argsJSON string) string {
	return fmt.Sprintf("Running %s:\n%s", name, prettyPrintArgs(argsJSON))
}

func formatConfirmMessage(name string, c safety.Classification, argsJSON string) string {
	kind := ""
	if c.Kind != "" {
		kind = " (" + strings.ToUpper(c.Kind) + ")"
	}

	return fmt.Sprintf("%s wants to run%s [risk: %s]\n%s\nAllow? [y/n]", name, kind, c.Risk, prettyPrintArgs(argsJSON))
}
```

This is the same file Task 1 already modified (extracting `prettyPrintArgs`/`formatToolInvocation`, changing the `"tool"` event's payload) — this step's full-file replacement already includes those changes, so if Task 1 already landed, this is a superset that additionally adds the compaction fields/logic. Do not apply Task 1's edits again separately; this step's content is the complete, final state of the file for both tasks combined.

- [ ] **Step 2: Update existing test call sites for the new `New()` signature**

`internal/agent/openai_test.go` currently has 4 calls to `New(m, ..., panicClassifier{} or &fakeClassifier{...}, <overrides>)`. Each needs two more trailing arguments: `0` for `contextWindow` (disables compaction — these tests aren't testing compaction) and `nil` for `summarizer` (never called when compaction is disabled).

Find each of these 4 lines in `internal/agent/openai_test.go` and update them exactly as shown:

Find:
```go
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "read", Risk: safety.Safe}}, nil)
```
Replace:
```go
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "read", Risk: safety.Safe}}, nil, 0, nil)
```

Find:
```go
	agt := New(m, []tool.InvokableTool{tl}, panicClassifier{}, safety.RiskOverrides{"do_thing": safety.Dangerous})
```
Replace:
```go
	agt := New(m, []tool.InvokableTool{tl}, panicClassifier{}, safety.RiskOverrides{"do_thing": safety.Dangerous}, 0, nil)
```

Find (this exact line appears twice — in `TestRun_DenialSkipsToolAndFeedsHistoryBack` and `TestRun_CancelDuringConfirmationDropsTheTurn` — update **both** occurrences):
```go
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "write", Risk: safety.Dangerous}}, nil)
```
Replace (both occurrences):
```go
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "write", Risk: safety.Dangerous}}, nil, 0, nil)
```

- [ ] **Step 3: Add `errors` and `strings` imports, plus a `fakeSummarizer`, to the test file**

Find this in `internal/agent/openai_test.go`:

```go
import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"github.com/kjkondratuk/dewey/internal/safety"
)
```

Replace it with:

```go
import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"github.com/kjkondratuk/dewey/internal/safety"
)
```

Then find this (right after the `panicClassifier` type definition):

```go
type echoInput struct {
	X string `json:"x"`
}
```

Replace it with:

```go
type fakeSummarizer struct {
	result  string
	err     error
	lastMsg []*schema.Message
}

func (f *fakeSummarizer) Summarize(ctx context.Context, messages []*schema.Message) (string, error) {
	f.lastMsg = messages
	if f.err != nil {
		return "", f.err
	}
	return f.result, nil
}

type echoInput struct {
	X string `json:"x"`
}
```

- [ ] **Step 4: Write the new compaction tests**

Append to `internal/agent/openai_test.go`:

```go
func TestShouldCompact(t *testing.T) {
	agt := New(nil, nil, nil, nil, 1000, nil)

	if agt.shouldCompact(899) {
		t.Error("shouldCompact(899) with window 1000 = true, want false (< 90%)")
	}
	if !agt.shouldCompact(900) {
		t.Error("shouldCompact(900) with window 1000 = false, want true (>= 90%)")
	}
}

func TestShouldCompact_DisabledWhenContextWindowIsZero(t *testing.T) {
	agt := New(nil, nil, nil, nil, 0, nil)

	if agt.shouldCompact(1_000_000) {
		t.Error("shouldCompact() with contextWindow 0 = true, want false (disabled)")
	}
}

func TestEstimateTokens_UsesProviderUsageAsBaseline(t *testing.T) {
	agt := New(nil, nil, nil, nil, 1000, nil)
	agt.history = []*schema.Message{{Role: schema.User, Content: "hi"}}

	full := &schema.Message{
		Role:         schema.Assistant,
		Content:      "hello",
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: 500}},
	}
	agt.history = append(agt.history, full)
	agt.recordUsage(full)

	if agt.lastKnownTokens != 500 {
		t.Errorf("lastKnownTokens = %d, want 500", agt.lastKnownTokens)
	}
	if agt.lastKnownAt != len(agt.history) {
		t.Errorf("lastKnownAt = %d, want %d", agt.lastKnownAt, len(agt.history))
	}

	// Anything appended after the recorded point should only add an
	// estimate on top of the trusted baseline, not recompute from scratch.
	agt.history = append(agt.history, &schema.Message{Role: schema.User, Content: "a short follow-up"})
	estimated := agt.estimateTokens()
	if estimated <= 500 {
		t.Errorf("estimateTokens() = %d, want > 500 (baseline + delta)", estimated)
	}
}

func TestRun_CompactsWhenHistoryExceedsThreshold(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		finalResponse("done"),
	}}
	fakeSumm := &fakeSummarizer{result: "the short summary"}

	// A tiny context window guarantees the pre-seeded history exceeds 90% of it.
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, nil, 10, fakeSumm)
	agt.history = []*schema.Message{
		{Role: schema.User, Content: strings.Repeat("filler content ", 200)},
		{Role: schema.Assistant, Content: strings.Repeat("more filler content ", 200)},
	}
	preCompactLen := len(agt.history)

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	if events[0].Kind != "compacted" {
		t.Fatalf("first event = %+v, want compacted", events[0])
	}
	if events[len(events)-1].Kind != "done" {
		t.Errorf("last event = %+v, want done", events[len(events)-1])
	}

	if len(fakeSumm.lastMsg) != preCompactLen {
		t.Errorf("summarizer received %d messages, want %d (pre-existing history, not the new user turn)", len(fakeSumm.lastMsg), preCompactLen)
	}
	if agt.history[0].Role != schema.System {
		t.Errorf("history[0].Role = %v, want schema.System", agt.history[0].Role)
	}
	if !strings.Contains(agt.history[0].Content, "the short summary") {
		t.Errorf("history[0].Content = %q, want it to contain the summary", agt.history[0].Content)
	}
	if agt.history[1].Content != "continue" {
		t.Errorf("history[1].Content = %q, want %q (current turn preserved)", agt.history[1].Content, "continue")
	}
}

func TestRun_CompactionFailureSurfacesAsError(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{}}
	fakeSumm := &fakeSummarizer{err: errors.New("model unavailable")}

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, nil, 10, fakeSumm)
	agt.history = []*schema.Message{
		{Role: schema.User, Content: strings.Repeat("filler content ", 200)},
	}

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var events []Event
	for evt := range ch {
		events = append(events, evt)
	}

	if len(events) != 1 || events[0].Kind != "error" {
		t.Fatalf("events = %+v, want exactly one error event", events)
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/agent/... -v -race -count=3`
Expected: PASS (all tests — the 4 original gating tests plus the 6 new compaction tests, across 3 repeats with the race detector).

- [ ] **Step 6: Commit**

```bash
git add internal/agent/openai.go internal/agent/openai_test.go
git commit -m "feat(agent): track context usage and auto-compact history at 90%"
```

---

### Task 5: App (TUI) — render the `"compacted"` event

**Files:**
- Modify: `internal/app/app.go`

- [ ] **Step 1: Add the `"compacted"` case**

Find this in `internal/app/app.go` (this reflects the file's state after Task 1's fencing change to the `"tool"` case):

```go
		case "chunk":
			m.appendChunk(msg.Payload)
			return m, waitForAgentEvent(m.agentChan)
		case "tool":
```

Replace it with:

```go
		case "compacted":
			m.newLine("system", msg.Payload)
			return m, waitForAgentEvent(m.agentChan)
		case "chunk":
			m.appendChunk(msg.Payload)
			return m, waitForAgentEvent(m.agentChan)
		case "tool":
```

`"compacted"` doesn't block on a response (unlike `"confirm"`), so it resumes `waitForAgentEvent` immediately, same as `"chunk"`/`"tool"`. Its payload (`"Compacted conversation history (was ~X/Y tokens, Z%)."`) is a plain sentence, not code/JSON, so it doesn't need the fenced-code-block treatment `"tool"`/`"confirm"` use.

- [ ] **Step 2: Build**

Run: `export PATH="/Users/kkondratuk/.local/share/mise/installs/go/1.26.2/bin:$PATH" && go build ./internal/app/...`
Expected: builds clean. (`go build ./...` as a whole will still fail at `main.go` until Task 6 updates the `agent.New` call — that's expected.)

- [ ] **Step 3: Commit**

```bash
git add internal/app/app.go
git commit -m "feat(app): render the compaction notice as a system turn"
```

---

### Task 6: Wire `main.go`

**Files:**
- Modify: `main.go`

- [ ] **Step 1: Replace the entire contents of `main.go`**

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
	"github.com/kjkondratuk/dewey/internal/compaction"
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
	summarizer := compaction.NewLLMSummarizer(simpleModel)
	contextWindow := config.DiscoverContextWindow(ctx, cfg.Intermediate)

	// register tools
	tools := tools2.NewToolFactory().DefaultInvokableToolset()
	infos := make([]*schema.ToolInfo, len(tools))
	for i, t := range tools {
		var err error
		infos[i], err = t.Info(ctx)
		if err != nil {
			log.Fatalf("failed to get tool info: %v", err)
		}
	}
	tm, err := intermediateModel.WithTools(infos)
	if err != nil {
		log.Fatalf("failed to bind tools: %v", err)
	}

	agt := agent.New(tm, tools, classifier, cfg.Overrides, contextWindow, summarizer)

	prog := tea.NewProgram(initModel(agt))
	_, err = prog.Run()
	if err != nil {
		log.Fatalf("program exited with error: %v", err)
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

This reflects `main.go`'s current state (config-driven models, the `WithTools`/`Info` error-handling hardening already in place) plus the two new additions: `summarizer`, `contextWindow`, and the updated `agent.New` call.

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: builds with no errors — the whole module compiles end-to-end again.

- [ ] **Step 3: Commit**

```bash
git add main.go
git commit -m "feat: wire context-window discovery and summarizer into main"
```

---

### Task 7: Whole-project verification

**Files:** none (verification only)

- [ ] **Step 1: Tidy dependencies**

Run: `export PATH="/Users/kkondratuk/.local/share/mise/installs/go/1.26.2/bin:$PATH" && go mod tidy`
Expected: no errors; `github.com/pkoukk/tiktoken-go` and its transitive deps appear as direct/indirect requirements as appropriate.

- [ ] **Step 2: Run the full test suite**

Run: `go test ./... -v -race`
Expected: PASS across `internal/agent`, `internal/app` (no tests — expected), `internal/compaction`, `internal/config`, `internal/safety`, `internal/tools`. Requires network access on a machine with no cached tiktoken vocabulary data (see Task 3, Step 5 note) — if this is the first time `internal/compaction` tests have run on this machine, that's expected, not a regression.

- [ ] **Step 3: Vet and build**

Run: `go vet ./... && go build ./...`
Expected: no vet warnings, clean build.

- [ ] **Step 4: Manual smoke test**

With LM Studio (or another OpenAI-compatible server) running:

Run: `go run .`

- Ask the agent to run a shell command (e.g. "list the files here") — confirm the system turn shows the actual `run_shell_command` invocation and its arguments (fenced), not just "Running run_shell_command...".
- Confirm `DiscoverContextWindow` picks up LM Studio's real context length for the loaded model (no direct UI for this in v1 — it only becomes visible indirectly, by compaction triggering at the right point; a full end-to-end compaction trigger is impractical to force manually in a short session, so this step is a best-effort spot check, not a hard requirement to complete before moving on).
