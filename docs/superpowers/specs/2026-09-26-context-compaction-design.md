# Context Monitoring, Auto-Compaction & Tool-Call Visibility — Design

## Goal

Three related additions to dewey's agent loop:

1. The TUI shows the actual shell command being run, not just the tool name.
2. The agent tracks how full the model's context window is getting.
3. When utilization crosses 90%, the agent automatically compacts its
   conversation history — summarizing everything so far into a single
   message — so the conversation can keep going instead of failing once the
   context window is exhausted.

## Non-goals

- No manual/user-triggered compaction command in this iteration — compaction
  is fully automatic and transparent, per explicit decision.
- No configurable threshold — 90% is a hardcoded constant for v1, matching
  the project's existing preference for simple constants over premature
  configurability (e.g. the classifier's fixed risk-tier thresholds).
- No partial/sliding-window compaction (keep last N messages verbatim +
  summarize the rest) — v1 replaces the entire prior history with one
  summary message. A sliding window is a reasonable future refinement, not
  built now.
- No context-window tracking for the `Simple`/`Hard`/`Embedding` tiers —
  only `Intermediate` (the main conversation model) accumulates growing
  history that needs monitoring.

## Components

### 1. Tool-call visibility fix (`internal/agent`)

Today, `Run()`'s `"tool"` event carries only `tc.Function.Name` as `Payload`,
so the TUI shows "Running run_shell_command..." with no indication of what
command is actually executing. `formatConfirmMessage` (added in the safety
feature) already knows how to pretty-print a tool's JSON arguments for the
confirmation prompt — that logic is extracted into a shared helper,
`formatToolInvocation(name, argsJSON string) string`, reused by both the
`"confirm"` event's message and the `"tool"` event's `Payload`. No new event
kind; just richer payload content on an existing one. `app.go` already wraps
system-turn payloads it renders in fenced code blocks where needed for the
confirm case — the `"tool"` case gets the same treatment for consistency
(the tool name/command display, not just raw argument dumps, could contain
markdown-special characters via user/tool-supplied strings).

### 2. Context-window discovery (`internal/config`)

```go
// ModelConfig gains one new optional field:
type ModelConfig struct {
	BaseURL       string `yaml:"base_url"`
	APIKey        string `yaml:"api_key"`
	Model         string `yaml:"model"`
	ContextWindow int    `yaml:"context_window"` // optional override
}

// DiscoverContextWindow resolves the context window size for a tier's
// model, trying progressively less precise sources:
//  1. LM Studio's native API (best-effort, silently ignored on failure)
//  2. the tier's explicit ContextWindow config field, if set
//  3. a hardcoded conservative default
func DiscoverContextWindow(ctx context.Context, m ModelConfig) int
```

LM Studio exposes a native (non-OpenAI-compatible) endpoint at
`<base>/api/v0/models` — distinct from the standard `<base>/v1/models` — that
returns per-model metadata including `max_context_length` and (for the
currently-loaded model) `loaded_context_length`. `DiscoverContextWindow`:

1. Derives the native API base by trimming a trailing `/v1` or `/v1/` from
   `m.BaseURL`, then does a `GET <base>/api/v0/models` with a short timeout
   (e.g. 2s) — this is a local network call in dewey's normal use case, no
   reason for it to be slow.
2. On success, finds the entry whose `id` matches `m.Model`, and returns
   `loaded_context_length` if present and non-zero, else `max_context_length`
   if present and non-zero.
3. On ANY failure (network error, non-2xx, malformed JSON, no matching
   model, or both fields zero/absent) — silently falls through, no error
   returned, no log noise. This is expected and normal for any non-LM-Studio
   OpenAI-compatible endpoint.
4. If step 1-3 didn't produce a value, use `m.ContextWindow` if it's
   non-zero (explicit YAML config: `context_window: 32768`).
5. If that's also zero, use the hardcoded default `8192`.

This runs once at startup in `main.go`, only for `cfg.Intermediate`, and the
resulting `int` is passed into `agent.New`.

### 3. Token estimation & compaction (`internal/compaction`, new package)

New package name avoids colliding with the `context` stdlib import that
every file touching this code also needs.

```go
package compaction

// EstimateTokens approximates the token count of a message history using
// a BPE tokenizer (github.com/pkoukk/tiktoken-go, cl100k_base encoding).
// This is an approximation for non-OpenAI models (different models use
// different vocabularies) but is far more consistent than a raw
// character-count heuristic, and costs nothing at runtime once the
// tokenizer's vocabulary file is cached locally (see note below).
func EstimateTokens(messages []*schema.Message) int

// Summarizer condenses a message history into a single summary string.
type Summarizer interface {
	Summarize(ctx context.Context, messages []*schema.Message) (string, error)
}

// NewLLMSummarizer builds a Summarizer backed by m (intended to be the
// Simple tier — the same "fast/cheap" model used for risk classification).
// Like the classifier, this is an isolated call: a fresh message list,
// never touching the caller's conversation history directly.
func NewLLMSummarizer(m model.BaseChatModel) Summarizer
```

**Known trade-off:** `pkoukk/tiktoken-go` downloads its BPE vocabulary data
from a remote CDN the first time an encoding is used, caching it locally
afterward (respects `TIKTOKEN_CACHE_DIR` if set, otherwise a default OS
cache directory). This means the very first compaction-adjacent token
estimate on a fresh machine requires one outbound network call, even though
dewey's actual model traffic is local. This is called out explicitly rather
than silently accepted — it's a one-time cost per machine, not per run, and
consistent with how `go mod` downloads dependencies once and caches them.

`EstimateTokens` serializes each message into a text approximation of what
actually gets sent over the wire (role + content + any tool-call
name/arguments + tool-result content), encodes it, sums token counts, and
adds a small fixed per-message overhead (a handful of tokens, mirroring
OpenAI's own documented per-message accounting) — again, an estimate, not
exact.

**Summarization prompt** instructs the model to preserve: the user's overall
goals and requests, key decisions made, important facts discovered (file
paths, commands run, configuration values, error messages), the state of
any in-progress work, and open questions/next steps — while omitting
conversational filler. The prior history is serialized into a plain-text
transcript (`ROLE: content` / `TOOL[name](args): result` lines) as the
summarizer's user message.

### 4. Agent wiring (`internal/agent`)

`openaiAgent` gains:

```go
type openaiAgent struct {
	// ...existing fields...
	contextWindow   int
	summarizer      compaction.Summarizer
	lastKnownTokens int // most recent provider-reported total, if any
	lastKnownAt     int // len(a.history) at which lastKnownTokens was recorded
}

const compactionThreshold = 0.90

func New(
	cm model.ToolCallingChatModel,
	tools []tool.InvokableTool,
	classifier safety.Classifier,
	overrides safety.RiskOverrides,
	contextWindow int,
	summarizer compaction.Summarizer,
) *openaiAgent
```

**Token tracking (hybrid, per earlier decision):** after each completed
model turn (`full` message appended to history), if
`full.ResponseMeta.Usage.TotalTokens > 0` (the provider actually reported
usage — reliable on `Generate`, opportunistic on `Stream`), record it as a
trusted baseline: `lastKnownTokens = full.ResponseMeta.Usage.TotalTokens`,
`lastKnownAt = len(a.history)`. `estimateTokens()` returns
`lastKnownTokens + EstimateTokens(a.history[lastKnownAt:])` when a baseline
exists (`lastKnownAt > 0`), or `EstimateTokens(a.history)` from scratch
otherwise — so real provider numbers are trusted when available, and only
the *delta* since the last real number needs estimating.

**Compaction check:** at the very top of `Run()`'s goroutine, before the
existing streaming loop begins (history is always in a clean,
no-pending-tool-calls state between `Run()` calls, so this is a safe,
simple place to check) — compare `estimateTokens()` against
`int(float64(contextWindow) * compactionThreshold)`. If over:

1. Summarize `a.history[:truncateLen]` (everything before the just-appended
   new user turn — `truncateLen` is already captured at the top of `Run()`
   for the existing cancellation-rollback logic, and is reused here).
2. Replace history: `a.history = append([]*schema.Message{summaryMsg},
   a.history[truncateLen:]...)` — i.e. one system-role summary message
   followed by the current turn's user message, which is preserved
   untouched.
3. Reset `lastKnownTokens = 0`, `lastKnownAt = 0` (stale baseline, history
   shape changed).
4. Emit `Event{Kind: "compacted", Payload: <description>}` — e.g. "Compacted
   conversation history (was ~184000/200000 tokens, 92%)." — then continue
   into the normal streaming loop as usual. This event does not block on a
   response (unlike `"confirm"`); the TUI just renders it as a system turn
   and keeps reading.
5. If `truncateLen == 0` (nothing to summarize — first-ever turn), skip
   entirely; this also can't realistically happen since utilization would
   be ~0% for an empty history.
6. If the summarizer call itself fails, emit an `"error"` event and return,
   same as any other unrecoverable failure in this loop — don't silently
   proceed with an over-budget context.

### 5. TUI (`internal/app`)

New `case "compacted":` in `Update()`'s `agentEventMsg` switch — renders the
payload as a system turn (fenced, consistent with `"confirm"`/`"tool"`) and
resumes `waitForAgentEvent` (this event doesn't block on a response, unlike
`"confirm"`).

### 6. `main.go`

- Call `config.DiscoverContextWindow(ctx, cfg.Intermediate)` once at
  startup.
- Build `summarizer := compaction.NewLLMSummarizer(simpleModel)` (reusing
  the same `simpleModel` client already built for the classifier).
- Pass both into `agent.New(...)`.

## Data flow (compaction path)

1. User submits a new message → `Run()` appends it, captures `truncateLen`
   (pre-append length).
2. Goroutine starts; before streaming, checks `estimateTokens() >=
   0.90 * contextWindow`.
3. If over: summarize prior history (isolated call to `Simple` tier) →
   replace history with `[summary, current user message]` → emit
   `"compacted"`.
4. Normal turn proceeds: stream response, resolve any tool calls (gated by
   the existing safety-classification flow), append results, loop until
   done.

## Error handling & edge cases

- LM Studio discovery failures are silent and expected (any non-LM-Studio
  provider hits this every time) — never surfaced to the user, never
  logged.
- A missing/zero context window after all three discovery steps can't
  happen (step 3, the hardcoded default, always produces a positive
  number) — `contextWindow` is always `> 0` by the time `agent.New` is
  called.
- Summarizer failure ends the turn with an `"error"` event rather than
  silently continuing over-budget or silently skipping compaction.
- The just-submitted user message is never lost or summarized away — it's
  always preserved verbatim after compaction, only prior turns are
  condensed.

## Testing

- `internal/config`: `DiscoverContextWindow` — LM Studio-shaped success
  response, malformed/unreachable endpoint (falls through to config field),
  no config field either (falls through to default), `loaded_context_length`
  preferred over `max_context_length` when both present.
- `internal/compaction`: `EstimateTokens` produces a positive, roughly
  content-size-proportional number for representative message histories
  (exact token counts aren't asserted, since the encoding is an
  approximation by design); `LLMSummarizer` — isolated call (doesn't touch
  a shared history), returns the model's text, propagates model errors.
- `internal/agent`: table-driven tests extending the existing gating
  tests — utilization under threshold never compacts; utilization over
  threshold triggers compaction before the turn proceeds, preserves the
  current user message, resets tracking state, and emits `"compacted"`;
  provider-reported `Usage` is correctly used as a baseline for subsequent
  estimates; summarizer failure surfaces as an `"error"` event.
- Manual: run against the live LM Studio instance, confirm
  `DiscoverContextWindow` picks up the real `max_context_length` reported
  for the loaded model.
