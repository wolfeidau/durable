import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import {
  ApprovalStatus,
  type Event,
  EpochMismatchSchema,
  EventKind,
  Role,
  SessionService,
} from "../gen/durable/v1/durable_pb";

let token = new URLSearchParams(location.hash.slice(1)).get("token") ?? "";

const transport = createConnectTransport({
  baseUrl: location.origin,
  interceptors: [
    (next) => (req) => {
      req.header.set("Authorization", `Bearer ${token}`);
      return next(req);
    },
  ],
});
const client = createClient(SessionService, transport);

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const set = (id: string, v: unknown) => ($(id).textContent = String(v));

let epoch = "";
let cursor = 0n;
let count = 0;
const stats = { gaps: 0, dupes: 0, reconnects: 0, epochResets: 0, heartbeats: 0 };
let sessionId = "";
let abort: AbortController | undefined;
let resumeAuth: (() => void) | undefined;

function render() {
  set("epoch", epoch);
  set("cursor", cursor);
  set("count", count);
  for (const [k, v] of Object.entries(stats)) set(k, v);
}

function clearList() {
  $("events").textContent = "";
  count = 0;
}

function trunc(s: string) {
  return s.length > 200 ? s.slice(0, 200) + "..." : s;
}

function line(e: Event) {
  const kind = EventKind[e.kind];
  let s = `${e.seq} ${kind}`;
  if (e.message) {
    s += ` ${Role[e.message.role]}: ${trunc(e.message.text)}`;
    if (e.message.toolCalls.length) s += ` [calls: ${e.message.toolCalls.map((c) => c.name).join(",")}]`;
    if (e.message.isError) s += " is_error";
  }
  if (e.call) s += ` call=${e.call.name}`;
  if (e.isError) s += " is_error";
  if (e.error) s += ` error=${trunc(e.error)}`;
  if (e.kind === EventKind.APPROVAL_REQUESTED || e.kind === EventKind.APPROVAL_DECIDED) {
    s += ` approval=${e.approvalSeq} ${ApprovalStatus[e.approval]}`;
  }
  return s;
}

function append(e: Event) {
  const div = document.createElement("div");
  div.className = "ev";
  div.textContent = line(e);
  if (e.kind === EventKind.APPROVAL_REQUESTED) {
    div.dataset.approval = String(e.approvalSeq);
    for (const allow of [true, false]) {
      const b = document.createElement("button");
      b.textContent = allow ? "Allow" : "Deny";
      b.onclick = async () => {
        try {
          await client.decideApproval({ sessionId, approvalSeq: e.approvalSeq, allow, by: "spike" });
        } catch (err) {
          console.error(err);
          alert(String(err));
        }
      };
      div.append(" ", b);
    }
  } else if (e.kind === EventKind.APPROVAL_DECIDED) {
    for (const b of $("events").querySelectorAll(`[data-approval="${e.approvalSeq}"] button`)) b.remove();
  }
  $("events").append(div);
  count++;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function watchLoop(signal: AbortSignal) {
  const id = sessionId;
  while (!signal.aborted) {
    set("status", "connected");
    try {
      for await (const r of client.watchEvents({ sessionId: id, epoch, afterSeq: cursor }, { signal })) {
        if (r.item.case === "event") {
          const e = r.item.value;
          if (e.seq <= cursor) {
            stats.dupes++;
          } else {
            if (e.seq !== cursor + 1n) stats.gaps++;
            append(e);
            cursor = e.seq;
            epoch = e.epoch;
          }
        } else if (r.item.case === "heartbeat") {
          stats.heartbeats++;
          set("lastHeartbeat", new Date().toLocaleTimeString());
        }
        render();
      }
    } catch (err) {
      if (signal.aborted) return;
      console.error(err);
      const ce = ConnectError.from(err);
      if (ce.code === Code.FailedPrecondition) {
        console.log("epoch mismatch", ce.findDetails(EpochMismatchSchema));
        stats.epochResets++;
        clearList();
        epoch = "";
        cursor = 0n;
        render();
        continue;
      }
      if (ce.code === Code.Unauthenticated) {
        set("status", "auth needed");
        await new Promise<void>((res) => (resumeAuth = res));
        resumeAuth = undefined;
        continue;
      }
    }
    if (signal.aborted) return;
    set("status", "reconnecting");
    stats.reconnects++;
    render();
    await sleep(1000);
  }
}

async function loadSessions() {
  try {
    const res = await client.listSessions({});
    const sel = $<HTMLSelectElement>("sessions");
    sel.textContent = "";
    for (const s of res.sessions) {
      const o = document.createElement("option");
      o.value = s.id;
      o.textContent = `${s.name} ${s.cwd} (${s.id})`;
      sel.append(o);
    }
    for (const w of res.warnings) console.error("list warning", w);
  } catch (err) {
    console.error(err);
    set("status", ConnectError.from(err).code === Code.Unauthenticated ? "auth needed" : "list failed");
  }
}

$("apply").onclick = () => {
  token = $<HTMLInputElement>("token").value.trim();
  resumeAuth?.();
  void loadSessions();
};

$("refresh").onclick = () => void loadSessions();

$("watch").onclick = () => {
  abort?.abort();
  resumeAuth?.();
  sessionId = $<HTMLSelectElement>("sessions").value;
  if (!sessionId) return;
  epoch = "";
  cursor = 0n;
  stats.gaps = stats.dupes = stats.reconnects = stats.epochResets = stats.heartbeats = 0;
  clearList();
  render();
  abort = new AbortController();
  void watchLoop(abort.signal);
};

$("submit").onclick = async () => {
  const text = $<HTMLInputElement>("text").value;
  try {
    const res = await client.submit({ sessionId, requestId: crypto.randomUUID(), text });
    set("submitResult", JSON.stringify(res.submission, (_, v) => (typeof v === "bigint" ? String(v) : v)));
  } catch (err) {
    console.error(err);
    set("submitResult", String(err));
  }
};

$<HTMLInputElement>("token").value = token;
render();
void loadSessions();
