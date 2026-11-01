# Instructions, MCP registry and flight rules: behaviour spec

Status: draft for review. Part 8, study stage.

This spec says how each supported harness loads instruction files, MCP servers and memory. It
says what each harness records about the context it loaded, and what stays runtime only. From
that it sets the behaviour of three Part 8 features:

- the **instruction renderer**, which turns a user's own instruction files into each harness's
  native file;
- the **MCP registry**, one neutral server list rendered into each harness's config;
- **flight rules**, the per-session ledger of everything that reached the agent, and memory
  writes as proposals.

Harnesses covered: Claude Code, Codex, opencode and pi. Versions studied: Claude Code 2.1.316,
Codex 0.166.0, pi 1.4.0, opencode V2 as documented on 2026-11-01. Every claim names its source in
[Sources](#sources). It describes behaviour in our own words and copies no code.

## Contents

1. [The line we hold](#1-the-line-we-hold)
2. [The bar](#2-the-bar)
3. [Terms](#3-terms)
4. [Instructions per harness](#4-instructions-per-harness)
5. [Import semantics](#5-import-semantics)
6. [The instruction renderer](#6-the-instruction-renderer)
7. [MCP per harness](#7-mcp-per-harness)
8. [The MCP registry](#8-the-mcp-registry)
9. [Memory per harness](#9-memory-per-harness)
10. [What each harness records](#10-what-each-harness-records)
11. [Flight rules: the ledger](#11-flight-rules-the-ledger)
12. [Memory writes as proposals](#12-memory-writes-as-proposals)
13. [Stale sessions and pick-up](#13-stale-sessions-and-pick-up)
14. [Decisions made in this study](#14-decisions-made-in-this-study)
15. [Open questions for the owner](#15-open-questions-for-the-owner)
16. [Sources](#16-sources)

---

## 1. The line we hold

Flightdeck does not build a harness. Model providers, tools, permissions and context stay the
harness's job. Yet the renderer, the registry and the ledger all touch context. The line:

**We render and show. The harness loads.**

| Flightdeck does | The harness does |
|---|---|
| Reads the instruction files the user names in Flightdeck's config | Discovers and loads its own instruction files |
| Writes a native file, once the user opts in for that target, with a digest header | Reads that native file like any other |
| Writes MCP server entries into the harness's own config | Connects to MCP servers, negotiates, exposes tools |
| Reads what the harness recorded about loaded context, and shows it | Decides what enters the model's context |
| Turns a memory write into a proposal, when the user opts in | Writes and reads its own memory |
| Offers a restart through the harness's resume command | Restores the conversation on resume |

Consequences:

- Nothing Flightdeck renders needs Flightdeck at run time. A harness started from a plain
  terminal loads the same files. Flightdeck never injects context through a hook or a wrapper.
- Flightdeck never edits a file it did not write, except one named key in a config file (the MCP
  server table), and only after a diff and an explicit flag.
- When Flightdeck cannot see what a harness loaded, the ledger says "unknown". It never guesses.

## 2. The bar

The owner's rule: match the popular tools in this space (herdr, Paseo, Rex) and add only cheap
guards. For this part the bar is low, because none of them does it:

| Area | Paseo | herdr | Rex | Flightdeck |
|---|---|---|---|---|
| User instruction files | Not handled | Not handled | Not handled | Renders them per harness, opt-in |
| MCP config | Can inject its own MCP tools into agents it starts (opt-in host setting) | None | None | Neutral registry rendered per harness |
| Agent manual | Not found | `hc --skill` prints one | `rex skill setup` installs one | Agent kit (separate task) |
| Context ledger | None | None | None | Flight rules |

So Part 8 is new ground, not hardening. Keep it simple: dry run, diff, explicit write, digest.
Add no locking, no background sync, no watcher daemons beyond what a stage needs.

## 3. Terms

| Term | Meaning here |
|---|---|
| source | An instruction file the user names in Flightdeck's config. Any layout: one file, a file with imports, several files per machine or per project. |
| target | One native file Flightdeck may write, for one harness, at one scope. |
| render | Flatten sources into one target's text, plus a digest header. |
| digest | A hash over every byte that went into a render. Stale detection compares digests. |
| inventory | The neutral MCP server list in Flightdeck's config. |
| ledger | Per session: every file, memory, hook injection, skill list and MCP instruction block that reached the agent. |
| proposal | A memory write held for the user's approval. |
| scope | The harness's own level for a context entry: managed, user, project, local, memory, session, skill, mcp. |

---

## 4. Instructions per harness

### 4.1 Where each harness looks

| | Claude Code | Codex | opencode V2 | pi |
|---|---|---|---|---|
| Global file | `~/.claude/CLAUDE.md`, plus every `.md` under `~/.claude/rules/` | `$CODEX_HOME/AGENTS.override.md`, else `$CODEX_HOME/AGENTS.md` (home defaults to `~/.codex`) | `~/.config/opencode/AGENTS.md` | In the agent directory (`$PI_CODING_AGENT_DIR`, default `~/.pi/agent`): `AGENTS.override.md`, else `AGENTS.md`, else `CLAUDE.md` |
| Managed (admin) file | Platform path, or `claudeMd` in managed settings; loads first; cannot be excluded | Admin and managed config | System config dir and MDM (docs) | None |
| Project files | Root to cwd: `CLAUDE.md` or `.claude/CLAUDE.md`, then `CLAUDE.local.md` at each level; `.claude/rules/**/*.md` | Project root (git root) down to cwd; at most one file per directory: `AGENTS.override.md`, `AGENTS.md`, then names in `project_doc_fallback_filenames` | `AGENTS.md` from cwd up to home (outside home: up to the project root) | Filesystem root to cwd; one file per directory: `AGENTS.override.md`, `AGENTS.md`, `CLAUDE.md` |
| `AGENTS.md` | Read natively since 2.1.277, but by default only when no `CLAUDE.md`, `.claude/CLAUDE.md` or `CLAUDE.local.md` exists on the path. A setting can load both | Native | Native, and the only name | Native, first choice |
| `CLAUDE.md` | Native | Only if listed in `project_doc_fallback_filenames` | Never read | Fallback per directory |
| Nested, below cwd | On demand when Claude reads a file there | No | On demand when the agent reads or lists there | No |
| Path-scoped rules | `.claude/rules` with `paths:` frontmatter | No | No | No |
| Extra files from config | No (`--add-dir` plus an env var loads another directory's files) | No | `instructions` array: paths, globs, `~/` paths, URLs; loads after `AGENTS.md` (V2 docs; an earlier build parsed it but did not load it) | No |
| `@` imports | Native | No: the line passes as text | No | No |
| Size limits | A file over 4 MiB is skipped whole | `project_doc_max_bytes`, 32 KiB by default; later files are cut | None documented | None documented |
| Order | Managed, user, project root to leaf; local after regular at each level | Global, then root to leaf; later wins | Global, then upward files, then `instructions` | Global, then root to leaf |
| Disable | `claudeMdExcludes` globs; setting sources | — | `OPENCODE_DISABLE_PROJECT_CONFIG=1` skips project files | `--no-context-files` |
| Trust needed | External imports in a project file need a one-time approval; user files load without it | Project `.codex/` config, hooks and rules need trust | — | Context files load without trust; `.pi/` config needs it |

Also in the system prompt, and outside this spec's renderer: pi's `SYSTEM.md` (replaces the base
prompt) and `APPEND_SYSTEM.md`; Claude's `--append-system-prompt`; Codex's
`model_instructions_file`, which **replaces** Codex's base instructions and must never be used as
an "extra instructions" key.

### 4.2 Trap routes

Each harness has a route that looks like "add one more file" and instead hides another file.
The renderer must never take one of these routes on its own.

| Harness | Trap | What goes wrong |
|---|---|---|
| Claude Code | Write an `AGENTS.md` into a project that has a `CLAUDE.md` | Claude reads the `CLAUDE.md` only; the `AGENTS.md` is silently ignored |
| Claude Code | Write a `CLAUDE.local.md` into a project that relies on `AGENTS.md` | `CLAUDE.local.md` counts as a `CLAUDE.md`, so `AGENTS.md` stops loading |
| Claude Code | A `SessionStart` hook that prints `AGENTS.md` | Once Claude reads `AGENTS.md` itself, the text arrives twice |
| Codex | Write `AGENTS.override.md` in the Codex home | It wins over the user's own `AGENTS.md` there, which disappears |
| Codex | Point `model_instructions_file` at an instruction file | Codex loses its base tool and safety instructions |
| Codex | A global file near 32 KiB | Project files are cut at the end, with no warning in the TUI |
| opencode | Rely on a `CLAUDE.md` | V2 never reads one, global or project |
| pi | Write `AGENTS.md` into the agent directory when the user keeps a `CLAUDE.md` there | `AGENTS.md` wins at that level; the user's `CLAUDE.md` stops loading |
| pi | Write `AGENTS.override.md` anywhere | It replaces `AGENTS.md` or `CLAUDE.md` in that directory |
| pi | Use `APPEND_SYSTEM.md` | One file per level, no merging: the user's own append file would be replaced |
| Any | Generate a native file from a source that holds `@` imports, for a harness without imports | Everything behind the import line is lost; the model sees a bare `@path` |
| Any | A user who already runs a loader (an extension, plugin or hook that expands their files) | A rendered native file delivers the same text a second time |

### 4.3 Pick-up of an edit

| Harness | When an instruction edit reaches a running session |
|---|---|
| Claude Code | At the next session start. After a compaction it re-reads instruction files (`load_reason` `compact`), so an edit can arrive then |
| Codex | Next run only; discovery runs once per run |
| opencode | Before the next model request, for the global and upward files. Nested files: next session |
| pi | After `/reload`, or the next session |

---

## 5. Import semantics

The renderer expands imports for harnesses that cannot. One rule set, matched to Claude Code,
because a user who writes imports writes them for Claude Code first:

1. **What is an import.** A line whose only content, after trimming, is `@` plus a path. A `@`
   anywhere else in a line is text (mentions, `user@host`).
2. **Code is not an import.** Skip fenced code blocks (backtick and tilde fences) and inline code
   spans.
3. **Paths.** `~/` is the home directory. An absolute path stays. A relative path resolves
   against the importing file's directory, never the working directory. A backslash before a
   space keeps the space in the path. A quoted path is not an import.
4. **Depth.** The root source is depth 0. An import may reach depth 4 (four hops, five levels in
   all). This matches the current Claude Code docs, which lowered the limit from five hops. An
   import at depth 5 or deeper is dropped. The renderer reads the cap per harness version from a
   table, so a later change is one edit.
5. **Cycles.** Keep a set of real paths already expanded on the current branch. A path seen
   again is dropped.
6. **One parent.** A file reached by two routes is included once, at its first position. Later
   routes are dropped and reported in the dry run.
7. **Missing targets.** Skipped silently in the output. Missing is normal: a file may exist only
   on some machines. The dry run lists each skipped path so the user can see it.
8. **Large files.** A file over 4 MiB is skipped whole, as Claude Code does.
9. **Imports-only output.** When a target needs only the imported text (the root file already
   reaches the harness another way), collect the depth-0 import chunks only. A deeper chunk is
   already inside its parent; collecting it again duplicates it.

The import line itself is replaced by the imported text. A dropped or missing import leaves
nothing behind; the model never sees a bare `@path`.

---

## 6. The instruction renderer

### 6.1 Sources

The user names sources in Flightdeck's config, each with a scope and an order:

| Field | Meaning |
|---|---|
| `path` | The file. It may hold imports |
| `scope` | `global` (every session on this host) or `project` (one project, by its repo) |
| `hosts` | Optional allow list of host ids; default all |
| `harnesses` | Optional allow list; default all |
| `order` | Position among sources of the same scope |

There are no fixed layer names. One file, a root file with imports per machine, or a separate
file per host are all just sources. A source may also be the harness's own native file (for
example the Claude global file): then it is read, never written.

### 6.2 Targets

Project scope: Flightdeck **never writes into a project folder**. Projects already carry their own
`AGENTS.md` or `CLAUDE.md`, loaded natively. The ledger shows them; the renderer leaves them.

Global scope, one target per harness:

| Harness | Target | Notes |
|---|---|---|
| Claude Code | Nothing, when the only global source is Claude's own file. Otherwise one file in the user rules directory, `~/.claude/rules/flightdeck.md` | User rules load in every project without an approval dialog. The user's `CLAUDE.md` is never touched. Imports could stay, but the file is flattened anyway so its digest covers every byte |
| Codex | `$CODEX_HOME/AGENTS.md`, flattened | Written only when absent or when it already carries a Flightdeck header. Never `AGENTS.override.md`. Warn above 24 KiB: it shares the 32 KiB budget with project files |
| opencode | A flattened file in Flightdeck's state directory, listed in the global config's `instructions` array | Patches only that array, and only when the installed version loads it (probe). Fallback: `~/.config/opencode/AGENTS.md`, only when absent or carrying our header |
| pi | `AGENTS.md` in the agent directory, flattened | Only when the directory holds no `AGENTS.override.md`, `AGENTS.md` or `CLAUDE.md` of the user's, or the file carries our header |

Before it offers a target, the renderer checks for an existing loader: a non-empty native file
that Flightdeck did not write, or an extension, plugin or hook that names the user's sources. If
it finds one, the target shows "already loaded by the user's setup" and the renderer offers no
write. This keeps working setups alone, as the task requires.

### 6.3 The header

The first lines of every rendered file:

```
<!-- Rendered by Flightdeck. Do not edit; edit the sources and render again.
     digest: sha256:<hex>  renderer: 1  at: <RFC 3339 time>
     sources: <n> read, <m> missing -->
```

The header names counts, not paths: a rendered file may be read by people who should not learn
the user's layout. The full source list lives in Flightdeck's state beside the digest.

### 6.4 The digest

SHA-256 over, in order: the renderer format version, the harness name, then for each file the
expansion visited, its real path, a marker (`read`, `missing`, `cycle`, `depth`, `duplicate`,
`too-large`) and, when read, its bytes. Rules:

- Any change to any source changes the digest.
- Creating a file that was missing changes the digest, because the marker changes.
- Two hosts with the same sources render the same digest.
- The digest excludes the render time.

### 6.5 Dry run and write

- `render --dry-run` prints a unified diff per target against the file on disk, the skipped
  imports, any trap the write would trigger, and writes nothing. It is the default.
- `render --write <target>` writes one named target. There is no "write all" in the first
  version.
- A write goes to a temporary file in the same directory, then renames over the target, so a
  harness never reads half a file.
- A target inside any git work tree raises a warning that names the repository: a rendered file
  may hold private text. The user decides.
- The renderer refuses to overwrite a file that carries no Flightdeck header, and says why.

---

## 7. MCP per harness

### 7.1 Config files and shape

| | Claude Code | Codex | opencode V2 | pi |
|---|---|---|---|---|
| User file | `~/.claude.json`, top-level `mcpServers` | `~/.codex/config.toml`, `[mcp_servers.<name>]` | Global `opencode.json` or `.jsonc`, `mcp.servers.<name>` | `~/.pi/agent/mcp.json`, `mcpServers` |
| Project file | `.mcp.json` at the repo root (needs approval); local scope inside `~/.claude.json` per project | Trusted `.codex/config.toml` | Project `opencode.json[c]` | Trusted `.pi/mcp.json` |
| stdio | `command`, `args`, `env` | `command`, `args`, `env`, `env_vars` (names passed through), `cwd` | `type: local`, `command` as one array, `environment`, `cwd` | `command`, `args`, `env`, `cwd` |
| HTTP | `type: http`, `url`, `headers`, `headersHelper` | `url`, `bearer_token_env_var`, `http_headers`, `env_http_headers`, `http_headers_helper` | `type: remote`, `url`, `headers`, `oauth` | `url`, `headers`, `oauth`; SSE refused |
| Env reference | `${VAR}`, `${VAR:-default}` | By name in the keys above | `{env:NAME}` | `${VAR}`, or `!command` as a whole value |
| Off switch | `/mcp` disable, saved per project in `~/.claude.json` | `enabled = false` | `disabled: true` | `enabled: false` |
| Same name in two scopes | Whole entry, highest wins: local, project, user, plugin, connector | Closest trusted layer wins | Higher file replaces the whole object | Project replaces user; an entry with only `enabled`/exposure keys overrides those keys |
| CLI | `claude mcp add`, `add-json`, `remove`, `--scope` | `codex mcp add`, `list`, `login` | `opencode mcp add`, `list`, `auth` | `pi mcp add`, `remove`, `list`, `login` |
| Server instructions | Reach the model; cut at 4,096 characters | Reach the model; first 512 characters matter most | Reach the model (docs) | First line used as the server's description when none is set |

### 7.2 Files that mix config with state

| File | State the harness writes into it | Rule for Flightdeck |
|---|---|---|
| `~/.claude.json` | OAuth and UI state, trust, per-project state, local-scope servers, `disabledMcpServers`. Claude rewrites it while it runs | Change only the top-level `mcpServers` entries Flightdeck owns. Never rewrite the file |
| `~/.codex/config.toml` | Project trust entries, notices, settings written by the TUI and `codex mcp add` | Format-preserving edit of the `[mcp_servers.<name>]` tables Flightdeck owns |
| `~/.pi/agent/mcp.json` | `/mcp` saves enabled and exposure changes into the file that defines the server | Edit only owned entries; keep the user's `enabled` and exposure keys when re-rendering |
| `opencode.json[c]` | `opencode mcp add` writes servers. Credentials live in the database, not here | Format-preserving JSONC edit of owned entries; keep comments |

"Owned" means a server name Flightdeck rendered, recorded in its state with the entry's digest.
Flightdeck never changes or deletes an entry it does not own. If an owned entry was edited by
hand since the last render (its digest differs), the dry run shows the edit and the write needs
`--force` for that server.

### 7.3 Pick-up of a change

| Harness | New or changed server in a running session |
|---|---|
| Claude Code | Not documented for hand edits: treat as a restart. `/mcp` reconnects a configured server. `list_changed` from a server refreshes its tools live |
| Codex | Restart (the app and IDE ask for one after a change) |
| opencode | Not documented: treat as a restart. `/mcps` connects or disconnects a configured server |
| pi | `/reload` reads `mcp.json` again; no restart |

---

## 8. The MCP registry

### 8.1 Inventory fields

| Field | Meaning |
|---|---|
| `name` | Letters, digits, `_` and `-`. Unique; the same name in every harness |
| `transport` | `stdio` or `http` |
| `command`, `args`, `cwd` | For stdio |
| `url` | For http |
| `env` | Map of variable name to a value reference: a plain non-secret value, or `secret:<provider>:<reference>` |
| `headers` | Same, for http |
| `hosts` | Allow list of host ids; default all |
| `harnesses` | Allow list; default all four |
| `auth` | `none`, `secret` (references above) or `oauth` (the harness's own sign-in; Flightdeck holds nothing) |
| `timeout` | Optional, seconds; rendered into each harness's own key |
| `instructions_note` | Optional text shown in the ledger beside the server; never rendered |

Which servers each harness gets is the user's choice, through `harnesses`. Lists may differ
per harness; the registry makes the difference visible, not forbidden.

### 8.2 Rendering rules

- Each harness gets its native shape (section 7.1). A field a harness cannot express (for
  example SSE for pi) makes the dry run say "not rendered for pi" with the reason.
- **No secret value ever lands in a rendered file.** A rendered entry holds names or a launcher
  command. An inventory entry with a literal value in a field named like a secret (`token`,
  `key`, `secret`, `password`, `authorization`) is refused.
- **stdio with secrets** renders as a launcher: the harness runs `gff-hab mcp launch <name>`,
  which resolves the references through the user's provider and then execs the real server. It
  works from a plain terminal, as long as the provider is reachable.
- **http with secret headers** uses the harness's own helper hook: Claude's `headersHelper`,
  Codex's `http_headers_helper`, pi's `!command` value. Each calls `gff-hab mcp header <name>`.
  opencode has no helper; there the entry renders with `{env:NAME}` and the user must supply the
  variable, or the server is marked "not rendered for opencode".
- A per-harness config digest is recorded after each write, for stale detection.

### 8.3 Claude's `~/.claude.json`

The plan reserves one choice for `/consensus`: patch the key directly, or drive Claude's CLI.
Facts for that run:

- `claude mcp add-json --scope user` and `claude mcp remove --scope user` write the same key and
  keep the rest of the file. They run Claude's own writer, which already copes with a running
  Claude rewriting the file.
- A direct patch races any running Claude that writes the file. It needs a read, merge and
  atomic rename, and can still lose a write made between the read and the rename.
- The CLI needs the `claude` binary on the host; Flightdeck already requires it to run Claude.

Recommendation for that run: drive the CLI per owned server; patch directly only in tests.

---

## 9. Memory per harness

| | Claude Code | Codex | opencode | pi |
|---|---|---|---|---|
| Native memory | Auto memory, on by default | Memories, off by default | None | None |
| Where | `~/.claude/projects/<project>/memory/`: a `MEMORY.md` index plus one file per memory; shared by every worktree of a repo; `autoMemoryDirectory` moves it | `$CODEX_HOME/memories/` plus a SQLite store; generated in the background | — | — |
| Loaded | First 200 lines or 25 KB of `MEMORY.md` at every session start; topic files read on demand | `memories.use_memories` | — | — |
| Written by | The agent, with its Write and Edit tools, during the session | A background job after sessions (`memories.generate_memories`) | — | — |
| Off switch | `autoMemoryEnabled: false`, or `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` | Leave `[features] memories` unset | — | — |
| Reviewed before use | No | No | — | — |

Extensions and plugins can add memory to pi or opencode. Flightdeck treats that like any other
extension-injected text (section 10).

---

## 10. What each harness records

| | Claude Code | Codex | opencode | pi |
|---|---|---|---|---|
| Where | Session JSONL under the Claude config dir | Rollout JSONL under the Codex home | Its database, read only through the server API | Session JSONL under the agent directory |
| Instruction files | `attachment` records of type `instructions`: one entry per file with `path`, `type` (`User`, `Project`, `Local`, `Managed`, `AutoMem`) and full `content`. Imported files are their own entries. Nested files arrive later as `nested_memory` with `path` and `content` | One concatenated text: a `world_state` record (`agents_md.text`) in 0.166; earlier builds put a user message headed "AGENTS.md instructions for <dir>". No per-file paths | Not recorded in any documented place. The server builds the system prompt per request | Not stored per turn. A compaction entry stores the whole system prompt at that point (`systemMessage`) |
| Memory | The `AutoMem` entry in `instructions`; reads and writes of topic files are ordinary tool calls | Not attributed; memories are part of the injected text when on | — | — |
| Skills | `skill_listing` (names, digests, count); `invoked_skills` (name, source, content) | `host_skills` body in `world_state`; earlier a skills developer message | Not recorded (docs) | Skill descriptions are in the system prompt only |
| MCP server instructions | `mcp_instructions_delta`: added names with their text blocks, removed names | Not seen as a separate record | Not recorded | Not recorded |
| Hook or extension injections | `hook_additional_context`: `hookEvent`, `hookName`, the text. `hook_success`: hook name, command, exit code | Not seen in sampled 0.166 rollouts; the docs do not say whether hook context is saved | Plugin-injected system text is runtime only | `custom_message` entries record extension-injected messages with their `customType` |
| Live signal | The `InstructionsLoaded` hook: `file_path`, `memory_type`, `load_reason` (`session_start`, `nested_traversal`, `path_glob_match`, `include`, `compact`), `parent_file_path` for imports. Not fired for an `AGENTS.md` read through the project-instructions setting | None for instructions | Server events; no documented instruction event | Startup header on screen only |
| Runtime only | Settings values, hooks that only block, `--append-system-prompt` text | Which file each block came from; hook trust state | Plugin system transforms; the system prompt | Extension `before_agent_start` prompt changes; `APPEND_SYSTEM.md` between compactions |

All four formats are undocumented or marked unstable. Each reader keys its parser on the
harness version in the file and fails visibly on an unknown shape, as `harness-adapters.md` rule
6 requires.

---

## 11. Flight rules: the ledger

### 11.1 Entry

| Field | Meaning |
|---|---|
| `kind` | `instruction`, `memory`, `hook`, `skills`, `skill`, `mcp`, `system` |
| `scope` | The harness's level: `managed`, `user`, `project`, `local`, `memory`, `session`, `skill`, `mcp` |
| `source` | A file path, a hook name with its event, a skill name, or an MCP server name |
| `configured_source` | The user's source file in Flightdeck config, when a rendered file's digest matches; else empty |
| `digest` | SHA-256 of the text that reached the agent |
| `bytes` | Size of that text |
| `first_seen` | Time of the record that carried it |
| `last_seen` | Latest repeat (compaction, resume) |
| `attribution` | `recorded` (the harness named it), `matched` (Flightdeck found the file's text inside a recorded block), `computed` (Flightdeck ran the harness's discovery itself) or `unknown` |
| `quote` | For a memory proposal only: the user's words that led to it |

The ledger stores digests and sizes, not text. The app fetches a file's text on demand from the
hab, which reads it from the transcript or the file in place. Text never leaves the hab unless a
client asks for it.

### 11.2 Per harness

- **Claude Code** (stage `task-tqx5onyc`). Build entries from the transcript attachments in
  section 10. Attribution is `recorded`. When the user installed Flightdeck's hook config,
  `InstructionsLoaded` events add the load reason and the parent of each import; the transcript
  stays the primary source because it holds the text. Unknown attachment types become placeholder
  entries with a warning.
- **Codex** (stage `task-7mv5spek`). Run Codex's discovery for the session's cwd (global file,
  then root to cwd) and digest each file: `computed`. Then look for each file's text inside the
  recorded `agents_md` text: found is `matched`, not found is `unknown`. Rendered files match by
  digest. Hook context is `unknown` except Flightdeck's own hooks, which send nothing.
- **opencode** (stage `task-7mv5spek`). Run opencode's discovery (global, upward, `instructions`
  entries) and digest each file: `computed`. Read MCP and skill lists from the server API.
  Plugin text is `unknown`.
- **pi** (later, with its adapter). Discovery as above, `computed`; `custom_message` entries are
  `recorded`; a compaction's system prompt lets the reader `match` files at that point.

### 11.3 What the user sees

Per session, a list grouped by scope. Each row: source, scope, size, first seen, attribution. A
hook injection row names the hook and its event. A changed digest between two loads of the same
source shows as a new row, not an overwrite. "Unknown" rows say why ("Codex does not record which
file this came from").

---

## 12. Memory writes as proposals

Off by default (coordinator call, 2026-10-31; the owner confirms, section 15). When the user
turns it on for a harness:

### 12.1 Claude Code

- A `PreToolUse` hook matches Write and Edit calls whose path is inside the session's auto memory
  directory.
- The hook files a proposal and denies the call, with a reason the agent sees: the memory is
  waiting for the user's approval. Nothing lands on disk.
- The proposal holds: the text the agent tried to write, the target file, the session, the
  project, and a **source quote**: the user message that came before the write, read from the
  transcript.
- On approval, Flightdeck writes the memory file itself (the same path and text, edited if the
  user edited it), or the user moves the rule to one of their own instruction sources. The next
  session loads it the native way.
- Rejected and expired proposals stay in the history.

Why a deny hook and not `autoMemoryDirectory`: moving the directory also moves where Claude reads
memory, so existing memories would vanish from the agent, and a staged write would still land in
the staged file. The deny hook keeps reads native and holds writes.

### 12.2 Codex

Codex memories are off by default and written by a background job, with no hook on the write.
Flightdeck does not intercept them. When a user turns memories on, the ledger lists the memory
files as `memory` entries, `computed`. This is a documented limit.

### 12.3 Record

`text`, `quote`, `harness`, `session`, `project`, `target`, `scope`, `created`, `expires`
(default 30 days), `status` (`pending`, `approved`, `edited`, `rejected`, `expired`),
`decided_by`. Stored in Logbook (Part 12) as a record type; in a small gff-hab store until then.
An unapproved proposal never reaches a rendered file or a memory directory.

---

## 13. Stale sessions and pick-up

A session is stale when a digest it loaded differs from the current one: a rendered instruction
file, or a per-harness MCP config digest.

| Harness | Stale instructions | Stale MCP |
|---|---|---|
| Claude Code | Offer restart through `claude --resume <id>` | Same |
| Codex | Offer restart through `codex resume <id>` | Same |
| opencode | Nothing to do: it picks up global and upward edits before the next request. Show "updates on the next prompt" | Offer a pane restart with `--session <id>` |
| pi | Offer `/reload`, typed through the input path when the pane is idle | Same |

Rules, all from the plan: one session at a time, never all at once; only when the agent state is
`idle` or `done`; the user starts each one; on failure, report it and leave the old session
resumable by hand.

---

## 14. Decisions made in this study

Internal calls, recorded with the reason. Each can be revisited in its stage.

| # | Decision | Reason |
|---|---|---|
| D1 | Import depth cap is four hops (depth 0 to 4), read from a per-version table | Matches the current Claude Code docs, which lowered it from five; the task's "depth 5" counts five levels including the root. A depth-6 import is dropped under either reading |
| D2 | Duplicate files are included once, at the first route; reported in the dry run | Avoids double text; the dry run makes a diamond visible without refusing it |
| D3 | Flightdeck never writes into a project folder | Projects carry their own native files; rendered text may hold private detail |
| D4 | Claude's global target is a file in `~/.claude/rules/`, never `CLAUDE.md` | User rules load everywhere with no dialog; the user's own file stays untouched |
| D5 | Codex target is `AGENTS.md`, never `AGENTS.override.md`; warn above 24 KiB | Override hides the user's file; the 32 KiB budget is shared with project files |
| D6 | opencode target is an `instructions` entry when the installed version loads it, else the global `AGENTS.md` only if absent | Non-destructive first; the docs and an earlier build disagree, so probe |
| D7 | No write where the user already has a loader or a native file Flightdeck did not write | A second delivery duplicates text; overwriting breaks a working setup |
| D8 | The header names counts, not paths | A rendered file may be shared; the source list stays in Flightdeck state |
| D9 | MCP: Flightdeck changes only entries it owns, tracked by name and digest | Several files mix config with state; users also add servers by hand |
| D10 | HTTP secrets go through each harness's header helper; opencode without one gets `{env:NAME}` or no entry | Keeps values out of files without a proxy |
| D11 | Ledger stores digests and sizes; text is fetched on demand | Keeps private text on the hab by default |
| D12 | Claude memory proposals use a `PreToolUse` deny hook, not `autoMemoryDirectory` | Reads stay native; nothing lands before approval |
| D13 | Codex memories are not intercepted; listed in the ledger only | No hook on a background write; matches the bar (nobody intercepts) |
| D14 | opencode stale instructions need no restart; pi uses `/reload` | Both harnesses reload by themselves; a restart would cost the user for nothing |
| D15 | Recommend the CLI over a direct patch for `~/.claude.json`, as input to the planned `/consensus` | Claude's own writer handles a running Claude |

## 15. Open questions for the owner

Only user-visible behaviour is listed.

1. **Memory proposals default.** Proposals ship off; a user turns them on per harness. When on,
   Claude's memory writes are held, and the agent is told "waiting for your approval". Confirm
   off by default, or choose on for new installs.
2. **Ledger text on the phone.** The ledger shows sources, sizes and digests by default; the full
   text of a file or hook injection opens on tap. Confirm that tapping may send instruction text
   to the phone, or keep text on desktop clients only.

## 16. Sources

Opened while writing this spec, 2026-11-01.

- Claude Code memory, imports, rules, `AGENTS.md`, auto memory:
  https://code.claude.com/docs/en/memory
- Claude Code hooks (`InstructionsLoaded`, `additionalContext`, `SessionStart` sources):
  https://code.claude.com/docs/en/hooks
- Claude Code MCP (scopes, precedence, expansion, `headersHelper`, instruction limits):
  https://code.claude.com/docs/en/mcp
- Claude Code transcript attachment types: record shapes (field names only) from local
  transcripts of Claude Code 2.1.3xx; no content reproduced
- Codex `AGENTS.md` discovery: https://learn.chatgpt.com/docs/agent-configuration/agents-md
- Codex MCP: https://learn.chatgpt.com/docs/extend/mcp
- Codex hooks: https://learn.chatgpt.com/docs/hooks
- Codex memories: https://learn.chatgpt.com/docs/customization/memories
- Codex config reference: https://learn.chatgpt.com/docs/config-file/config-reference
- Codex rollout shapes: record types and field names from local rollouts of Codex 0.166.0; no
  content reproduced
- Codex instruction loader (no import handling):
  https://github.com/openai/codex/blob/main/codex-rs/core/src/agents_md.rs
- opencode V2 instructions: https://opencode.ai/v2/docs/instructions/
- opencode V2 MCP servers: https://opencode.ai/v2/docs/mcp-servers/
- opencode V2 config: https://opencode.ai/v2/docs/config/
- pi 1.4.0 docs shipped with the package: `configuration.md`, `mcp.md`, `session-format.md`,
  `sessions.md`, `cli.md`, `security.md`, `slash-commands.md`; online at https://pi.dev/docs/latest
- Paseo MCP injection: https://paseo.sh/docs/mcp
- herdr-core agent manual: https://github.com/herdrdev/herdr-core
- MCP specification: https://modelcontextprotocol.io/specification/latest

Internal inputs: `docs/specs/landscape.md`, `docs/specs/harness-adapters.md`, and the project's
planning notes and research reports on harness configuration. None of their private detail is
reproduced here.
