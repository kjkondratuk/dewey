# Design: Claude Code config interoperability

Date: 2026-10-04

## Problem

dewey has two tools — `run_shell_command` and `remember` — and no way to acquire
more. Meanwhile Claude Code on the same machine has an installed ecosystem
sitting on disk: five marketplaces, five enabled plugins, two plugin-bundled MCP
servers, roughly thirty-five skills, several subagents, and session hooks. All of
it is plain JSON, YAML, and markdown under `~/.claude`.

Rebuilding any of that for dewey would be wasted work. The formats are readable
and the content is already curated by the person using it. dewey should consume
it.

The expensive part is not reading the config. It is that these specs are written
against a harness dewey doesn't have: Claude's tool vocabulary, a tool-search
mechanism for large tool sets, subagent dispatch, and a hook protocol. The config
layer is a few hundred lines. The harness it implies is six subsystems.

## Goals

- Run imported skills, subagents, and slash commands **as written**, without
  editing them.
- Connect to MCP servers declared in Claude's config and in plugin `.mcp.json`
  files, exposing their tools to the model.
- Stay current automatically: a plugin installed through Claude Code's
  `/plugin` is available in dewey on next launch, with no sync step.
- Adapt to whatever context window is actually available. dewey's resolved
  window ranges from 8192 (the no-server fallback) to 262144 (what LM Studio
  reports for gemma-4-31b), so every budget here is a fraction of the resolved
  window rather than a constant. Nothing may assume a large window, and nothing
  should waste a large one.
- Preserve the existing safety invariant: a deterministic floor the model cannot
  lower, extended to cover tools that are not shell commands.
- Never hard-fail on third-party content. A broken plugin costs its own
  capability and a notice, nothing more.

## Non-goals

- **Marketplace management.** No clone, update, version-resolve, or install.
  Claude Code already populates `~/.claude/plugins/cache/`; dewey reads what is
  there. `/plugin` remains the way to install things.
- **Being Claude Code.** Tool names and spec formats are matched for
  compatibility. Session resumption, the full settings schema, output styles,
  background tasks, and the rest of the surface are out of scope.
- **Ecosystem neutrality.** Claude's formats are the native formats here. If
  Gemini extensions or Copilot skills ever matter, that is a later refactor
  against a second real implementation, not an abstraction built speculatively
  now.
- **Rendering images or embedded resources** from MCP results. A local text
  model cannot consume them; they become placeholders.

## Approach: read-through, not sync

Three options were considered.

**Read-through** reads `~/.claude` live at startup and assembles a session
inventory. **Import/sync** resolves everything into dewey-native YAML via a
`dewey sync` command. **Native-first** defines dewey's own spec formats and
treats Claude as one importer behind an interface.

Read-through wins on the thing that actually matters day to day: there is no
second copy of the truth, so there is no drift and no sync step to forget. Its
cost is coupling to an undocumented on-disk layout, which is real — Anthropic
can restructure the plugin cache without notice. That is mitigated by parsing
defensively (ignore unknown keys, never fail hard, report what was skipped) and
accepting the failure mode: if the layout changes, dewey loses imported
capabilities and says so. A stale snapshot that silently disagrees with Claude
Code is worse.

Sync's usual selling point is curation — importing ten of thirty-five skills to
save context. That argument does not apply here, because the context budget is
handled by deferred loading and index budgets (see below) rather than by
narrowing what exists.

Native-first would buy generality for a second ecosystem that does not exist,
and the frontmatter formats across ecosystems are near-identical anyway, so the
abstraction would earn very little.

### The one exception

MCP tool **discovery results** are cached at `~/.dewey/cache/mcp-index.json`,
keyed per server by a hash of that server's config block. This is a cache of
derived data, not of user config; config is still read live every launch, and a
changed server block invalidates its own entry. The reason is in "MCP
lifecycle" below: without it, launching dewey means spawning a process for
every configured server before the first prompt.

## Architecture

