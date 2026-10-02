# dewey

A personal terminal agent harness. dewey talks to any OpenAI-compatible model
server — Bionic or LM Studio running locally, or a hosted API — runs shell
commands on your behalf, asks before anything consequential, compacts the
conversation when the context window fills, and remembers your preferences
across projects.

It is a single Go binary with a TUI. No daemon, no database.

## Contents

- [Quick start](#quick-start)
- [How the model is chosen](#how-the-model-is-chosen)
- [Bionic / LM Studio](#bionic--lm-studio)
- [Other providers](#other-providers)
- [Configuration reference](#configuration-reference)
- [Tool safety and approval](#tool-safety-and-approval)
- [Context compaction](#context-compaction)
- [Long-term memory](#long-term-memory)
- [Keybindings](#keybindings)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [Current limitations](#current-limitations)

## Quick start

Requires **Go 1.26.2** and a running model server.

```bash
git clone git@github.com:kjkondratuk/dewey.git
cd dewey
go run .
```

With no config file at all, dewey assumes Bionic (or LM Studio) on its default
port:

| Setting | Default |
|---|---|
| Base URL | `http://localhost:1234/v1` |
| API key | `lm-studio` (the server ignores the value) |
| Model | `gemma-4-31b-it-turboquant-mlx` |

If your loaded model has a different id — it almost certainly does — set it in
a config file as described below. dewey does not auto-detect which model to
use; it only auto-detects that model's context window size.

**The model id is not validated at startup.** dewey builds its client without
contacting the server, so a wrong id or an unreachable server surfaces as a red
error block on your first message rather than as a startup failure. If dewey
launches cleanly and then errors the moment you say anything, check the model id
first.

## How the model is chosen

dewey resolves models through **tiers**, so cheap work can go to a small fast
model while the main conversation uses a larger one. Each tier is a
`base_url` + `api_key` + `model` triple.

| Tier | What uses it | Required |
|---|---|---|
| `intermediate` | The main conversation — your prompts, tool calling, streamed replies. | Yes (defaulted) |
| `simple` | The safety classifier (one call per tool call) and the compaction summarizer. | No — falls back to `intermediate` |
| `hard` | Nothing yet. Parsed but unused. | No |
| `embedding` | Nothing yet. Parsed but unused. | No |

Configuration is read from two files and merged:

1. `~/.dewey/config.yaml` — global
2. `<project-root>/.dewey/config.yaml` — project-specific, wins where it overlaps

The project file is located by searching **upward** from your working
directory, stopping at a `.dewey` directory or a repository boundary, so
running dewey from a subdirectory still finds it. A missing file is not an
error; malformed YAML is.

**Resolution order per tier**, highest priority first:

1. The project config's entry for that tier
2. The global config's entry for that tier
3. A built-in default. The default base URL and key apply to every tier; only
   the default *model* is specific to `intermediate`.

Two wrinkles worth knowing.

**A tier is replaced wholesale across files, not merged field-by-field.** If the
project file sets only `models.simple.model`, the whole `simple` tier comes from
the project file and the global file's `base_url` for that tier is ignored. This
is deliberate, so a project can point a tier at an entirely different server
without inheriting half of another config.

**`simple` then inherits anything it left unset from the resolved
`intermediate`** — base URL, key, model, and context window. Setting just
`models.simple.model` is the common case (one server, two models), and pulling
the base URL from the built-in default instead would silently aim the classifier
at a server that isn't running whenever `intermediate` is elsewhere. Note this
is inheritance from `intermediate`, not from the built-in defaults; `hard` and
`embedding` get the built-ins.

Minimal config naming your actual model:

```yaml
# ~/.dewey/config.yaml
models:
  intermediate:
    model: your-model-id-here
```

Using a small model for classification and summarization:

```yaml
models:
  intermediate:
    model: gemma-4-31b-it-turboquant-mlx
  simple:
    model: qwen3-4b-instruct
```

`intermediate` takes the default base URL and key, and `simple` inherits both
from it, so one local server hosting both models is all you need. Pointing
`intermediate` at a different server needs no change here either — `simple`
follows it.

## Bionic / LM Studio

This is the setup dewey defaults to, and the one it has actually been run
against. Bionic is LM Studio's desktop app; the local server and its API are
the same either way, so the steps below apply to both.

1. Install [Bionic](https://lmstudio.ai) and download a model. **It must
   support tool calling** — dewey is useless without it. The model list marks
   these, and the API reports `"capabilities": ["tool_use"]`.
2. Start the local server. Default port is `1234`.
3. Load your model, and set its context length while you're there — dewey reads
   the loaded value to decide when to compact.
4. Find the model id. It is the `id` field from:

   ```bash
   curl -s http://localhost:1234/v1/models
   ```

   Ids look like `gemma-4-31b-it-turboquant-mlx` — the quantized filename, not
   the friendly display name.
5. Put that id in your config:

   ```yaml
   models:
     intermediate:
       model: gemma-4-31b-it-turboquant-mlx
   ```

6. `go run .`

The default `api_key` of `lm-studio` is a placeholder. The local server does not
check it, but the OpenAI client library requires something non-empty.

### Context window auto-detection

Alongside the OpenAI-compatible API, the server exposes a **native** one at
`/api/v0/models` reporting context sizes the OpenAI spec has no field for:

```bash
curl -s http://localhost:1234/api/v0/models
```

```json
{"data": [{
  "id": "gemma-4-31b-it-turboquant-mlx",
  "state": "loaded",
  "max_context_length": 262144,
  "loaded_context_length": 262144,
  "capabilities": ["tool_use"]
}]}
```

dewey queries this at startup (2-second timeout) and prefers
`loaded_context_length`, since loading a model with a smaller window than it
supports is common. Failure is silent and expected on any other provider — it
is a convenience, not a requirement.

Every other provider needs `context_window` set by hand; see below.

## Other providers

Anything speaking the OpenAI chat-completions API should work — Ollama, vLLM,
llama.cpp's server, LiteLLM as a gateway, or the hosted OpenAI API itself.
Point `base_url` at it and supply a key.

```yaml
models:
  intermediate:
    base_url: "http://localhost:11434/v1"   # Ollama
    api_key: "ollama"
    model: "qwen3:32b"
    context_window: 32768
```

Two requirements, both non-negotiable:

- **Tool calling.** dewey's only tool for doing work is `run_shell_command`. A
  model that cannot emit tool calls will talk about running commands without
  ever running one.
- **Streaming.** Replies are streamed; a server that only supports
  non-streaming completions will not work.

Set `context_window` for anything other than Bionic / LM Studio.

## Configuration reference

Every field, with its default. All are optional.

```yaml
# Model tiers. Each may set any subset; gaps are defaulted.
models:
  intermediate:                                  # the main conversation
    base_url: "http://localhost:1234/v1"
    api_key: "lm-studio"
    model: "gemma-4-31b-it-turboquant-mlx"
    context_window: 32768                        # fallback; see note below
  simple:                                        # classifier + summarizer
    base_url: "http://localhost:1234/v1"         # defaults to intermediate's
    api_key: "lm-studio"
    model: ""                                    # defaults to intermediate's
  hard: {}                                       # parsed, unused
  embedding: {}                                  # parsed, unused

# Extend the deterministic floor applied to shell commands. These merge over
# the built-in defaults rather than replacing them.
shell_rules:
  # Programs that always require approval, whatever the arguments.
  always_write: [terraform-destroy-wrapper]

  # Programs that read or write depending on the subcommand. Listing a program
  # here means unknown invocations of it count as writes.
  reads_when:
    bazel:
      subcommands: [query, info, version]
    ansible:
      subcommands: ["--version", "--list-hosts"]
      # Patterns match the arguments after the program name, and describe
      # READS only. They never describe writes: anything unmatched has to fall
      # through as a write, or an unanticipated command runs silently.
      patterns: ['^--check\b']

  # Programs that read by default and write only with one of these flags.
  # Replaces the built-in list for that program rather than extending it.
  writes_with_flags:
    rsync: ['--delete']

  # Remove a program from always_write. The one knob that weakens the floor,
  # so dewey reports it at startup.
  never_write: [mktemp]

# Long-term memory.
memory:
  enabled: true
  dir: ~/.dewey/memory       # leading ~ is expanded
  max_tokens: 4000           # budget for the injected memory block
```

`context_window` is a **fallback, not an override**: native discovery is always
attempted first and wins when it succeeds, so against Bionic / LM Studio this
value is ignored. It is what gets used on every other provider, and omitting it there
means a conservative 8192. It is only ever read for the `intermediate` tier —
setting it on another tier parses and is silently ignored.

`shell_rules` and `memory` merge **per-key** between the global and project
files, unlike model tiers — adding one subcommand for `git` keeps the built-in
list. An invalid regex in `patterns` is a fatal startup error rather than a
silently-skipped rule, since a rule you believe is in effect is the failure
this is guarding against.

The `remember` tool declares itself a write in the built-in rules
(`always_write_tools`), so its approval prompt always fires regardless of what
the classifier decides. You can still override it explicitly if you want
it ungated.

## Tool safety and approval

dewey has one tool for doing work — `run_shell_command`, which runs a command
via `sh -c` and returns combined stdout/stderr plus the exit code — and one for
remembering things. There is no sandbox. The protection is that you approve
anything risky.

Before each tool call runs, the `simple` model classifies it along two axes —
whether it **writes**, and how risky it is:

| Classification | Behavior |
|---|---|
| `read` + `safe` | Runs immediately |
| anything `write` | **Asks first, at any risk tier** |
| `moderate` or `dangerous`, even if `read` | Asks first |

Both signals gate independently. A write rated `safe` still prompts — local
models routinely rate `echo hello > f.txt` as safe, and "writes get confirmed"
is the actual rule rather than a tier threshold.

Shell commands additionally get a **deterministic floor** before the model's
answer is used. Output redirection and a list of known-mutating programs (`rm`,
`mv`, `git`, `npm`, `chmod`, `sudo`, …) are recognized structurally and force a
`write`/`moderate` minimum. The classifier can raise this but never lower it,
so the only gate in front of an arbitrary-command tool does not rest solely on
a language model's judgement.

The floor is a floor, not a parser: it doesn't expand variables, follow
aliases, or read invoked scripts, and anything it misses still goes to the
classifier. It over-prompts in places — `git status` is treated as a write
because telling it apart from `git reset --hard` needs real subcommand parsing
— which is the right direction to err.

A prompt looks like this:

```
run_shell_command wants to run (WRITE) [risk: dangerous]

Permanently deletes the project's build output folder and everything inside it.

    rm -rf ./dist

Allow? [y/n]
```

The plain-English line comes from the classifier, so you can judge a command
without reading its flags. Answer `y` to run, `n` to decline, `esc` to cancel
the whole turn.

The summary comes from the classifier, so a prompt that fired only because the
deterministic floor raised it may show a summary that undersells what the
command does. The command text above it is authoritative.

Declining is not an error. The model is told you had the capability and chose
not to use it, and asked to propose something else rather than retry.

Classification **fails safe**: any error, malformed response, or unrecognized
value is treated as `dangerous`/write, so a classifier outage means more
prompts, never fewer. A config `override` skips the classifier entirely for
that tool.

## Context compaction

When estimated usage crosses **90%** of the context window, dewey summarizes
the conversation so far into a single system message and continues. It happens
automatically at the start of a turn, before anything is streamed, and prints a
note:

```
Compacted conversation history (was ~235929/262144 tokens, 90%).
```

Token counting trusts the provider's reported usage when it supplies any, and
otherwise estimates with a tokenizer (`cl100k_base`), measuring only what was
added since the last trustworthy number.

**One caveat worth knowing:** that tokenizer downloads its vocabulary on first
use and caches it under `$TMPDIR`. If the download can't happen — offline first
run, or a cleared temp directory with no network — estimation returns zero and
compaction simply never fires, silently. Set `TIKTOKEN_CACHE_DIR` to somewhere
durable if you work offline. Providers that report usage are unaffected.

The summarizer is instructed to preserve goals, decisions, discovered facts
(file paths, commands, error messages), in-progress work, and open questions,
and to drop conversational filler. Your current message is never summarized —
only history before it.

## Long-term memory

dewey keeps a profile of **how you work** that persists across projects and
sessions: preferences, working style, durable environment facts. Not project
documentation — that belongs in the project.

### Where memories live

| Location | Purpose |
|---|---|
| `~/.dewey/memory/*.md` | Global. Travels with you everywhere. |
| `<project-root>/.dewey/memory/*.md` | Project-local. Committable, so conventions travel with the repo. |

Both load at launch and merge, with project memories overriding global ones
that share an `id`. New memories the agent proposes go to the **global**
directory; project memories are meant to be authored and committed
deliberately.

### Format

One memory per file:

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
| `id` | Stable slug, also the filename stem. Optional — defaults to the filename. |
| `created` | Date. Informational, and decides eviction order when the budget is tight. |
| `org` | **Hard gate.** Absent means "applies anywhere". Present means "only when the detected organization matches exactly". |
| `tags` | Stack filter. Absent means no restriction; present means at least one must match the detected stack. Only `go`, `node`, `rust`, and `python` are meaningful — tags match by intersection with what detection produces, so anything else can never match and is dropped on save. |

`org` and `tags` behave differently on purpose. A memory tagged `[go]` with
`org: acme` does **not** apply to Go work for a different organization — org
gates, tags only narrow. A memory with `org:` set never applies when the
organization can't be determined, so an unrecognized repo gets only your
unscoped memories.

### How scope is detected

At startup dewey works out what the current work is and tells you:

```
Memory scope: org "acme", tags [go].
```

Or, in a repo with no remote:

```
Memory scope: no organization detected (no git remote), tags [go]. Only unscoped memories apply here.
```

- **Tags** come from marker files: `go.mod` → `go`, `package.json` → `node`,
  `Cargo.toml` → `rust`, `pyproject.toml`/`requirements.txt` → `python`. The
  search walks upward to the repository root, so a subdirectory gets the same
  tags as the root.
- **Org** comes from `git remote get-url origin` — the owner segment, so
  `github.com/acme/widget` yields `acme`. Nothing else sets it.

Org is deliberately **not** configurable. An earlier version let a project's
`.dewey/config.yaml` declare it, but a checkout's contents come from whoever
authored the repo — so cloning a repo that shipped `org: acme` would load your
Acme-scoped memories while you worked in a stranger's code. Deriving it from
the remote means there is no repo-supplied claim to abuse.

The consequence: **a repo with no remote cannot be org-scoped.** Only unscoped
memories apply there. Comparison is case-insensitive and whitespace-trimmed, so
a remote owner of `Acme` matches `org: acme`.

### Saving memories

The agent proposes a memory when you state a preference, correct the same
behavior twice, or establish a durable fact about your environment. It arrives
as a normal approval prompt, and nothing is written unless you say yes.

**Saved memories apply everywhere by default.** Most preferences follow you
between projects, so the agent only narrows one when it has reason to: it can
ask for the memory to be limited to the current organization, and it can tag a
memory with a stack. It can never name a *different* organization — the choice
is "here" or "everywhere", which is what keeps one client's conventions out of
another's work while still allowing universal preferences.

You can also just write the files by hand — that is the whole storage format,
and edits are picked up on the next model call without restarting.

A few deliberate behaviors:

- Memories are injected into the system prompt, rebuilt before every model
  call, so one saved mid-session applies immediately.
- The injected block is capped at `max_tokens`, newest kept first. Anything
  dropped is reported rather than silently omitted.
- Files that can't be parsed are skipped and reported, not fatal.
- Non-regular files (symlinks) are refused. These files are read into the
  system prompt, and a committed symlink pointing at something private would
  put its contents in the model's context.
- Colliding slugs are disambiguated rather than overwriting. Re-saving
  identical text reuses its id.

Memory can be turned off entirely with `memory: {enabled: false}`, which also
unregisters the `remember` tool and stops telling the model it exists.

## Keybindings

| Key | Idle | Agent working | Approval prompt |
|---|---|---|---|
| `enter` | Submit | — (input disabled) | — |
| `y` / `n` | — | — | Allow / deny |
| `esc` | — (nothing in flight) | Press twice within 1.5s to abort | Cancel turn |
| `ctrl+c` | Quit | Quit | Quit |
| `↑` / `↓` | Scroll | Scroll | Scroll |
| `pgup` / `pgdn` | Page | Page | Page |
| `←` / `→` | Scroll sideways | — | — |

Input is disabled while a turn is in flight, so aborting takes a deliberate
double-escape. At an approval prompt a single `esc` cancels, since the question
is already waiting on you.

The live keys are also shown on the status line while a turn is in flight
(`Thinking… · esc esc to abort`) and at an approval prompt
(`y allow · n deny · esc cancel`).

The conversation keeps your scroll position while output streams; submitting a
new prompt returns you to the bottom.

Block borders carry meaning: blue for your messages, gray for replies and
status, **yellow** when something is waiting on you, **red** for errors and
denials.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Launches fine, red error on the first message | Wrong `model` id, or the server isn't running. The id is never validated at startup. Check `curl -s <base_url>/models`. |
| Talks about running a command but never runs one | The model doesn't support tool calling. Nothing else in dewey works without it. |
| An approval prompt on *every* tool call, even obviously safe ones | The `simple` tier can't be reached, so classification fails and fails safe to `dangerous`. If you set `simple.base_url` explicitly, confirm something is listening there. |
| Compaction never happens on a long conversation | Either the provider reports usage and the window is genuinely large enough, or the tokenizer vocabulary couldn't be fetched. See the note under [Context compaction](#context-compaction). |
| `remember` isn't offered | `memory.enabled` is false, in which case the tool is unregistered and the model is not told it exists. |
| A memory isn't being applied | Compare its `org`/`tags` against the scope notice printed at startup. An `org:` memory never applies when no organization is detected, and a repo with no git remote has none. |
| A memory file seems ignored | Look for a startup notice naming it. Malformed frontmatter, an empty body, and symlinks are each reported and skipped. |
| Startup errors about a shell rule | An invalid regex under `shell_rules.<program>.patterns`. The message names the program and the pattern. |
| A command you run constantly keeps prompting | Add its read-only form under `shell_rules.reads_when`, or the program to `never_write` if it should never prompt. Both are reported at startup so a weakened rule stays visible. |

## Development

The Go toolchain is pinned to 1.26.2.

```bash
go build ./...
go vet ./...
go test ./... -race
gofmt -l .
```

Layout:

| Package | Responsibility |
|---|---|
| `internal/agent` | The conversation loop: streaming, tool calls, approval gating, compaction. |
| `internal/app` | Bubble Tea TUI. |
| `internal/config` | Config loading, merging, defaults, context-window discovery. |
| `internal/safety` | Risk tiers and the LLM-backed classifier. |
| `internal/compaction` | Token estimation and the summarizer. |
| `internal/memory` | Memory store, scope detection, retrieval, prompt assembly. |
| `internal/tools` | Tool definitions. |
| `prompts` | Model-facing text as embedded markdown. |

Prompts live in `prompts/*.md` and are compiled in with `go:embed`, so they can
be edited as prose. The built binary has no runtime dependency on the
directory.

Design documents and implementation plans are under `docs/superpowers/`.

## Current limitations

- **No sandbox.** `run_shell_command` runs with your full user privileges.
  Approval prompts are the only guard, and the risk classification comes from a
  local model's judgement.
- **`hard` and `embedding` tiers do nothing.** They are parsed and resolved but
  never used.
- **Context-window auto-detection only works against Bionic / LM Studio**,
  which is the one server exposing the native endpoint that reports it.
  Everything else needs `context_window` set, or silently gets 8192.
- **One tool.** Everything goes through the shell.
- **Tool output is not truncated.** A command's entire combined output goes
  into the conversation, so one `cat` of a large file can consume the context
  window in a single turn.
- **No mouse support.** Wheel scrolling does nothing; use the arrow and page
  keys.
- **No conversation persistence.** Quitting discards the conversation; only
  memories survive.
- **Memory retrieval is tag-based, not semantic.** Deliberate at this corpus
  size — the whole profile fits in context, so exact filtering is cheaper and
  more debuggable than embeddings. The `Retriever` interface exists so this can
  change.
