# Plan: Tool-call UX and busy-state handling

Date: 2026-09-27
Design: `docs/superpowers/specs/2026-09-27-tool-ux-and-busy-state-design.md`

Five tasks, each independently committable and testable. Rationale for every
decision lives in the design doc; this file is the execution order and the
acceptance criteria.

---

## Task 1 — `internal/safety`: plain-English summary

**`risk.go`** — add to `Classification`:

```go
// Summary is a one-sentence plain-English description of what the call
// does, shown in the confirmation prompt so a user who can't read the
// tool's argument syntax can still evaluate it. Cosmetic and best-effort:
// empty when the classification came from a config override, from the
// fail-safe path, or when the model omitted it.
Summary string
```

**`classifier.go`** — add a third bullet to `classifierSystemPrompt` asking for
`"summary"`: one short sentence, plain English, written for someone who cannot
read shell syntax, naming the *effect* rather than the flags. Update the required
response shape in the prompt to include the field.

Add `Summary string \`json:"summary"\`` to the anonymous parse struct and carry
`strings.TrimSpace(parsed.Summary)` into the returned `Classification`.

**Critical:** `summary` must not participate in validation. A missing or empty
summary returns a normal `Classification`, never `failSafeClassification` — only
`kind`/`risk` remain fatal.

**Tests:** summary parsed and trimmed; empty/absent summary still yields a valid
classification with the correct kind and risk; a malformed `kind` still fails
safe even when a summary is present.

---

## Task 2 — `internal/agent`: denial wording, named results, argument rendering

**Denial result.** Replace `"error: user denied permission for this action"` with
a `deniedToolResult` const that never uses the word "error", states outright that
the capability exists and the user declined this specific call, and tells the
model not to retry but to acknowledge and offer an alternative. Leave
`error: unknown tool X` unchanged — the contrast is the point.

**Named tool results.** Pass `schema.WithToolName(tc.Function.Name)` to all four
`schema.ToolMessage(...)` call sites (unknown-tool, denial, success, exec error).

**Argument rendering.** Add:

- `describeArgs(argsJSON)` — single-string-field JSON objects render bare (real
  newlines, no escaping); everything else falls back to `prettyPrintArgs`.
- `fence(body)` — wraps in a backtick fence sized to `max(3, longestRun+1)` so
  command text containing backticks cannot terminate the block early. This
  guards the same markdown-corruption class that was fixed once before by
  fencing the whole payload.
- `formatToolInvocation(name, argsJSON)` — `Running <name>:` + fenced args.
- `formatToolInvocationBrief()` — the bare unfenced line `Running…`.
- `formatConfirmMessage` — header, blank line, summary (omitted when empty),
  blank line, fenced args, `Allow? [y/n]`.

**Brief-vs-full choice.** Track a per-tool-call `confirmed` bool in the tool-call
loop; set it true only after an *allowed* confirmation. Emit
`formatToolInvocationBrief()` when confirmed, `formatToolInvocation(...)`
otherwise.

**Tests:** denial result contains no `error` prefix and mentions the user's
decision; tool messages carry `ToolName`; `describeArgs` unwraps a lone string,
preserves real newlines, and falls back to JSON for multi-field and malformed
input; `fence` escapes bodies containing ``` runs; confirm output includes the
summary when present and omits the line when empty; an approved call emits the
brief payload while an auto-run safe call emits the full one.

---

## Task 3 — `internal/app`: stop double-fencing

`app.go` currently wraps `"tool"` and `"confirm"` payloads in `"```\n"+…+"\n```"`.
The agent now emits complete markdown, so render both verbatim:

```go
m.newLine("system", msg.Payload)
```

Applies to both the `"confirm"` and `"tool"` cases. No other behavior change.

---

## Task 4 — `internal/app`: thinking indicator

Add to `App`: `spinner spinner.Model`, `busy bool`.

- `Init()` constructs the spinner (`spinner.MiniDot`) so `main.go`'s struct
  literal stays valid and unchanged.
- Starting a query sets `busy = true` and returns
  `tea.Batch(waitForAgentEvent(ch), m.spinner.Tick)`.
- `spinner.TickMsg` is forwarded to the spinner **only while busy**, so the tick
  chain terminates on its own once the turn ends.
- `"cancelled"`, `"error"`, `"done"` clear `busy`.

**Layout.** The status line *replaces* the input line rather than adding a row,
keeping the total at `height - 1` so `layout()` needs no change and the
conversation never resizes mid-turn. While busy the bottom row is:

- awaiting confirmation → `Awaiting your approval — y to allow, n to deny, esc to cancel`
- otherwise → spinner + `Thinking… (<hint>)`

The cursor is only published when the input is actually visible (`!m.busy`).

---

## Task 5 — `internal/app`: input gating and double-escape abort

Add `abortArmed bool`, `abortDisarmMsg` type, and `abortArmWindow = 1500ms`.

A `if m.busy { … }` block goes *after* the existing `pendingConfirm` block (which
keeps its single-escape cancel) and before normal key handling:

- `ctrl+c` → quit
- `esc` → if armed, cancel the context and disarm; else arm and schedule a
  `tea.Tick` emitting `abortDisarmMsg`
- `pageup`/`pagedown`/`up`/`down` → scroll (the user must be able to read output
  while waiting)
- anything else → swallowed; input is disabled

Starting a query blurs `Entry`; the terminal events re-`Focus()` it (returning
the resulting `tea.Cmd`). `abortDisarmMsg` clears `abortArmed`.

This also closes the pre-existing reentrancy gap where `enter` during an in-flight
turn could start a second `Run()` racing the first on `a.history`.

**Tests:** this was originally planned as a coverage gap (`internal/app` had no
test file, and the Bubble Tea model looked awkward to drive headlessly). Review
showed the gap was hiding a real bug, so `internal/app/app_test.go` now drives
`Update` directly with synthetic `tea.KeyPressMsg`/`agentEventMsg` values and a
fake agent. It covers the busy/focus lifecycle across every terminal event, the
double-escape abort including the stale-timer case, confirm-prompt interaction,
spinner tick gating, status-line width clamping, and cursor publication.

---

## Task 6 — Verification

`go build ./... && go vet ./... && go test ./... -race -count=3`, then a review
pass over the whole diff (spec compliance + code quality), then a manual smoke
run of the TUI.
