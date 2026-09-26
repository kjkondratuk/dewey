# Tool Safety Classification & Confirmation — Design

## Goal

Prevent the agent from silently executing tool calls that have real-world
side effects. Every tool call the model requests is classified per-invocation
into a risk tier; tiers above "safe" pause the TUI for an explicit y/n
confirmation before the call executes. Individual tools can be pinned to a
fixed risk tier via config, bypassing classification.

As part of the same effort, the single hardcoded chat model in `main.go` is
replaced with a tiered model configuration (`simple` / `intermediate` /
`hard`), plus a reserved (unwired) `embedding` model slot for future use.

## Non-goals

- No bulk/session-wide "allow all" approval — every moderate/dangerous call
  confirms individually.
- No routing of the main conversation across tiers by task difficulty —
  `intermediate` is unconditionally the main agent's model. `hard` and
  `embedding` are configured but not called from any code path yet.
- No per-tool code-level classification hooks. Classification is either fully
  automatic (via the `simple`-tier classifier model) or fully pinned (via
  config), never hardcoded per tool in Go.

## Components

### `internal/safety` (new package)

```go
type RiskTier int

const (
    Safe RiskTier = iota
    Moderate
    Dangerous
)

type Classification struct {
    Kind string // "read" | "write" — informational only, doesn't affect gating
    Risk RiskTier
}

type Classifier interface {
    Classify(ctx context.Context, toolName, toolDesc, argsJSON string) (Classification, error)
}
```

- `llmClassifier` is the concrete `Classifier`. It holds a `model.ChatModel`
  (the resolved `simple`-tier model) and issues a single, isolated
  `Generate` call per classification — a fresh message list, never appended
  to the main agent's conversation history.
- Prompt: system instructions describing the task, plus a user message
  containing the tool name, its description (from `schema.ToolInfo.Desc`),
  and the raw resolved arguments JSON. The model is instructed to respond
  with strict JSON: `{"kind":"read"|"write","risk":"safe"|"moderate"|"dangerous"}`.
- Parsing is tolerant of surrounding whitespace/text but requires a valid
  JSON object with recognized enum values. On any error — network failure,
  timeout, malformed/missing JSON, unrecognized enum value — `Classify`
  returns `{Kind: "write", Risk: Dangerous}` (fail-safe: never silently
  downgrade risk on failure).

- `RiskOverrides map[string]RiskTier` — a flat map from tool name to a
  pinned risk tier, loaded from config (see below). A tool present in
  `RiskOverrides` skips `Classifier.Classify` entirely — no LLM round trip,
  no `Kind` label (the confirmation prompt just shows the pinned risk).

### `internal/config` (new package)

Loads and merges a unified YAML config from two locations:

- `~/.dewey/config.yaml` (user-global)
- `./.dewey/config.yaml` (project-local, relative to CWD)

Merge rule: load global first, then project; for every key set in both
(model tiers by name, override entries by tool name), the project value
wins. Keys set in only one file still apply from that file. This merge is
per-key, not whole-file — a project config that only sets `overrides` still
inherits the global `models` section, and vice versa.

Malformed YAML in either file is a fatal startup error (`log.Fatalf`) — this
is safety-relevant config; failing loudly beats silently ignoring it.

```yaml
models:
  simple:
    base_url: "http://localhost:1234/v1"
    api_key: "lm-studio"
    model: "some-small-model"
  intermediate:
    base_url: "http://localhost:1234/v1"
    api_key: "lm-studio"
    model: "gemma-4-31b-it-turboquant-mlx"
  hard:
    base_url: ""
    api_key: ""
    model: ""
  embedding:
    base_url: ""
    api_key: ""
    model: ""

overrides:
  read_file: safe
  list_dir: safe
```

Defaults applied after merge, before use:

- `intermediate`: if the section (or any field) is unset, defaults to
  exactly today's hardcoded values — `http://localhost:1234/v1`,
  `lm-studio`, `gemma-4-31b-it-turboquant-mlx`. An empty/missing config file
  must produce identical behavior to today's `main.go`.
- `simple`: if unset, falls back to the fully-resolved `intermediate`
  config (same endpoint, same model) rather than guessing at a small-model
  name that may not exist on the user's local server.
- `hard` / `embedding`: `base_url` defaults to the local LM Studio endpoint
  (`http://localhost:1234/v1`) when unset, per the same "default to local"
  rule, but these tiers are otherwise only parsed and stored — nothing in
  this feature constructs a client from them.

`internal/config` exposes:

```go
type ModelConfig struct {
    BaseURL string
    APIKey  string
    Model   string
}

type Config struct {
    Simple       ModelConfig
    Intermediate ModelConfig
    Hard         ModelConfig
    Embedding    ModelConfig
    Overrides    safety.RiskOverrides
}

func Load(projectDir string) (Config, error)

func BuildChatModel(ctx context.Context, m ModelConfig) (model.ToolCallingChatModel, error)
```

`BuildChatModel` wraps `openai.NewChatModel` (the same eino OpenAI-compatible
client already in use) and is called for both `Simple` and `Intermediate` in
`main.go`. `Hard` and `Embedding` are not passed through it anywhere in this
feature.

### `internal/agent/agent.go` changes

- `Event` gains an optional response channel field for the new event kind:

  ```go
  type Event struct {
      Kind    string
      Payload string
      Confirm chan<- bool // set only when Kind == "confirm"
  }
  ```

