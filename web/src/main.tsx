import { Code, ConnectError, createClient, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { useCallback, useEffect, useState, type ReactElement } from "react";
import { createRoot } from "react-dom/client";
import { ApprovalStatus, EventKind, Role, SessionService, type Event, type SessionInfo } from "./gen/durable/v1/durable_pb";
import { getToken, setToken } from "./token";
import { useWatchEvents } from "./useWatchEvents";

const bearer: Interceptor = (next) => (req) => {
  req.header.set("Authorization", `Bearer ${getToken()}`);
  return next(req);
};
const transport = createConnectTransport({ baseUrl: location.origin, interceptors: [bearer] });
const client = createClient(SessionService, transport);

const errText = (err: unknown) => ConnectError.from(err).message;

function describe(e: Event): string {
  const kind = EventKind[e.kind] ?? String(e.kind);
  let s = `${e.seq} ${kind}`;
  if (e.message) {
    s += ` ${Role[e.message.role] ?? ""}: ${e.message.text}`;
    if (e.message.toolCalls.length) s += ` [calls: ${e.message.toolCalls.map((c) => c.name).join(", ")}]`;
  }
  if (e.call) s += ` call=${e.call.name}`;
  if (e.isError) s += " (error)";
  if (e.error) s += ` error=${e.error}`;
  if (e.kind === EventKind.APPROVAL_REQUESTED || e.kind === EventKind.APPROVAL_DECIDED) {
    s += ` approval=${e.approvalSeq}`;
    if (e.kind === EventKind.APPROVAL_DECIDED) s += ` ${ApprovalStatus[e.approval] ?? ""}`;
  }
  return s;
}

function TokenInput({ onApply }: { onApply: (t: string) => void }): ReactElement {
  const [value, setValue] = useState("");
  return (
    <div>
      token needed:{" "}
      <input type="password" size={50} value={value} onChange={(e) => setValue(e.target.value)} />{" "}
      <button onClick={() => onApply(value.trim())}>Apply</button>
    </div>
  );
}

function Session({ sessionId, needsAuth, onToken }: { sessionId: string; needsAuth: boolean; onToken: (t: string) => void }): ReactElement {
  const w = useWatchEvents(transport, sessionId, getToken);
  const [text, setText] = useState("");
  const [note, setNote] = useState("");

  const applyToken = (t: string) => {
    onToken(t);
    w.resumeAuth();
  };

  const submit = async () => {
    try {
      await client.submit({ sessionId, requestId: crypto.randomUUID(), text });
      setText("");
      setNote("");
    } catch (err) {
      setNote(errText(err));
    }
  };

  const decide = async (approvalSeq: bigint, allow: boolean) => {
    try {
      await client.decideApproval({ sessionId, approvalSeq, allow, by: "web" });
    } catch (err) {
      setNote(errText(err));
    }
  };

  const decided = new Set(w.events.filter((e) => e.kind === EventKind.APPROVAL_DECIDED).map((e) => e.approvalSeq));
  const heartbeat = w.lastHeartbeat === undefined ? "none" : new Date(w.lastHeartbeat).toLocaleTimeString();

  return (
    <div>
      <div className="status">
        <span>status {w.status}</span>
        <span>epoch {w.epoch}</span>
        <span>cursor {String(w.cursor)}</span>
        <span>gaps {w.gaps}</span>
        <span>reconnects {w.reconnects}</span>
        <span>heartbeat {heartbeat}</span>
      </div>
      {(w.status === "auth-needed" || needsAuth) && <TokenInput onApply={applyToken} />}
      <div>
        {w.events.map((e) => (
          <div className="ev" key={String(e.seq)}>
            {describe(e)}
            {e.kind === EventKind.APPROVAL_REQUESTED && !decided.has(e.approvalSeq) && (
              <>
                {" "}
                <button onClick={() => void decide(e.approvalSeq, true)}>Allow</button>{" "}
                <button onClick={() => void decide(e.approvalSeq, false)}>Deny</button>
              </>
            )}
          </div>
        ))}
        {[...w.partialText].map(([submission, partial]) => (
          <div className="live" key={`p${submission}`}>
            {`reply ${submission}: ${partial}`}
          </div>
        ))}
        {[...w.toolOutput].map(([callId, out]) => (
          <div className="live" key={`t${callId}`}>
            {`output ${callId}: ${out}`}
          </div>
        ))}
      </div>
      <div>
        <input size={60} placeholder="prompt" value={text} onChange={(e) => setText(e.target.value)} />{" "}
        <button onClick={() => void submit()}>Submit</button> {note}
      </div>
    </div>
  );
}

function App(): ReactElement {
  const [sessions, setSessions] = useState<SessionInfo[]>([]);
  const [sessionId, setSessionId] = useState("");
  const [listError, setListError] = useState("");
  const [needsAuth, setNeedsAuth] = useState(false);

  const load = useCallback(async () => {
    try {
      const res = await client.listSessions({});
      setSessions(res.sessions);
      setListError("");
      setNeedsAuth(false);
    } catch (err) {
      setNeedsAuth(ConnectError.from(err).code === Code.Unauthenticated);
      setListError(errText(err));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const onToken = (t: string) => {
    setToken(t);
    void load();
  };

  return (
    <div>
      <h3>durable</h3>
      <div>
        session{" "}
        <select value={sessionId} onChange={(e) => setSessionId(e.target.value)}>
          <option value="">(none)</option>
          {sessions.map((s) => (
            <option key={s.id} value={s.id}>
              {`${s.name} ${s.cwd} (${s.id})`}
            </option>
          ))}
        </select>{" "}
        <button onClick={() => void load()}>Refresh</button> {listError}
      </div>
      {!sessionId && needsAuth && <TokenInput onApply={onToken} />}
      {sessionId && <Session sessionId={sessionId} needsAuth={needsAuth} onToken={onToken} />}
    </div>
  );
}

const root = document.getElementById("root");
if (root) createRoot(root).render(<App />);
