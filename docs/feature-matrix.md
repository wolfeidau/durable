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

`durable` leads on crash semantics and has a small extension surface. Its two seams, `Model`
and `Tool`, are clean, but apart from MCP tools and `PreToolUse` and `PostToolUse` hooks, only
Go code compiled into the binary can use them. The concrete tools and the Claude and Responses adapters are public packages, so an embedder can reuse them.

Pi and Claude Code have converged on the same set of extension points: context files,
skills, MCP, hooks, subagents and packages. Pi Durable shows how those fit a durable loop.
Pi Durable also ships a stronger durability model in places (tasks, sleep, forks, documents).
`durable` is stricter on three points: `synchronous=FULL` survives power loss, a lease fences
writers across processes, and bash orphans are stopped on recovery.

## Extension surface

| Capability | durable | Pi | Pi Durable | Claude Code |
|---|---|---|---|---|
| Custom tools | Yes: MCP, or Go compiled in | Yes: TypeScript extensions | Yes: `defineTool` | Yes: MCP |
| Replace built-in tools | Part: library only | Yes | Yes: `wraps` | Part: hook `updatedInput`, deny rules |
| Lifecycle hooks | Part: `PreToolUse`, `PostToolUse` | Yes: in-process events | Yes: task hooks | Yes: 33 events |
| Out-of-process hooks | Part: command only | Ext | Ext | Yes: command, HTTP, MCP, prompt, agent |
| Hook can block or rewrite a tool call | Yes: `ToolGate`, or a `PreToolUse` command | Yes | Yes: `beforeTool` | Yes: `PreToolUse` |
| Hook can replace a tool result | Yes: `PostToolUse` command | Yes: `tool_result` | Yes: `afterTool` | Yes: `updatedToolOutput` |
| MCP client | Yes: stdio, HTTP | Yes: stdio, HTTP | Ext: tool-based bridge | Yes |
| Deferred tools, tool search | No | Yes | Yes | Yes: on by default |
| Skills | Yes: Agent Skills spec | Yes: Agent Skills spec | n/d | Yes |
| Slash commands, prompt templates | Part: `/skill:` | Yes | n/a: library | Yes: skills |
| Context files | Yes: `AGENTS.md` | Yes: `AGENTS.md`, `CLAUDE.md` | Ext: prompt sections | Yes: `CLAUDE.md`, `.claude/rules/` |
| Replace or append system prompt | Part: library only | Yes: `SYSTEM.md`, `APPEND_SYSTEM.md` | Yes: sections | Yes: output styles, SDK |
| Plugin packaging | No | Yes: npm, git, local | Part: extension registry | Yes: plugins, marketplaces |
| Custom model providers | Part: `Model` interface; Anthropic and OpenRouter built in | Yes: `registerProvider` | Yes: `pi-ai` | Part: Claude on several clouds |
| Custom commands, flags, shortcuts | No | Yes | n/a: library | Part: commands, keybindings |
| Custom terminal UI | No | Yes: renderers, widgets | n/a: library | Part: status line, mods |
| Project trust gate | No | Yes | n/a: library | Yes |

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
| Compaction | Yes: server-side, Claude 5+, direct Anthropic provider only | Yes | Yes | Yes |
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
| Strips credentials from bash | Part: `ANTHROPIC_*`, `OPENROUTER_*`, `OP_*` | n/d | n/d | n/d |
| OS sandbox | No: deferred, see #9 | Ext: containers | No | Yes |

## Interfaces and embedding

| Capability | durable | Pi | Pi Durable | Claude Code |
|---|---|---|---|---|
| Interactive terminal | Part: line REPL | Yes: TUI | n/a: library | Yes |
| Non-interactive run | Part: piped stdin | Yes: `-p` | n/a: library | Yes: `-p` |
| Structured event output | Part: `Observer`, library only | Yes: JSON mode | Yes: `watch` | Yes: stream JSON |
| RPC over stdio | No | Yes | n/a: library | Part: SDK |
| Embeddable library | Yes: harness, tools, Claude and Responses adapters | Yes: SDK | Yes | Yes: Agent SDK |
| Multiple clients on one session | No | n/d | Yes: read-only opens, `watch` | Part: Remote Control |
| Telemetry | No | n/d | Yes: metrics | Yes |

