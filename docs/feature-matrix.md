# Feature matrix

How `durable` compares with Pi, Pi Durable and Claude Code, with a focus on modularity.
Checked on 2026-11-01 against this repository and each project's documentation.

- **Pi**: the `pi` terminal coding agent (`packages/coding-agent`).
- **Pi Durable**: the experimental TypeScript harness library (`packages/durable`). It's
  the closest comparison, because it's a library you build an agent on, not a finished agent.
- **Claude Code**: Anthropic's coding agent.

Key: **Yes** built in. **Ext** not built in, but there's a supported way to add it.
**Part** covers some of it. **No** not available. **n/d** not documented, so don't read it as
a no.

## Summary

`durable` leads on crash semantics and has almost no extension surface. Its two seams,
`Model` and `Tool`, are clean, but only Go code compiled into the binary can use them. The
concrete tools and Claude adapter are public packages, so an embedder can reuse them.

Pi and Claude Code have converged on the same set of extension points: context files,
skills, MCP, hooks, subagents and packages. Pi Durable shows how those fit a durable loop.
Pi Durable also ships a stronger durability model in places (tasks, sleep, forks, documents).
`durable` is stricter on three points: `synchronous=FULL` survives power loss, a lease fences
writers across processes, and bash orphans are stopped on recovery.

## Extension surface

| Capability | durable | Pi | Pi Durable | Claude Code |
|---|---|---|---|---|
| Custom tools | Part: Go, compiled in | Yes: TypeScript extensions | Yes: `defineTool` | Yes: MCP |
| Replace built-in tools | Part: library only | Yes | Yes: `wraps` | Part: hook `updatedInput`, deny rules |
| Lifecycle hooks | No | Yes: in-process events | Yes: task hooks | Yes: 33 events |
| Out-of-process hooks | No | Ext | Ext | Yes: command, HTTP, MCP, prompt, agent |
| Hook can block or rewrite a tool call | Yes: `ToolGate`, Go only | Yes | Yes: `beforeTool` | Yes: `PreToolUse` |
| Hook can replace a tool result | No | Yes: `tool_result` | Yes: `afterTool` | Yes: `updatedToolOutput` |
| MCP client | No | Yes: stdio, HTTP | Ext: tool-based bridge | Yes |
| Deferred tools, tool search | No | Yes | Yes | Yes: on by default |
| Skills | No | Yes: Agent Skills spec | n/d | Yes |
| Slash commands, prompt templates | No | Yes | n/a: library | Yes: skills |
| Context files | No | Yes: `AGENTS.md`, `CLAUDE.md` | Ext: prompt sections | Yes: `CLAUDE.md`, `.claude/rules/` |
| Replace or append system prompt | Part: library only | Yes: `SYSTEM.md`, `APPEND_SYSTEM.md` | Yes: sections | Yes: output styles, SDK |
| Plugin packaging | No | Yes: npm, git, local | Part: extension registry | Yes: plugins, marketplaces |
| Custom model providers | Part: `Model` interface | Yes: `registerProvider` | Yes: `pi-ai` | Part: Claude on several clouds |
| Custom commands, flags, shortcuts | No | Yes | n/a: library | Part: commands, keybindings |
| Custom terminal UI | No | Yes: renderers, widgets | n/a: library | Part: status line, mods |
| Project trust gate | n/a: nothing to load | Yes | n/a: library | Yes |

## Agent loop and context

| Capability | durable | Pi | Pi Durable | Claude Code |
|---|---|---|---|---|
| Streaming output | Part: `OnText`, not committed | Yes | Yes: committed view | Yes |
| Steering during a run | No | Yes | Yes: `whenBusy: "steer"` | Yes: queued messages |
| Queued follow-ups | Part: `Submit` between runs | Yes | Yes: durable inbox | Yes |
| Parallel tool calls | No: one at a time | Yes: default | Yes: task per call | Yes |
| Subagents | No | Ext: example extension | Ext: ownership primitives | Yes |
| Background tasks, sleep | No | Ext | Yes: stored sleep | Yes |
| Scripted tool calls | No | Yes: codemode | Yes: code mode | Part: dynamic workflows |
| Compaction | Yes: server-side, Claude 5+ | Yes | Yes | Yes |
| Manual or guided compaction | No | Yes | Yes | Yes: `/compact` |
| Plan mode | No | Ext: example extension | Ext | Yes |
| Long-term memory | No | Ext | Yes: documents | Yes: auto memory |
| Token and cost tracking | No | Yes | Yes: `pi.usage` | Yes |
| Image input | No | Yes | n/d | Yes |
| Built-in tools | 5: read, list, grep, edit, bash | read, write, edit, bash and others | `CodingTools` | Many, plus web and LSP |

## Durability and sessions

| Capability | durable | Pi | Pi Durable | Claude Code |
|---|---|---|---|---|
| Automatic resume after a crash | Yes: `kill -9` tested | n/d | Yes | Part: manual `--resume` |
| Atomic step commits | Yes | n/d | Yes | n/d |
| Per-tool replay policy | Yes | n/d | Yes: `replay: "safe"` | n/d |
| Unknown-outcome result | Yes | n/d | Yes: interrupted result | n/d |
| Orphaned process cleanup | Yes: `flock`, process group | n/d | n/d | n/d |
| Exactly-once input | Yes: `request_id` | n/d | Yes: `requestId` | n/d |
| Fenced single writer | Yes: lease | n/d | No: no cross-process locking | n/d |
| Survives power loss by default | Yes: `synchronous=FULL` | n/d | No: `synchronous=NORMAL` | n/d |
| Storage backends | SQLite, any `database/sql` driver | JSONL | Memory, IndexedDB, SQLite, JSONL, Durable Objects | JSONL |
| Multiple conversations per store | No | n/a: one per file | Yes | n/a: one per file |
| Fork or branch | No | Yes: tree, fork, clone | Yes: `fork` | Yes: `--fork-session`, `/branch` |
| List or pick sessions | No | Yes | n/d | Yes |
| Rewind file changes | No | n/d | n/d | Yes: `/rewind`, edits only |
| Cancel durably | Yes: `Cancel` | n/d | Yes: `abort` | n/d |
| Tool cleanup hook after a crash | Yes: `Recoverer` | n/d | n/d | n/d |

