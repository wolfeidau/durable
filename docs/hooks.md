# Hooks

Run your own commands around tool calls to allow, deny, rewrite or annotate them.

**Hooks run as you and aren't sandboxed, and they aren't a security boundary:** an approved `bash`
command can edit `~/.durable/hooks.json`, and the change applies at the next start.

## Configuration

`~/.durable/hooks.json` runs commands around tool calls, using the JSON contract from Claude Code's
hooks. Only `PreToolUse` and `PostToolUse` exist, and each event starts one process.

```json
{
  "hooks": {
    "PreToolUse": [
      { "matcher": "bash", "hooks": [{ "type": "command", "command": "/path/to/policy.sh", "timeout": 30 }] }
    ]
  }
}
```

- Handlers need `"type": "command"` and a `command`, which runs under `bash -c`.
- `"args"` (even `[]`) runs the command directly with no shell.
- A `matcher` is empty or `*` for every tool, a name or `a|b` list, or a Go regexp.

## Hook input and output

The hook gets JSON on stdin and answers with JSON on stdout.

- Input fields: `hook_protocol` (currently 1), then `session_id`, `cwd`, `permission_mode`,
  `hook_event_name`, `tool_name`, `tool_input` and `tool_use_id`, plus `tool_response` for
  `PostToolUse`.
- Unknown fields in a JSON answer are a failure, and so is `"continue": false`.

### `PreToolUse`

- Exit 2 denies with stderr as the reason.
- Under `hookSpecificOutput`, `permissionDecision` is `allow`, `deny` or `ask`, and `updatedInput`
  replaces the input.
- Several hooks combine as deny, then ask, then allow.
- A hook `allow` skips the approval prompt, and with no decision the normal approval applies.
- `additionalContext` is ignored.
- A top-level `decision` is a failure.

### `PostToolUse`

- `hookSpecificOutput.updatedToolOutput` (a string or `{"output": "..."}`) replaces the result
  before it reaches the model and the session database.
- `additionalContext` or a top-level `"decision": "block"` with `reason` appends text.
- Exit 2 appends stderr.

## Trust and scope

- Hooks come only from the user's file, never from the repository.
- A broken file refuses to start, including on resume.
- Hooks apply under `--yes` too, where a hook `ask` denies.
- Changes apply at the next start, because hooks aren't pinned.

## Failure

Hooks fail closed. A timeout (default 60 seconds), bad output, a non-zero exit other than 2 or a
start failure denies a `PreToolUse` call, and withholds a `PostToolUse` result, unless the handler
sets `"onFailure": "continue"`. The model sees a fixed message, and the cause goes to `hooks.log`
next to the session database.

## Process

- Hooks run in an empty temp folder, unless `"useWorkspaceCwd": true`.
- They run in order.
- They don't inherit `ANTHROPIC_*`, `OPENROUTER_*` or `OP_*`.
- Stdout over 1 MiB is a failure.

## Differences from Claude Code

- Tool names are durable's, so `bash` not `Bash`. A matcher naming no tool prints a `[hooks]
  warning` at startup.
- `tool_response` is `{"output", "is_error"}`, not tool-specific.
- `PostToolUse` also runs for tool errors, because there is no separate failure event.
- Failures block by default, where Claude Code lets the call proceed.
- Hooks run one after another in config order, not concurrently.
- Only `type: "command"` works, and `if`, `async`, `http` and other fields are load errors.
- Fields we don't support in a hook's answer fail the hook instead of being ignored, including
  `updatedMCPToolOutput` and `"continue": false`.
- An event name with the wrong case, such as `preToolUse`, is a load error. Other unsupported events
  print a warning.

## Limits

- A `PreToolUse` hook can run more than once after a crash, so it must be safe to repeat.
- `PostToolUse` doesn't run for a call a crash interrupted, which is recorded as an unknown outcome
  with no output.
- Nothing stops a running hook after `kill -9`.
