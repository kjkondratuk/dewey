# Design: Long-term memory

Date: 2026-09-27

## Problem

dewey starts every session knowing nothing about the person using it. Anything
learned — that this machine needs `GOTOOLCHAIN=local`, that the user wants table
-driven tests, that one client's repos use a different commit convention — has to
be re-established by hand every time, in every project.

The gap this fills is specifically **user-shaped** context: preferences, working
style, and durable environment facts that customize how the agent behaves, and
that don't belong committed inside any one project.

## Goals

- Persist preferences and durable facts across projects and sessions.
- Condition them on context: stack, kind of work, and **organization**.
- Never let one organization's conventions influence work for another.
- Keep the corpus inspectable and hand-editable. It's the user's own profile.
- Nothing is written without the user's explicit approval.
- A saved memory takes effect immediately, not on next launch.

## Non-goals

- **Project knowledge** (architecture, where things live, why a decision was
  made). That belongs in the project, and is a different lifecycle.
- **Verbatim conversation recall.** Compaction already handles in-session
  history; searchable transcripts are a separate feature.
- **A `forget` tool.** Files are hand-editable and `rm` works. Revisit when the
  corpus is big enough for that to chafe.
- **Embedding-based retrieval.** See "Why not a vector DB" below. The seam is
  designed in; the implementation is not built.

## Why not a vector DB

Memories are gated behind user approval, so they accumulate slowly — a realistic
mature corpus is a few hundred entries of roughly a sentence each, on the order
of 6k–15k tokens against a 262k context window. The whole corpus fits in context
with room to spare.

