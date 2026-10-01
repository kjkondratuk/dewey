# Design: Long-term memory

Date: 2026-09-27

## Problem

dewey has no memory today, so anything learned — that this machine needs
`GOTOOLCHAIN=local`, that the user wants table-driven tests, that one client's
repos use a different commit convention — has to be re-established by hand every
session, in every project.

The gap this fills is specifically **user-shaped** context: preferences, working
style, and durable environment facts that customize how the agent behaves.

Once this exists, dewey starts a session already knowing these things: memories
are read from disk at launch, before the first prompt, not discovered during the
conversation.

## Goals

- Persist preferences and durable facts across projects and sessions.
- Condition them on context: stack, kind of work, and **organization**.
- Never let one organization's conventions influence work for another.
- Keep the corpus inspectable and hand-editable. It's the user's own profile.
- Nothing is written without the user's explicit approval.
- A saved memory takes effect immediately, not on next launch.

## Non-goals

- **Project documentation** — architecture notes, where things live, why a
  decision was made. A project-local memory directory exists (see Storage), but
  it is for behavioral conventions that should apply while working in that repo,
  not a replacement for the project's own docs. The content guidance stays the
  same in both locations: how to work, not what the code is.
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

Memories are read from **two** locations at launch, mirroring how `config.Load`
already merges global and project configuration:

| Location | Purpose |
|---|---|
| `~/.dewey/memory/*.md` | Global. Travels with the user across every project. |
| `<project>/.dewey/memory/*.md` | Project-local. Can be committed, so conventions travel with the repo and apply to anyone working in it. |

Both are loaded and merged. On an `id` collision the project copy wins, matching
the precedence config already uses — the more specific location overrides the
more general one. Either directory may be absent.

New memories written by the `remember` tool go to the **global** directory by
default. Project-local memories are expected to be authored and committed
deliberately rather than accumulated by the agent, so promoting one is a
deliberate `git mv`.

One memory per file, in either location:

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
- **Org** comes from `git remote get-url origin` — the owner segment of
  `github.com/<owner>/<repo>` and the equivalents for other hosts — and from
  nothing else. Unknown (empty) when there is no remote.

An earlier version of this design let the project's own `.dewey/config.yaml`
declare the org, treating it as explicit and authoritative. That was wrong in
two ways, both found in review:

- A checkout's contents come from whoever authored the repo. Cloning a repo
  that ships `org: acme` made dewey load the user's Acme-scoped memories while
  working in a stranger's code — the repo, not the user, decided which private
  memories entered the system prompt.
- Config lookup did not search upward while `git remote` does, so running from
  a subdirectory silently swapped one source for the other and the same
  checkout reported two different orgs at two different depths.

A single source removes both. The cost is that a repo with no remote cannot be
org-scoped at all, so only unscoped memories apply there — which fails closed.
Because the org is now derived rather than configured, the resolved scope is
reported as a startup notice; org-scoped memories must name it exactly, and
there would otherwise be nothing to match against.

Project-level `.dewey/` is still located by searching upward from the working
directory, stopping at a `.dewey` directory or a repository boundary, so
launching from a subdirectory keeps the project's config and memories.

## Loading and refresh

Memories are loaded from disk at launch, so the first prompt of a session
already carries them.

They are then re-assembled into the system prompt before **each** `Stream` call
rather than only at startup, so a memory saved mid-session takes effect on the
very next model call instead of after a restart.

This is backed by a cache rather than re-reading both directories every time.
Each store reports a revision and the provider reloads only when it changes.

A file store's revision is a digest over its `.md` entries — name, size, and
modification time — combined with an in-process write counter. The digest must
cover the files rather than the directory: a directory's own mtime advances
only when an entry is added or removed, not when an existing file is
overwritten, so a directory-level check would miss the most likely curation
action of all, which is opening a memory in an editor and rewriting it. The
write counter is folded in alongside so this process's own saves register
regardless of filesystem timestamp granularity.

`MergedStore`'s revision advances when either underlying store's does, so an
edit in the project directory is noticed just like one in the global directory.

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
| `Store` | Load/Save/Revision over a single directory. File-backed; skips and reports malformed files rather than failing the session. |
| `MergedStore` | Composes the global and project stores: loads both, applies project-over-global precedence, writes to global, and reports a revision that advances when either changes. |
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
  dir: ~/.dewey/memory       # global store; default shown, leading ~ expanded
  max_tokens: 4000           # budget for the injected block
```

There is no `org` setting: see "Context detection" for why it is derived from
the git remote instead.

The project-local store is always `<projectDir>/.dewey/memory` and is not
configurable — it sits beside the project config file that dewey already looks
for, so there is nothing to point at.

Exceeding `max_tokens` drops the oldest memories first and surfaces a notice —
silent truncation of a profile the user curated would be worse than the overflow.

## Failure modes

| Condition | Behavior |
|---|---|
| Either memory dir missing | Treated as empty. The global one is created lazily on first save. |
| A memory file that is not a regular file | Refused and reported. Project memory directories are committed, so a clone can ship a symlink, and these files are read into the system message — a link to a private key would put key material in the model's context. |
| A new memory whose slug collides | Disambiguated with a numeric suffix. Re-saving identical text keeps its id. Preference statements share opening words constantly, and saves land via rename, so a collision previously destroyed a memory the user had already approved. |
| Malformed frontmatter | That file is skipped and reported; the rest load. |
| Duplicate `id` across the two stores | Project wins, silently — that is the documented override mechanism, not an error. |
| Duplicate `id` within one store | Both load and both apply. Nothing downstream requires ids to be unique within a store, so this needs no special handling. |
| Budget exceeded | Oldest dropped, notice surfaced. |
| `enabled: false` | No injection, no `remember` tool registered. |

Memory being unavailable must never prevent dewey from running — it degrades to
today's behavior.

## Testing

- Store: round-trip, frontmatter parsing, malformed-file tolerance, duplicate ids,
  missing directory, revision advancing on save.
- MergedStore: both directories load, project overrides global on matching id,
  either directory absent, saves land in the global store, and the revision
  advances when either underlying store changes.
- Selection: org gate (matching, mismatched, unknown, absent), tag intersection,
  and specifically that `[go, acme]` does **not** select under a different org.
- Detection: each stack marker, project-config precedence over git remote, remote
  URL parsing across formats, no-remote fallback.
- Provider: reload on revision change, no reload without one, budget enforcement.
- Agent: system message survives compaction; `estimateTokens` includes it; a
  memory saved mid-turn is present in the next `Stream` call.
- Tool: writes only after approval; denial writes nothing; org is taken from
  context and not from model input.
