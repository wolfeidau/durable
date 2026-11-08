# Web UI

The browser UI for `durable-server`, from its first cut onward. The server's design, the API and
the validation layers are in [architecture](architecture.md); access and auth are in
[server access](server-access.md).

**Status: first iteration in progress.** The first cut is built (#80). This iteration turns it from
working into usable, based on feedback from real sessions.

## Built

As of #80, the UI works end to end without the CLI:

- A session list with each session's status (pending approvals, queued or running work) and a
  new-session form.
- The live transcript as plain text, with streamed model text and `bash` output, tool calls and
  their results, submission errors and compaction events.
- The approval prompt, submit, cancel, and a status line that reports reconnects, missing auth and a
  session taken over by another process.
- Resume across server restarts under one epoch, and replay from 0 after a restore.

## First iteration

### Done means

- Every item under [feedback](#feedback) is fixed or explicitly deferred.
- Transcript rendering holds up with thousands of events.
- `durable-server` ships as a release binary with the UI embedded.

Then the browser tests ([#83](https://github.com/wolfeidau/durable/issues/83)) start, against the UI
as it stands.

### Feedback

From using the first cut in real sessions. Each item gets a status as it's picked up.

| Item | Status |
|---|---|
| | |

### Priorities

Provisional, written before the feedback above. Reorder once it's in.

1. **Transcript rendering at scale.** Each event redraws every row, which won't hold at a few
   hundred events. Carried over from slice K.
2. **Readable output.** Markdown for assistant messages, and tool calls and results that collapse.
3. **Release binaries.** `.goreleaser.yaml` builds only `cmd/durable`. Add `durable-server`, with
   `mise run web:build` before the Go build so the binary embeds the UI. CGO stays, so each target
   still builds on a matching host.

## Known gaps

- **No Cancel for old turns.** Cancel needs a `request_id`, which turns queued before #78 don't
  have.
- **Anthropic doesn't pin its model.** Reopening an anthropic session uses the built-in default.
- **Opening resumes work.** Watching a session restarts its queued work, including a copy made by
  `durable.Restore`. It only matters once the UI offers restore; see
  [open questions](architecture.md#open-questions).

## Out of scope

- Restore from the UI, until the question above is settled.
- Sharing a session live with the CLI. The lease hands a session over instead.
- Remote access, TLS and proxies, which wait for [hosting](architecture.md#hosting-is-deferred).