## Safety

| Capability | durable | Pi | Pi Durable | Claude Code |
|---|---|---|---|---|
| Default posture | Ask, except read-only tools | No approval prompts | Host decides | Classifier or ask |
| Permission modes | Part: default, `--yes` | Ext | Ext | Yes: 6 modes |
| Allow, ask, deny rules | No | Ext | Ext | Yes |
| Workspace path scoping | Part: file tools only | No: cwd isn't a boundary | Ext | Yes |
| Strips credentials from bash | Part: `ANTHROPIC_*`, `OP_*` | n/d | n/d | n/d |
| OS sandbox | No: deferred, see #9 | Ext: containers | No | Yes |

## Interfaces and embedding

| Capability | durable | Pi | Pi Durable | Claude Code |
|---|---|---|---|---|
| Interactive terminal | Part: line REPL | Yes: TUI | n/a: library | Yes |
| Non-interactive run | Part: piped stdin | Yes: `-p` | n/a: library | Yes: `-p` |
| Structured event output | Part: `Observer`, library only | Yes: JSON mode | Yes: `watch` | Yes: stream JSON |
| RPC over stdio | No | Yes | n/a: library | Part: SDK |
| Embeddable library | Yes: harness, tools, Claude adapter | Yes: SDK | Yes | Yes: Agent SDK |
| Multiple clients on one session | No | n/d | Yes: read-only opens, `watch` | Part: Remote Control |
| Telemetry | No | n/d | Yes: metrics | Yes |

## Recommended order

Build the seams first, because each later item depends on them. Items 1 to 4 are
prerequisites for a modular harness. Items 5 to 9 are features on top.

| # | Item | Why now | Size |
|---|---|---|---|
| 1 | Done: `tools` and `claude` are public packages | Other modules can reuse them | S |
| 2 | Done: `ToolGate` can allow, deny, ask or rewrite | Replaces `Approve`; rules, hooks and plan mode all build on it | M |
| 3 | Done: `Options.Observer` and `Events` expose every commit | Enables JSON output, telemetry, UI and usage tracking; the CLI doesn't use it yet | M |
| 4 | Load `AGENTS.md` and `CLAUDE.md`, and pin them per session | Cheapest large gain; config for hooks and MCP also needs a loader | S |
| 5 | Add MCP client tools | Opens up the existing tool ecosystem | M |
| 6 | Add skills using the Agent Skills spec | The same skills work in Pi and Claude Code | S |
| 7 | Add subprocess hooks using Claude Code's JSON contract | The only practical runtime extension in Go, and existing hooks may carry over | M |
| 8 | Add steering and durable subagents | README next steps 2 and 3. Subagents need multiple conversations per store first | L |
| 9 | Add a package format | Wait until items 5 to 7 give it something to bundle | M |

Leave out for now: a TUI, multiple providers (`Model` already allows them), codemode and the
OS sandbox, which stays deferred on #9.

## Design constraints

Modularity here has to keep the crash guarantees, which sets four rules.

1. **Commit rewritten input before the tool runs.** `runCall` stores a gate's rewritten input
   in the same commit that marks the call `started`, so a replay or `Recover` uses the
   rewritten arguments. The stored assistant message keeps the model's original arguments, so
   the result carries a `Rewritten` note.
2. **Run gates only on `pending` calls.** `Gate` and `Ask` never run for a `started` call, so a
   gate can be non-deterministic: a crash before `started` simply decides again. A gate's
   decision is not re-evaluated for a `started` call.
3. **Treat a tool as able to disappear.** An MCP server can vanish between runs. Today a
   `started` call for an unknown tool records `unknown tool`, which is wrong for a tool that
   may have run. It should record `UnknownOutcome`. MCP tools default to `ReplayNever`, and
   server read-only hints don't skip approval, because the server is making that claim.
4. **Pin the prefix per session.** Context files, skill listings and tool order all feed the
   prompt cache and stored thinking blocks. Snapshot them when the session starts, and don't
   reread them on resume.

Go also changes the extension model. Pi loads TypeScript into its process, which Go can't do
well, because `plugin` is fragile and needs matching builds. So use three tiers: Go interfaces
for embedders, MCP for tools, and subprocess JSON for hooks.

## Sources

- [Pi coding agent docs](https://github.com/earendil-works/pi/tree/main/packages/coding-agent/docs)
- [Pi Durable README](https://github.com/earendil-works/pi/blob/main/packages/durable/README.md)
- [Extend Claude Code](https://code.claude.com/docs/en/features-overview)
- [Claude Code hooks](https://code.claude.com/docs/en/hooks)
- [Claude Code permission modes](https://code.claude.com/docs/en/permission-modes)
- [Claude Code checkpointing](https://code.claude.com/docs/en/checkpointing)
- [How Claude Code works](https://code.claude.com/docs/en/how-claude-code-works)
- [Agent Skills specification](https://agentskills.io/specification)