```
internal/claudecompat/      pure parsing — no exec, no network
  discover.go               settings.json, known_marketplaces.json, plugin cache
  spec/frontmatter.go       YAML frontmatter + body splitter
  spec/{skill,agent,command,plugin,mcp,hook}.go
  inventory.go              resolved Inventory + notices
internal/mcp/               client pool, transports, lifecycle, eino adapter
internal/files/             session read-tracking for Write/Edit invariants
internal/skills/            skill index + Skill tool
internal/subagent/          Task tool + nested runner
internal/commands/          slash-command registry + expansion
internal/hooks/             matcher, runner, JSON protocol
internal/tools/             grows: core.go, registry.go, search.go
```

The boundary that matters: `claudecompat` only reads and parses. It never
spawns a process or opens a socket. Everything with blast radius lives in `mcp`
and `hooks`, which consume an already-validated `Inventory`. This keeps the
layer that handles untrusted third-party files entirely testable from fixtures,
and keeps the dangerous code small enough to audit.

### Startup sequence

1. `config.Load(cwd)` — unchanged, plus a new `claude:` section.
2. `claudecompat.Discover(home, config.ProjectRoot(cwd))` → `Inventory` plus
   notices, which join the existing rule and memory notices already surfaced in
   `main.go`.
3. `mcp.NewPool(ctx, inv.MCPServers)` — builds the tool index from cache,
   spawning nothing.
4. `tools.NewRegistry(...)` — core tools bound; MCP tools indexed but unbound;
   `Skill`, `Task`, `ToolSearch` bound and pinned.
5. `hooks.NewRunner(inv.Hooks)` — fires `SessionStart`.
6. `agent.New(baseModel, registry, classifier, window, summarizer, hookRunner)`.

### Changes to existing packages

`agent.New` currently takes a pre-bound model and `[]tool.InvokableTool`. It
takes the **unbound** `model.ToolCallingChatModel` and a registry instead:

```go
type ToolRegistry interface {
    Bound(ctx context.Context) []*schema.ToolInfo
    Resolve(name string) (tool.InvokableTool, bool)
    Load(ctx context.Context, names []string) error
    Index() []ToolSummary
    BoundTokens() int
    Describe(name string) string
}
```

`eino`'s `WithTools` is immutable and documented for deriving per-request tool
variants from a shared base model (`components/model/interface.go:91-102`), so
per-call rebinding costs nothing structurally. `openaiAgent.messages()` already
rebuilds the system message per call for mid-turn freshness; the model
derivation slots in beside it. The `tools` and `descs` maps both collapse into
the registry.

Two smaller edits to `internal/agent/openai.go`:

- `estimateTokens` gains `registry.BoundTokens()`. Today it counts the system
  message and history but **not the bound tool schemas**, which are sent on
  every request. With two small tools that is a rounding error; with deferred
  MCP loading it is a hole that would let the window fill invisibly, so the 90%
  compaction threshold fires late and the request overflows instead of
  compacting.
- The `unknown tool` branch becomes: consult the index, `Load`, retry once, and
  only then error. See "Deferred loading" for why.

`internal/memory` and `internal/compaction` are untouched. `internal/safety`
changes only as described under "Safety".

## Core tools

`internal/tools/core.go` implements Claude's tool vocabulary using Claude's
exact tool and parameter names, so `tools:` allowlists in imported agent
frontmatter resolve verbatim: `Bash`, `Read`, `Write`, `Edit`, `Grep`, `Glob`.

`run_shell_command` is renamed to `Bash`. Same implementation, same
`ShellRules` classification path — only the name the ecosystem expects.
`remember` stays as dewey's own tool.

Two invariants are load-bearing for imported skills, and both need session
state dewey does not have:

- `Edit` fails when `old_string` matches more than once, rather than editing
  the first occurrence. This is the property TDD and refactoring skills rely on
  to be safe.
- `Write` refuses to overwrite an **existing** file that was not read this
  session. Creating a new file needs no prior read — that is the common case
  and blocking it would break most skills on their first step.

