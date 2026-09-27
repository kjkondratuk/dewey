# Design: Tool-call UX and busy-state handling

Date: 2026-09-27

## Problem

Six distinct complaints, all about the human-facing edges of the tool-call loop:

1. The agent doesn't distinguish "I lack access to do that" from "the user denied
   this specific call" — it responds to a denial as though it were a capability
   limitation.
2. Shell commands are displayed as the raw JSON argument object, so
   `{"command": "grep -rn foo .\n"}` shows JSON ceremony and escaped `\n`
   sequences instead of readable command text.
3. After the user approves a call, the full command text is printed *again* when
   it executes — redundant noise.
4. The confirmation prompt shows only the raw command, so a user who can't read
   shell syntax has no basis for deciding whether to approve.
5. Nothing indicates that a submitted query was received while the model is
   thinking — the UI just sits there.
6. Input stays live while the agent is working, so the user can submit a second
   query into a busy agent (which races on `a.history`). There's no deliberate
   abort gesture.

## Root causes

**Item 1** has two contributing causes, both confirmed by reading the code:

- `internal/agent/openai.go` records a denial as
  `schema.ToolMessage("error: user denied permission for this action", tc.ID)`.
  The `error:` prefix makes a *human decision* look like a *tool failure*, which
  is exactly the conflation being reported. The model has no way to tell this
  apart from `error: unknown tool X` (which is a genuine capability error).
- Every tool result is built with `schema.ToolMessage(content, id)` and never
  sets `ToolName`, even though `schema.WithToolName(name)` exists. Local chat
  templates (notably Gemma's, which is the configured default model) render tool
  results by name; an unnamed result is presented to the model less clearly.

Separately noted but **out of scope**: `prompts/system.md` exists in the repo but
is not referenced by any Go file — there is no system prompt in the conversation
at all. Its template placeholders (`.Memory`, `.Conversation`,
`.CurrentInstruction`) imply a single-shot prompt-template architecture rather
than the message-history architecture the agent actually uses, so wiring it is a
separate design question. The item-1 fix below does not depend on it.

**Items 2–4** are all formatting concerns that currently live in two agent-side
formatters (`formatToolInvocation`, `formatConfirmMessage`) plus a rendering
decision in `internal/app/app.go`, which wraps both payloads in a ``` fence.
Splitting the formatting decision across two packages is what makes item 3
awkward to fix — the agent doesn't know what the UI already showed.

**Items 5–6** are pure TUI state: `App` has a `streaming` flag but no notion of
"a query is in flight," so it can neither render a waiting indicator nor gate
input.

## Design

### 1. Unambiguous denial, and named tool results

Reword the denial result so it cannot be read as a capability error, and state
the expected follow-up behavior explicitly:

```
The user was shown this exact tool call and chose to DENY it. This is not an
error and not a missing capability — the tool works and you are allowed to use
it, but the user declined this specific invocation. Do not retry the same call.
Briefly acknowledge the denial, then either propose a different approach or ask
the user how they'd like to proceed.
```

Keep `error: unknown tool X` as-is — that one genuinely *is* a capability error,
and the contrast is now meaningful.

Attach the tool name to every tool result via `schema.WithToolName(name)`:
denial, unknown-tool, success, and execution-failure paths alike.

### 2. Render the salient argument, not the JSON

Add `describeArgs(argsJSON string) string`:

- Parse as a JSON object. If it has exactly one field and that field's value is
  a string, return the string bare. This covers `run_shell_command`'s
  `{"command": "..."}` — the only tool today — and yields real newlines instead
  of escaped `\n`.
- Otherwise fall back to the existing indented-JSON rendering.

The single-string-field heuristic is deliberately generic rather than
special-casing the literal key `"command"`: any future single-argument tool gets
readable output for free, and multi-argument tools degrade to JSON rather than
silently dropping fields.

### 3. Formatters own their own fencing

Move the ``` fence decision out of `app.go` and into the agent's formatters, so
the agent can vary its output density. `app.go` renders `"tool"` and
`"confirm"` payloads verbatim as system turns.

- `formatToolInvocation(name, argsJSON)` → fenced block containing the described
  args, for calls that run without confirmation.
- `formatToolInvocationBrief()` → the bare line `Running…`, unfenced, for calls
  the user *just* approved. The command is already on screen directly above.
- `formatConfirmMessage(name, c, argsJSON)` → header line, the plain-English
  summary (when present), a fenced block with the described args, then the
  `Allow? [y/n]` prompt.

The agent decides which invocation formatter to use based on whether that
specific tool call went through confirmation.

### 4. Plain-English summary from the classifier

The classifier already makes one LLM call per tool call *before* the
confirmation prompt is shown, so the summary costs nothing extra.

- Add `Summary string` to `safety.Classification`.
- Extend the classifier prompt to also return `"summary"`: one short sentence,
  plain English, no jargon, describing what the call does and what it would
  change.
- `Summary` is cosmetic, so a missing or empty value is **not** fatal — unlike
  `kind`/`risk`, it does not trip the fail-safe path. `failSafeClassification`
  carries no summary, and config-override classifications have none either (same
  as `Kind` today); the confirm prompt simply omits the line.

### 5. Thinking indicator

Add a `spinner.Model` (`charm.land/bubbles/v2/spinner`, `MiniDot`) to `App`,
plus a `busy` flag meaning "a query is in flight and we are not currently
streaming visible text."

Render below the conversation and above the input: `⠙ Thinking…`, or
`⠙ Thinking… (esc esc to abort)` once the hint is relevant.

The spinner ticks via `spinner.TickMsg`; its `Tick` command is batched with
`waitForAgentEvent` when a query starts, and `spinner.TickMsg` is forwarded to
the spinner in `Update`. Ticking stops when `busy` goes false, so there is no
runaway timer after the turn ends.

### 6. Input gating and double-escape abort

- While a query is in flight, `Entry` is blurred and key events are not
  forwarded to it, so typing does nothing and `enter` cannot submit a second
  query. This also closes the pre-existing reentrancy gap where a second
  `Run()` could race the first on `a.history`.
- Abort requires **two** escapes: the first arms and shows the hint, the second
  cancels the context. A `tea.Tick` disarms after 1.5s so a stray escape doesn't
  leave the abort primed indefinitely.
- The `y`/`n` confirmation prompt keeps its existing **single**-escape cancel.
  It is an explicit question with a cursor waiting on an answer, where escape
  conventionally means "never mind" — and the command text is right there, so
  there is no accidental-abort risk to protect against.

## Files touched

| File | Change |
|---|---|
| `internal/safety/risk.go` | `Summary` field on `Classification` |
| `internal/safety/classifier.go` | prompt + parsing for `summary` |
| `internal/agent/openai.go` | denial wording, `WithToolName`, `describeArgs`, three formatters, brief-vs-full invocation choice |
| `internal/app/app.go` | verbatim payload rendering, spinner, `busy` gating, double-esc |
| corresponding `_test.go` files | coverage for all of the above |

No changes to `main.go`, config, or the compaction package.

## Out of scope

- Wiring `prompts/system.md` / introducing a system prompt (see above).
- Per-tool argument renderers beyond the single-string-field heuristic.
- Making the summary available for config-override classifications (would need a
  classifier call for tools the override exists specifically to skip).
