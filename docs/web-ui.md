# Web UI

The browser UI for `durable-server`, from its first cut onward. The server's design, the API and
the validation layers are in [architecture](architecture.md); access and auth are in
[server access](server-access.md).

**Status: paused until phase 4 works end to end.** The first cut is built (#80). The rest of this
iteration waits for [remote execution](architecture.md#phases), so feedback comes from using the
whole architecture rather than the UI on its own. The feedback below is recorded for then.

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

| # | Item | Status |
|---|---|---|
| 1 | **Visual design.** The UI should look as polished as `../airunner`'s dashboard, which uses a high-contrast dark theme, a header bar, cards and tables. Bring that design over in durable's token style (light and dark, `prefers-color-scheme`), not as a copy of its 900 lines of CSS. | Open |
| 2 | **Browsing for a working directory.** The form takes a typed absolute path, because `CreateSession` takes a path and a browser can't read the server's filesystem: a native folder picker returns no absolute path. Browsing needs a server RPC that lists directories. See [directory browsing](#directory-browsing). | Superseded for sandboxed sessions: they [name a repo](remote-execution.md#a-session-names-a-repo-not-a-directory), so the form becomes a repo picker (slice R) |
| 3 | **Running the harness in [OpenShell](https://docs.nvidia.com/openshell/about/architecture).** Investigate splitting durable into a harness/agent process and a web server, with the harness in an OpenShell sandbox. This reverses [the harness runs outside the sandbox](architecture.md#the-harness-runs-outside-the-sandbox), so it's a phase 4 design question rather than UI work. See [OpenShell](#openshell). | Open, investigation |
| 4 | **Keep the session list and the one-view layout.** A full-page list, or one session filling the page, is preferred over a thread sidebar. Keep this layout as the design changes. | Keep |

### Directory browsing

Options, cheapest first:

1. **Recent directories.** Offer the cwds of existing sessions, which `ListSessions` already returns. No new RPC, but no browsing.
2. **`ListDirectories(path)`.** Returns the subdirectories of an absolute path, starting from `$HOME`. It lists names only and never file contents. A leaked token already means a shell anywhere the user can reach (see [security](architecture.md#security)), so this adds no new kind of exposure, but it still needs the token, and its tests.
3. **Configured roots.** A `--root` flag on `serve` limits both browsing and `CreateSession` to listed directories. It narrows what a leaked token can reach, and reverses the decision to accept any directory.

Recommended: 1 and 2 together, with 3 left until hosting.

This holds while the harness and the tools run on the server's machine. Once phase 4 moves the
workspace into a sandbox, the server's filesystem is no longer where the agent works, and choosing a
directory becomes choosing a repo or workspace. Settle that in the phase 4 design before building 2.

### OpenShell

NVIDIA OpenShell runs an agent in a container with declarative policy for the filesystem, network, processes and inference. Filesystem and process rules are fixed when the sandbox is created; network and inference rules can change at runtime. It's alpha and single-player.

Questions to answer before choosing it:

- **Where the model key and `session.sqlite` live.** The architecture keeps both out of the sandbox, so `bash` can't read the key or alter the log. If the harness runs inside, OpenShell's inference policy would have to proxy model calls with the key held outside, and the database would need to sit outside the agent's writable paths.
- **What a lost sandbox costs.** With the harness outside, a lost sandbox is an unknown-outcome tool call and the session survives. With it inside, the session is lost unless the database lives outside.
- **The split itself.** No plan for a harness/server split is written down yet. Phase 4's design is a server that holds the harness and dials out to a worker that runs tools. A split with the harness in the sandbox is a different design and needs its own decision record.
- **Toolchain.** Issue #9 rejected containers locally because the agent needs the user's real toolchain. Does OpenShell's policy model change that?

### Priorities

Reordered after the feedback above. OpenShell (item 3) is a phase 4 investigation and isn't on
this list.

1. **Visual design** (item 1), keeping the one-view layout (item 4).
2. **Directory browsing** (item 2): recent directories, then `ListDirectories`.
3. **Transcript rendering at scale.** Each event redraws every row, which won't hold at a few
   hundred events. Carried over from slice K.
4. **Readable output.** Markdown for assistant messages, and tool calls and results that collapse.
5. **Release binaries.** `.goreleaser.yaml` builds only `cmd/durable`. Add `durable-server`, with
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