`internal/files` provides that: a session-scoped record of which paths were read
and at what mtime. Without it, `Edit` degrades to blind string replacement and
every skill built on it misbehaves in a way that looks like model failure
rather than harness failure.

`Read` returns `cat -n` line-numbered output, because skill and agent prose
assumes line numbers are available to cite. `Grep` uses ripgrep when it is on
`PATH` and falls back to a Go `regexp` walk otherwise — a hard `rg` dependency
would be a surprising install requirement for a single Go binary.

## Deferred loading

MCP tools register as `mcp__<server>__<tool>`. Plugin-provided servers use the
server key `plugin_<plugin>_<serverKey>`, which is the convention Claude Code
itself uses (claude-mem's search tool appears there as
`mcp__plugin_claude-mem_mcp-search__search`). Matching it means agent
frontmatter that allowlists MCP tools resolves correctly.

There are enough MCP tools available that listing them all with descriptions
costs roughly 3k tokens. Against a 262k window that is nothing; against the
8192-token fallback it is 37% of the window spent before anything useful
happens. Since the resolved window is not known until startup and may be
either, the index is rendered under a budget rather than sized for one regime:

- `claude.index_budget_tokens`, defaulting to 10% of the discovered window.
- Within budget: tool names grouped by server, no descriptions.
- Over budget: server names and tool counts only, reported as a notice.

`ToolSearch(query, max_results)` mirrors Claude's: `select:a,b,c` fetches exact
names, bare keywords rank over names and descriptions. It returns full schemas
and promotes them into the bound set. The full set is searchable regardless of
how the index degraded.

**Bound-set management.** Loaded schemas accumulate, so the bound set is capped
primarily by **tokens**, not by count: `claude.max_bound_tool_tokens`, default
`min(20% of window, 16k)`, with `claude.max_bound_tools` (default 24) as a
secondary guard against pathologically tiny schemas. A count-only cap would be
wrong on small windows, where two dozen verbose MCP schemas can exceed 8192
tokens by themselves.

Each percentage default carries an absolute ceiling for the same reason the
percentages exist: 20% of a 262k window is 52k of tool schemas, which is not a
sensible thing to send on every request even when it fits. The index budgets
are likewise `min(10%, 8k)`.

Eviction is least-recently-used among tools **not used in the current turn**.
Core tools, `Skill`, `Task`, and `ToolSearch` are pinned and never evicted. If
every bound tool has been used this turn and the cap is still exceeded, the cap
is allowed to overshoot for the remainder of the turn rather than evicting a
tool the model is actively using — which would otherwise produce a
load-evict-reload loop. `BoundTokens()` reports the real cost into
`estimateTokens`, so an overshooting turn triggers compaction rather than
overflowing.

**On how load-bearing this is.** Deferred loading was chosen over a static
allowlist when the working assumption was an 8k window, where it is close to
mandatory. At the 262k window dewey actually resolves today, binding every MCP
tool eagerly would fit, and the mechanism is a turn-cost optimization rather
than a necessity. It is still the right design: it is the only option that
works across the full 8k–262k range dewey has to tolerate, it keeps per-request
payloads small when hundreds of tools exist, and `claude.index_budget_tokens`
can be raised to approximate eager binding on a large window. But the
justification is adaptability, not survival.

**Auto-resolve.** When the model calls a tool that is indexed but unbound, the
registry loads it and executes — no error, no visible retry. This is
deliberate: `ToolSearch` is a discovery convenience, not a step a 30B model
must execute correctly for anything to work. Local models are measurably worse
than frontier models at search-then-call patterns, and this makes that
weakness cost latency instead of capability. A name absent from the index too
is still the existing unknown-tool error.

## MCP lifecycle

`github.com/modelcontextprotocol/go-sdk` v1.8.0 — official, past v1, actively
released. Stdio (`command`/`args`/`env`) and HTTP/SSE (`url`/`headers`)
transports both come from `.mcp.json`. Variable expansion covers
`${CLAUDE_PLUGIN_ROOT}` → the plugin's resolved version directory, plus
`${VAR}` and `${VAR:-default}` from the environment.

