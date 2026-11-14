import { ConnectError } from "@connectrpc/connect";
import { useQuery } from "@connectrpc/connect-query";
import { type ReactElement } from "react";
import { getServerInfo } from "./gen/durable/v1/durable-SessionService_connectquery";
import { type EnvironmentInfo, type GrantInfo } from "./gen/durable/v1/durable_pb";
import { approvalsLabel } from "./newSession";
import { groupSettings, shortField, sourceLabel, sourceTone } from "./server";

function Environments({
  environments,
  grants,
  sources,
}: {
  environments: EnvironmentInfo[];
  grants: GrantInfo[];
  sources: Map<string, string>;
}): ReactElement {
  const summary = new Map(grants.map((g) => [g.name, g.summary]));
  return (
    <div className="table-wrap static">
      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Image</th>
            <th>Grants</th>
            <th>Approvals</th>
            <th>Source</th>
          </tr>
        </thead>
        <tbody>
          {environments.map((e) => (
            <tr key={e.name}>
              <td className="session-name">{e.name}</td>
              <td className="mono">{e.image}</td>
              <td>
                {e.grants.length === 0 && <span className="faint">none</span>}
                {e.grants.map((g) => (
                  <span key={g} className="badge tone-muted" title={summary.get(g)}>
                    {g}
                  </span>
                ))}
              </td>
              <td>{approvalsLabel(e)}</td>
              <td>
                <SourceBadge source={sources.get(`environments.${e.name}`) ?? ""} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function SourceBadge({ source }: { source: string }): ReactElement {
  return <span className={`badge ${sourceTone(source)}`}>{sourceLabel(source)}</span>;
}

export function ServerPage(): ReactElement {
  const { data, error, isPending } = useQuery(getServerInfo, {});
  const settings = data?.settings ?? [];
  // Environments get their own table, with the source taken from their setting.
  const groups = groupSettings(settings).filter((g) => g.name !== "environments");
  const sources = new Map(settings.map((s) => [s.field, s.source]));

  return (
    <div className="server-page">
      <div className="section-head">
        <h2>Server</h2>
      </div>
      {isPending && !error && <p className="muted">Loading</p>}
      {error && <p className="banner tone-error">{ConnectError.from(error).rawMessage}</p>}
      {data?.warnings.map((w) => (
        <p className="banner tone-warn" key={w}>
          {w}
        </p>
      ))}
      {groups.map((g) => (
        <section key={g.name}>
          <h3>{g.name}</h3>
          <div className="table-wrap static">
            <table>
              <thead>
                <tr>
                  <th>Setting</th>
                  <th>Value</th>
                  <th>Source</th>
                </tr>
              </thead>
              <tbody>
                {g.settings.map((s) => (
                  <tr key={s.field}>
                    <td className="mono">{shortField(s.field)}</td>
                    <td className="mono">{s.value}</td>
                    <td>
                      <SourceBadge source={s.source} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      ))}
      {data && (
        <section>
          <h3>Environments</h3>
          {data.environments.length === 0 ? (
            <p className="card empty">No sandbox environments. Sessions run on the host.</p>
          ) : (
            <Environments environments={data.environments} grants={data.grants} sources={sources} />
          )}
        </section>
      )}
      {data && data.grants.length > 0 && (
        <section>
          <h3>Grants</h3>
          <div className="table-wrap static">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Allows</th>
                  <th>Binaries</th>
                  <th>Hosts</th>
                </tr>
              </thead>
              <tbody>
                {data.grants.map((g) => (
                  <tr key={g.name}>
                    <td className="session-name">{g.name}</td>
                    <td className="muted">{g.summary}</td>
                    <td className="mono">
                      {g.binaries.map((b) => (
                        <div key={b}>{b}</div>
                      ))}
                    </td>
                    <td className="mono">
                      {g.hosts.map((h) => (
                        <div key={h}>{h}</div>
                      ))}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </div>
  );
}
