# Project instructions and skills

Give the model standing instructions through `AGENTS.md` files and reusable [Agent
Skills](https://agentskills.io/specification).

## Project instructions

A new session loads `AGENTS.md` files into the system prompt.

**A cloned repository's `AGENTS.md` goes into the system prompt, so treat it as untrusted input,
especially with `--yes`.**

### Which files load

In this order:

1. `~/.durable/AGENTS.md`.
2. Each `AGENTS.md` from the repository root (the nearest ancestor with a `.git` entry) down to the
   working directory.

The walk stops at the root, and with no repository only the working directory is read. `CLAUDE.md`
isn't read, and `@path` imports stay as literal text.

### Limits and refused files

- Files are limited to 32 KiB each and 64 KiB in total, counting file content but not the wrapper
  tags. Anything over is cut, marked `[truncated]` and reported at startup.
- The budget is spent from the working directory upward, so the most specific files are kept.
- A project `AGENTS.md` symlink that resolves outside the repository is refused, as is anything
  that isn't a regular file. An in-repository symlink must be relative.
- The `~/.durable` file may be a symlink.
- Files containing NUL bytes are skipped.
- A `.git` owned by another user isn't treated as a repository root.

### Pinning

The text is pinned in `instructions.md` next to the session database when the session first starts,
even if no prompt is sent and even if the first request fails. A resume uses the same prompt and
prints a `[context]` line saying pinned instructions are in use.

- To reload, start a new `--session` or delete the session directory. Deleting only
  `session.sqlite` keeps the pin.
- Sessions that predate this feature keep an empty pin, so `AGENTS.md` isn't loaded for them.
- `--no-context` skips loading for a new session.

## Skills

A new session loads [Agent Skills](https://agentskills.io/specification).

**A cloned repository's skills go into the model's context, so treat them as untrusted input,
especially with `--yes`.**

### Where skills come from

In this order:

1. `.durable/skills` then `.agents/skills` in each project directory, nearest first up to the
   repository root.
2. `~/.durable/skills`.
3. `~/.agents/skills`.

- The first skill of a name wins, and a shadowed one gets a warning.
- `.claude/skills` isn't scanned, and Claude Code reads only that location, so a skill meant for
  both needs a copy.
- A project skill that resolves outside the repository is skipped.

### How the model uses them

- The model loads a skill with the `skill` tool. The tool lists the skills in its description and
  needs no approval to read.
- Scripts a skill mentions run through `bash` and ask as usual.
- `/skill:<name> [text]` loads a skill into your own prompt, including one with
  `disable-model-invocation`, which the model never sees.
- `allowed-tools` is ignored.
- Compaction may drop a loaded skill's instructions, and the model can load it again.

### Pinning

The catalog is pinned in `skills.json` next to the session database when the session first starts,
so a resume uses the same tool.

- To reload, start a new `--session`.
- Sessions that predate this feature keep an empty pin.
- `--no-context` skips skills for a new session.