MCP `inputSchema` converts to eino via `schema.NewParamsOneOfByJSONSchema`
(`schema/tool.go:140`), so the adapter is close to passthrough rather than a
field-by-field translation.

**Lazy spawning.** Eagerly connecting every configured server would spawn a
process each for serena, grafana, notion, slack, claude-mem, and token-tracker
at launch — unacceptable latency for a TUI — but the deferred index needs tool
lists up front. The index cache resolves this: startup renders from cache and
spawns nothing, and a server boots only when one of its tools is loaded or
invoked. The first run per server pays discovery; later runs are instant.

**Failure isolation.** A server that fails to start, exceeds its 10s connect
timeout, or dies mid-session is marked unavailable: its tools leave the bound
set, a notice reaches the TUI, and in-flight calls return a tool error the
model can read and reason about. One automatic restart attempt per session,
then it stays down. One bad server never blocks startup or ends the session.

**Results.** Content blocks flatten to text; image and embedded-resource blocks
become a bracketed placeholder naming what was omitted. Results are capped at
`claude.max_tool_result_tokens` (default 25% of the window) with
middle-truncation and an explicit marker. This applies to every tool including
`Bash` — a single Grafana or Notion query can return more than an 8k window
holds.

## Safety

`safety.ShellRules` becomes `safety.Rules` with two sections:

```yaml
rules:
  shell:        # existing program/subcommand rules, unchanged semantics
  tools:        # name or glob → read | write | dangerous
    Read: read
    Grep: read
    Glob: read
    Write: write
    Edit: write
    "mcp__serena__find_*": read
    "mcp__slack__*": write
```

Floor semantics are preserved exactly: config sets a floor, the classifier may
raise it, nothing may lower it.

### Migrating `shell_rules`

Today's top-level `shell_rules:` key becomes `rules.shell:`. Because
`shell_rules` shipped recently and is in use, it stays accepted as a deprecated
alias that populates `rules.shell`, with a notice when it is seen. Setting both
is an error rather than a silent precedence rule — there is no defensible
guess about which the user meant.

### Claude `permissions` blocks

`permissions.allow` / `deny` / `ask` entries from `settings.json` are parsed and
mapped onto `rules.tools`: `deny` → `dangerous`, `ask` → `write`, `allow` →
`read`. Claude's `Tool(arg:*)` form maps to a glob over the tool name, and the
argument part is dropped for every tool except `Bash`, where it is handed to
the existing shell rules rather than reinterpreted.

Imported entries are a floor like any other, and rank below dewey's own
`rules.tools` on conflict, since those are written specifically for dewey. This
user has no `permissions` configured anywhere today, so the feature carries no
behavior at present — it exists so that adding one later does not require
re-deriving the mapping.

### Resolving an MCP tool's tier

Defaulting every unmatched MCP tool to `write` is safe but prompts on every
read-only call, which is unusable across hundreds of tools. MCP tools may carry
`readOnlyHint` and `destructiveHint` annotations, but those are server-authored
and therefore untrusted. Resolution is three-level:

1. A config `tools:` rule matches → that is the floor.
2. No rule, but the server advertises `readOnlyHint: true` **and** the
   classifier independently judges the call read-only → `read`, no prompt.
   Either one dissenting → `write`.
3. No hint → classifier decides, failing safe to `write` as it does today.

An untrusted hint can therefore only ever skip a prompt when dewey's own
classifier reaches the same conclusion on its own.

### Untrusted text in the classifier prompt

MCP tool descriptions are third-party text that flows into the classifier
prompt. They get the same flattening already applied to classifier output in
`plainLine` (`internal/agent/openai.go:333`), plus explicit untrusted-text
delimiters. A server's description field must not be able to restyle the
security prompt asking about it.

## Skills

