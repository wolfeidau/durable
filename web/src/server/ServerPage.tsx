import { useQuery } from "@connectrpc/connect-query";
import { type ReactElement } from "react";
import { getServerInfo } from "../gen/durable/v1/durable-SessionService_connectquery";
import { type EnvironmentInfo, type GrantInfo } from "../gen/durable/v1/durable_pb";
import { DataTable } from "../ui/DataTable";
import { approvalsLabel } from "../sessions/newSession";
import { QueryStatus } from "../ui/QueryStatus";
import { groupSettings, shortField, settingSourceLabel, sourceTone } from "./server";

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
    <DataTable head={["Name", "Image", "Grants", "Approvals", "Source"]} wrapClass="static" numeric={false}>
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
    </DataTable>
  );
}

function SourceBadge({ source }: { source: string }): ReactElement {
  return <span className={`badge ${sourceTone(source)}`}>{settingSourceLabel(source)}</span>;
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
      <QueryStatus isPending={isPending} error={error} warnings={data?.warnings} />
      {groups.map((g) => (
        <section key={g.name}>
          <h3>{g.name}</h3>
          <DataTable head={["Setting", "Value", "Source"]} wrapClass="static" numeric={false}>
                {g.settings.map((s) => (
                  <tr key={s.field}>
                    <td className="mono">{shortField(s.field)}</td>
                    <td className="mono">{s.value}</td>
                    <td>
                      <SourceBadge source={s.source} />
                    </td>
                  </tr>
                ))}
          </DataTable>
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
          <DataTable head={["Name", "Allows", "Binaries", "Hosts"]} wrapClass="static" numeric={false}>
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
          </DataTable>
        </section>
      )}
    </div>
  );
}