- `openaiAgent` gains `classifier safety.Classifier` and
  `overrides safety.RiskOverrides` fields, set via `agent.New(...)`.

- Before executing each requested tool call:
  1. Look up `overrides[tc.Function.Name]`. If present, use that `RiskTier`
     directly (no `Kind`, no classifier call).
  2. Otherwise call `classifier.Classify(ctx, name, desc, tc.Function.Arguments)`.
  3. If the resulting risk is `Safe`, run the tool immediately, as today.
  4. If `Moderate` or `Dangerous`, build a human-readable description
     (tool name, kind if known, risk tier, pretty-printed arguments JSON),
     create a `resp := make(chan bool, 1)`, emit
     `Event{Kind: "confirm", Payload: description, Confirm: resp}`, then:

     ```go
     select {
     case allowed := <-resp:
         if !allowed {
             a.history = append(a.history, schema.ToolMessage(
                 "error: user denied permission for this action", tc.ID))
             continue
         }
     case <-ctx.Done():
         // same handling as the existing mid-stream cancellation path:
         // drop the user turn, emit a "cancelled" event, return.
     }
     ```
  5. On allow, proceed to `t.InvokableRun` exactly as today.

### `internal/app/app.go` changes

- New `pendingConfirm *chan<- bool` field on `App`.
- `agentEventMsg` gains a `case "confirm"`:
  - Render `msg.Payload` as a new `"system"` turn.
  - Store `msg.Confirm` in `m.pendingConfirm`.
  - Do **not** call `waitForAgentEvent` yet — the agent goroutine is
    blocked on the response channel, not the event channel.
- `Update`'s `tea.KeyPressMsg` handling checks `m.pendingConfirm != nil`
  first, before any other key handling:
  - `y` → send `true`, clear `pendingConfirm`, append a system turn
    ("Allowed."), resume `waitForAgentEvent(m.agentChan)`.
  - `n` or `esc` → send `false`, clear `pendingConfirm`, append a system
    turn ("Denied."), resume `waitForAgentEvent(m.agentChan)`.
  - Any other key is ignored while a confirmation is pending — text entry
    does not accept input.
  - `esc` specifically also still calls `m.cancel()` as today, so a pending
    confirmation can be abandoned entirely (unblocks the agent via
    `ctx.Done()` in the select above, taking the cancellation path instead
    of the denial path).

### `main.go` changes

- Load config via `config.Load(cwd)`.
- Build `simpleModel` and `intermediateModel` via `config.BuildChatModel`.
- Construct `safety.NewLLMClassifier(simpleModel)`.
- Pass `intermediateModel` (with `WithTools`, as today), the classifier, and
  `cfg.Overrides` into `agent.New(...)`.
- Remove the hardcoded `openai.ChatModelConfig{...}` literal and the
  commented-out Claude scaffold — both are superseded by config-driven
  construction. (The commented Claude experiment can be revisited later by
  pointing the `hard` tier at it once that tier is wired up.)

## Data flow

1. Model requests a tool call.
2. Agent resolves classification (override lookup, else classifier call
   against the `simple`-tier model, isolated from conversation history).
3. `Safe` → runs immediately, no UI interruption, loop continues.
4. `Moderate`/`Dangerous` → agent emits `confirm` event and blocks on
   `select{resp, ctx.Done()}`. TUI renders:
   ```
   ⚠ web_request wants WRITE access (risk: dangerous)
   {"method":"POST","url":"https://example.com/foo","body":"..."}
   Allow? [y/n]
   ```
5. User presses `y`/`n`/`esc` → response sent → agent unblocks → executes
   or records denial in history → loop continues, streams the model's next
   response (which may reference the denial).

## Error handling & edge cases

- Classifier failure/timeout → fails safe to `dangerous`/`write`, never
  silently allows.
- Denial → appended to history as a tool-result error message; the turn is
  **not** aborted, matching how unknown-tool and tool-execution errors are
  already handled in `agent.go`.
- `esc` while a confirmation is pending → cancels the context, which the
  agent's `select` observes, taking the existing cancellation path (drops
  the user's turn, emits `"cancelled"`).
- Malformed `~/.dewey/config.yaml` or `./.dewey/config.yaml` → fatal startup
  error.
- Missing config entirely (no `~/.dewey/` or `./.dewey/` directories) →
  fully defaulted config, behavior identical to today plus classification
  now running against the `intermediate` model (since `simple` falls back
  to it).

## Testing

- `internal/config`: merge precedence (project overrides global per-key,
  keys unique to one file survive), default application for each tier,
  malformed-YAML fatal path.
- `internal/safety`: classifier JSON parsing (valid, malformed, missing
  fields, model error) — all non-happy paths must resolve to
  `{Kind: "write", Risk: Dangerous}`; override lookup short-circuits the
  classifier.
- `internal/agent`: table-driven tests over the gating logic — safe tier
  skips confirmation, moderate/dangerous emits the event and blocks,
  allow/deny/cancel all resume the loop correctly and produce the expected
  history entries.
- Manual: run the TUI against a locally running LM Studio instance, trigger
  a write-classified `web_request` call (e.g. ask the agent to POST
  somewhere), confirm the prompt renders and both `y` and `n` behave as
  designed.