Indexed from `skills/<name>/SKILL.md` across enabled plugins, `~/.claude/skills`,
and `<project>/.claude/skills`. Plugin skills are namespaced
(`superpowers:brainstorming`); user and project skills stay bare. This matches
Claude because skills reference each other by those names.

The system-prompt block carries name and description only. Installed
descriptions run long — the `devops-engineer` description alone is roughly 1,800
tokens — so each truncates to its first sentence under
`claude.skills_budget_tokens`, with the full text available on invocation.

`Skill(skill, args)` returns the `SKILL.md` body prefixed with:

```
Base directory for this skill: <absolute path>
```

That line is load-bearing. Skills instruct the reader to open
`references/<file>.md`, relative to the skill directory rather than cwd.
Without the base directory, every progressive-disclosure reference in the
ecosystem resolves to nothing.

## Subagents

Registry from `agents/*.md` across the same three locations, namespaced the same
way (`engineering-toolkit:devops-engineer`). `Task(description, prompt,
subagent_type)` spawns a fresh `openaiAgent` with its own history and
compaction, system prompt from the agent body, and tools filtered through the
registry by the frontmatter `tools:` list — `*` or absent means everything,
unknown names are dropped with a notice.

`model:` maps opus→`Hard`, sonnet→`Intermediate`, haiku→`Simple`,
`inherit`→parent tier. **The mapping must check that the target tier is
configured.** `applyModelDefaults(&hard, "")` in `internal/config/config.go`
leaves `hard.Model` empty when the tier is unset, so an unconfigured `Hard`
would produce a request with an empty model name that fails at call time rather
than at startup. Unconfigured tiers fall back to `Intermediate` with a notice.

Nested events forward to the parent's channel with a depth marker so the TUI
can indent them. Subagent approval prompts surface to the user rather than
auto-approving. Depth caps at 2, with `Task` withheld from agents already at
the limit.

Delegation is also dewey's structural answer to a small main window: a subagent
runs in a fresh one.

## Slash commands

`commands/*.md` → `/[<plugin>:]<name>`, with frontmatter `description`,
`argument-hint`, `allowed-tools`, and `model`. Expansion covers `$ARGUMENTS`,
`$1`–`$9`, `@file` inclusion, and `!`-prefixed bash substitution routed through
the normal `Bash` classification and approval gate.

`internal/app/app.go` intercepts a leading `/` on submit, resolves, expands, and
sends the result as the user message. A completion menu is out of scope;
`/help` lists what is available.

## Hooks

All events: `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`,
`Stop`, `SubagentStop`, `PreCompact`, `Notification`, `SessionEnd`. Sources are
`~/.claude/settings.json`, project settings, and plugin `hooks/hooks.json`, all
enabled by default.

Claude's contract: JSON on stdin, JSON or plain text on stdout, exit 0 normal,
exit 2 blocks with stderr as the reason, anything else a non-blocking error
reported as a notice. Per-hook timeout 60s, parallel within an event,
`CLAUDE_PROJECT_DIR` and `CLAUDE_PLUGIN_ROOT` exported. `PreToolUse` and
`PostToolUse` honor `matcher` as a regex over the tool name.

### Ordering and trust

`PreToolUse` runs **before** classification and may `deny` outright. On `allow`,
trust splits by source:

- Hooks from the user's own `settings.json` or project settings are the user's
  configuration and **may lower** the floor.
- Hooks from plugin `hooks/` directories are third-party code and **may raise
  but never lower** it.

Otherwise installing a plugin would silently grant it the ability to disable the
approval gate.

### Transcript fidelity

Hooks receive `session_id` and `transcript_path`. dewey writes
`~/.dewey/sessions/<id>.jsonl` in Claude's message shape, best-effort. Expect
partial success: claude-mem's `SessionStart` hook largely reads its own database
and should work, while hooks that parse transcripts to generate observations may
not, because dewey will not emit every field they look for. This is a known
limitation of the approach, not a defect to fix later.

## Package shape