## Recommended order

Build the seams first, because each later item depends on them. Items 1 to 4 are
prerequisites for a modular harness. Items 5 to 8 are features on top.

| # | Item | Why now | Size |
|---|---|---|---|
| 1 | Done: `tools` and `claude` are public packages | Other modules can reuse them | S |
| 2 | Done: `ToolGate` can allow, deny, ask or rewrite | Replaces `Approve`; rules, hooks and plan mode all build on it | M |
| 3 | Done: `Options.Observer` and `Events` expose every commit | Enables JSON output, telemetry, UI and usage tracking; the CLI doesn't use it yet | M |
| 4 | Done: `AGENTS.md` only, pinned per session in the CLI | Cheapest large gain; config for hooks and MCP also needs a loader | S |
| 5 | Done: `mcp` package and `~/.durable/mcp.json`, pinned per session | Opens up the existing tool ecosystem | M |
| 6 | Done: `skills` package, pinned per session in `skills.json` | The same format as Claude Code, and `.agents/skills` is shared with Pi | S |
| 7 | Done: `hooks` package and `~/.durable/hooks.json`, not pinned; only `PreToolUse` and `PostToolUse` so far | The only practical runtime extension in Go. Existing Claude Code hooks need adapting; see [hooks](hooks.md) | M |
| 8 | Add steering and durable subagents | README next steps 2 and 3. Subagents need multiple conversations per store first | L |

Leave out for now: a TUI, more providers beyond Anthropic and OpenRouter, codemode and the
OS sandbox, which stays deferred on #9. A package format also waits: it adds no capability
beyond MCP, skills and hooks, so build it when someone else needs to install a shared set of
extensions, or when copying one setup across machines or projects becomes a real cost.

## Design constraints

Modularity here has to keep the crash guarantees, which sets five rules.

1. **Commit rewritten input before the tool runs.** `runCall` stores a gate's rewritten input
   in the same commit that marks the call `started`, so a replay or `Recover` uses the
   rewritten arguments. The stored assistant message keeps the model's original arguments, so
   the result carries a `Rewritten` note.
2. **Run gates only on `pending` calls.** `Gate` and `Ask` never run for a `started` call, so a
   gate can be non-deterministic: a crash before `started` simply decides again. A gate's
   decision is not re-evaluated for a `started` call.
3. **Treat a tool as able to disappear.** An MCP server can vanish between runs, so a `started`
   call for an unknown tool records `UnknownOutcome`, not `unknown tool`. MCP tools are always
   `ReplayNever`, and server hints don't change replay or approval, because the server is
   making that claim. A pinned tool whose server has gone stays in the list and returns an error.
4. **Pin the prefix per session.** Context files, skill listings and tool order all feed the
   prompt cache and stored thinking blocks. Snapshot them when the session starts, and don't
   reread them on resume. The CLI does this for `AGENTS.md` in `instructions.md` and for MCP
   tools in `mcp-tools.json`, for skills in `skills.json`, and for the provider, protocol and
   OpenRouter model in `provider.json`, all beside the session database.
5. **Fail hooks closed, and don't pin them.** A failed `PreToolUse` hook denies the call and a
   failed `PostToolUse` hook withholds the result, unless the handler opts into
   `"onFailure": "continue"`. A hook failure is never a gate error, because that leaves the
   call `pending` and blocks the queue on every restart. Hooks don't feed the prompt prefix, so
   they aren't pinned, and a new policy applies at the next start. A broken `hooks.json`
   refuses to start, because running without the user's policy is unsafe.

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