Semantic retrieval earns its cost when the corpus can't fit and query phrasing is
unpredictable. Neither holds here. What it would cost: an embedding model
resident alongside a 31B chat model competing for the same VRAM, an embedding
round-trip per write and per query, re-indexing whenever the embedding model
changes (embeddings don't transfer between models), and opacity — diagnosing
"why did it do that?" becomes a similarity-score investigation instead of reading
a file.

Deterministic tag filtering is sufficient, debuggable, and free. The `Retriever`
interface (below) exists so this decision can be revisited against a real corpus
rather than a predicted one. `config.Embedding` already exists, unused, for that
day.

## Storage

One memory per file under `~/.dewey/memory/*.md`:

```markdown
---
id: prefer-table-tests
created: 2026-09-27
org: acme
tags: [go, testing]
---
Prefers table-driven tests with a `cases` slice over repeated near-identical
test functions.
```

| Field | Meaning |
|---|---|
| `id` | Stable slug, also the filename. Used for dedup and hand-reference. |
| `created` | Date, informational. |
| `org` | **Hard gate.** Absent = applies anywhere. Present = applies only when the detected org matches exactly. |
| `tags` | Topical. Absent = no topical restriction. Present = at least one must match the context. |

One file per memory rather than a single document: memories can be added,
edited, or deleted independently, diffs stay legible, and `git init` in the
directory yields history for free.

### Selection

A memory is selected when:

```
(org is absent OR org == detected org) AND (tags is empty OR tags ∩ context ≠ ∅)
```

The two fields deliberately have different semantics. Treating `org` as just
another OR-matched tag would mean a memory tagged `[go, acme]` fires on Go work
at a *different* organization — precisely the cross-contamination this design
exists to prevent. Org gates; tags filter.

A memory with `org:` set never matches when the detected org is unknown. This
fails closed: an unrecognized repo gets only universal memories, rather than
inheriting some client's conventions by accident.

### Context detection

The context tag set is derived from the working directory:

- **Stack** — `go.mod` → `go`, `package.json` → `node`, `Cargo.toml` → `rust`,
  `pyproject.toml`/`requirements.txt` → `python`. Multiple may apply.
- **Org**, in precedence order:
  1. `org:` in the project's `.dewey/config.yaml` — explicit and authoritative.
  2. Parsed from `git remote get-url origin`: the owner segment of
     `github.com/<owner>/<repo>` and equivalents for other hosts.
  3. Unknown (empty).

Project config comes first because git-remote parsing is too fragile to be the
sole basis for an isolation guarantee, and because repos without remotes need a
way to declare their org at all.

## Immediate effect

The selected set is assembled into the system prompt before **each** `Stream`
call, not once at startup, so a memory saved mid-turn applies to the very next
model call.

This is backed by a cache rather than re-reading the directory every time. The
store reports a revision; the provider reloads only when it changes. The file
store's revision advances when it writes a memory itself, and when the memory
directory's modification time advances — so hand-editing a file mid-session is
also picked up.

## System prompt

Memory needs somewhere to live, and dewey currently sends no system prompt at
all. This feature adds one: base agent instructions plus a "what I know about
you" block listing the selected memories. It does not adopt `prompts/system.md`'s
template shape (`.Memory`/`.Conversation`/`.CurrentInstruction`), which assumes a
single-shot prompt architecture rather than the message history dewey uses.

**The system message is a field on the agent, not `history[0]`.** `compact()`
replaces `history[:truncateLen]` with a summary; a system message living at index
0 would be summarized away the first time compaction fired. Holding it separately
and prepending it when calling `Stream` means compaction structurally cannot
consume it, rather than depending on an offset staying correct as the code
changes. `estimateTokens` must count it, since it is part of every request.

## Write path

A `remember` tool taking `{text, tags}`, registered in the tool factory.

It needs no new confirmation UI: the classifier will tag it a write, so the
existing risk-gating path shows the proposed memory with its plain-English
summary and waits for y/n. A denial feeds back through the existing denial
result, which already tells the model not to retry.

The system prompt instructs the agent to propose a memory when the user states a
durable preference, corrects the same behavior more than once, or establishes an
environment fact — and explicitly *not* to propose project-specific facts, which
belong in the project.

The tool infers `org` from the current context rather than accepting it as a
parameter. A model that could set `org` freely could also set it wrongly, and the
failure mode is silent cross-org leakage.

## Package shape

`internal/memory`:

| Unit | Responsibility |
|---|---|
| `Memory` | The struct: ID, Text, Org, Tags, Created. |
| `Store` | Load/Save/Revision. File-backed; skips and reports malformed files rather than failing the session. |
| `Retriever` | **The seam.** `Select(memories, context) []Memory`. |
| `TagRetriever` | The only implementation for v1. |
| `DetectContext(cwd, projectOrg)` | Builds the context tag set and resolves the org. |
| `Provider` | Caches, watches revision, assembles the memory block for the prompt. |

Prompt assembly extends the existing `prompts` package with an embedded base
system prompt.

## Config

```yaml
memory:
  enabled: true              # default true
  dir: ~/.dewey/memory       # default
  max_tokens: 4000           # budget for the injected block
org: acme                    # project-level, in the project's .dewey/config.yaml
```

Exceeding `max_tokens` drops the oldest memories first and surfaces a notice —
silent truncation of a profile the user curated would be worse than the overflow.

## Failure modes

| Condition | Behavior |
|---|---|
| Memory dir missing | Treated as empty. Created lazily on first save. |
| Malformed frontmatter | That file is skipped and reported; the rest load. |
| Duplicate `id` | Last one wins, reported. |
| Budget exceeded | Oldest dropped, notice surfaced. |
| `enabled: false` | No injection, no `remember` tool registered. |

Memory being unavailable must never prevent dewey from running — it degrades to
today's behavior.

## Testing

- Store: round-trip, frontmatter parsing, malformed-file tolerance, duplicate ids,
  missing directory, revision advancing on save.
- Selection: org gate (matching, mismatched, unknown, absent), tag intersection,
  and specifically that `[go, acme]` does **not** select under a different org.
- Detection: each stack marker, project-config precedence over git remote, remote
  URL parsing across formats, no-remote fallback.
- Provider: reload on revision change, no reload without one, budget enforcement.
- Agent: system message survives compaction; `estimateTokens` includes it; a
  memory saved mid-turn is present in the next `Stream` call.
- Tool: writes only after approval; denial writes nothing; org is taken from
  context and not from model input.