| Package | Owns | Depends on |
| --- | --- | --- |
| `claudecompat` | Parsing `~/.claude` into an `Inventory` | nothing in dewey |
| `claudecompat/spec` | Frontmatter and manifest formats | nothing |
| `mcp` | Transports, lifecycle, index cache, eino adapter | `claudecompat` |
| `files` | Session read-tracking | nothing |
| `tools` | Core tools, registry, `ToolSearch` | `mcp`, `files` |
| `skills` | Skill index, `Skill` tool | `claudecompat` |
| `subagent` | `Task` tool, nested runner | `agent`, `tools`, `config` |
| `commands` | Slash-command registry and expansion | `claudecompat` |
| `hooks` | Matchers, runner, protocol | `claudecompat` |

`agent` depends on `tools` through the `ToolRegistry` interface only, and on
`hooks` through a narrow runner interface, keeping it testable without either.

## Config

```yaml
claude:
  enabled: true                  # master switch for all of this
  index_budget_tokens: 0         # 0 = min(10% of window, 8k)
  skills_budget_tokens: 0        # 0 = min(10% of window, 8k)
  max_bound_tool_tokens: 0       # 0 = min(20% of window, 16k)
  max_bound_tools: 24            # secondary guard on the above
  max_tool_result_tokens: 0      # 0 = min(25% of window, 32k)
  mcp:
    enabled: true
    connect_timeout: 10s
  hooks:
    enabled: true
  skills:
    enabled: true
  subagents:
    enabled: true
    max_depth: 2
  commands:
    enabled: true

rules:
  shell: {}                      # was top-level shell_rules, see Migrating
  tools: {}                      # new, see Safety
```

Every subsystem has an independent switch, so a misbehaving one can be turned
off without losing the others. `claude.enabled: false` restores today's
behavior exactly.

### Total static overhead

The budgets interact and have to be read together. At defaults: tool index 10%,
skills index 10%, bound tool schemas up to 20% — 40% of the window before the
system prompt, the memory block, or any conversation.

What that costs depends entirely on the resolved window, which varies by more
than an order of magnitude across realistic dewey setups:

| Resolved window | Static overhead | Verdict |
| --- | --- | --- |
| 8192 (the `defaultContextWindow` fallback, no server reachable) | ~3.3k | Tight. Disable skills, reduce `index_budget_tokens` |
| 16k–32k (a typical loaded local context) | 6k–13k | Comfortable |
| 262k (`max_context_length` for gemma-4-31b) | ~105k | Irrelevant |

This is why every budget is a configurable fraction rather than a constant: the
same defaults have to be survivable at 8k and non-wasteful at 262k. Startup
reports the resolved window and static overhead as a notice, so the operating
regime is visible rather than inferred from behavior.

### The window may be over-reported

`discoverLMStudioContextWindow` prefers `loaded_context_length` and falls
through to `max_context_length`. When a model is not resident,
`loaded_context_length` is null and the fall-through returns the
**architectural ceiling** — 262144 for gemma-4-31b, 1048576 for the nemotron
models — rather than what the server will actually serve once the model is
loaded with a practical context size.

Consequences for this design: `shouldCompact` never fires, requests fail at the
provider instead of compacting, and every percentage-based budget above
inflates against a window that does not exist.

The budgets are fractions, so they scale wrongly but harmlessly here. The
compaction failure is the real problem, and it predates this work. Two
mitigations belong in phase 0, where the window is first consumed as a planning
input:

- Treat a window derived from `max_context_length` on an unloaded model as a
  **hint, not a measurement**: clamp it to a configurable
  `models.<tier>.max_assumed_context_window` (default 32768) and report the
  clamp as a notice.
- Re-discover on first successful model call, when `loaded_context_length` is
  populated, and adopt the real value.

An explicit `context_window` in config overrides all of this and is the escape
hatch if the heuristics are wrong.

## Failure modes

| Failure | Behavior |
| --- | --- |
| `~/.claude` absent | No imported capabilities; core tools still work |
| Malformed plugin manifest, skill, or agent | That item skipped, notice names the file |
| Plugin cache layout unrecognized | Affected plugins skipped with a notice |
| MCP server fails to start or times out | Server unavailable, notice, one restart attempt |
| MCP server dies mid-session | Tools unbound, in-flight calls return a tool error |
| MCP result exceeds cap | Middle-truncated with an explicit marker |
| Tool index exceeds budget | Degrades to server names and counts, notice |
| Hook exits 2 | Tool call blocked, stderr shown as the reason |
| Hook exits nonzero and non-2 | Non-blocking, reported as a notice |
| Hook times out | Killed, treated as non-blocking error |
| Subagent model tier unconfigured | Falls back to `Intermediate` with a notice |
| `Task` called at max depth | Tool is not offered; unknown-tool error if forced |
| Both `shell_rules` and `rules.shell` set | Fatal — dewey's own config, no safe guess |

The single rule: third-party content never hard-fails startup. Only dewey's own
config being invalid is fatal.

## Testing

- `claudecompat`: a `testdata/` tree mirroring a real `~/.claude`, including
  deliberately malformed files, table-driven. No access to the real home
  directory.
- `mcp`: the SDK's in-process server for transport, adapter, and
  failure-isolation tests. No network. Cache invalidation tested by mutating a
  server config block.
- `tools`: registry budget rendering, eviction order, pinning, auto-resolve,
  and `BoundTokens` accounting. Core-tool invariants — `Edit` on a non-unique
  match, `Write` without a prior `Read`, `Read` line numbering.
- `safety`: the MCP tier matrix across every combination of config rule,
  `readOnlyHint`, and classifier verdict. Existing approval-gate tests stay
  green.
- `hooks`: temp scripts asserting each exit-code path, matcher regexes, timeout
  behavior, and the trust split between user and plugin hooks on `allow`.
- `subagent`: tool filtering, depth cap, model-tier fallback, event forwarding,
  using the existing fake model from the agent tests.
- `skills`: base-directory prefixing, namespacing, description truncation.

## Phases

| # | Phase | Notes |
| --- | --- | --- |
| 0 | `claudecompat` + inventory + notices | No behavior change; pure foundation |
| 1 | Core tools + `files` + registry | `run_shell_command` → `Bash` |
| 2 | `safety.Rules` generalization | Floor semantics preserved |
| 3 | MCP: transports, index cache, adapter, isolation, tiers | Largest phase |
| 4 | Deferred index + `ToolSearch` + bound-schema accounting | Closes the compaction hole |
| 5 | Skills + slash commands | Cheap given phase 1 |
| 6 | Subagents | Best relief for small windows |
| 7 | Hooks | Last; most security-sensitive |

Phase 4 depends on 3. Phase 6 depends on 1 and 2. Hooks are last despite being
enabled by default, because they are the only phase that executes arbitrary
third-party code.

**This document is a system design, not a single implementation plan.** It is
too large for one: expect roughly four plans — phases 0–2 as the foundation,
phase 3–4 as the MCP and deferred-tool work, phases 5–6 as the spec-consuming
features, and phase 7 as hooks. Each gets its own plan written against this
spec, and each ships independently useful behavior. Phases 0–2 alone give dewey
a real tool surface with no Claude integration visible yet.

## Risks

**Layout drift.** `~/.claude`'s structure is undocumented and may change. The
mitigation is defensive parsing and graceful degradation, not version pinning.
Accepted as a cost of the read-through approach.

**Local model capability.** Imported skills are written for frontier models.
Long rigid-process skills with multi-step checklists will likely underperform
on a 30B-class local model. Expect to curate what is enabled rather than
enabling everything, and treat skill quality on local models as an open
question this design does not answer.

**Transcript-parsing hooks.** Partial, as described under Hooks.

**Tool-surface lock-in.** Adopting Claude's tool vocabulary wholesale means
dewey's tool design is constrained to Claude's shapes going forward. This is a
deliberate trade for ecosystem compatibility, but it is a real constraint on
future tool work.
