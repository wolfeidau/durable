import { type Setting } from "./gen/durable/v1/durable_pb";

export type SettingGroup = { name: string; settings: Setting[] };

// A field's group is the part before its first dot, so listen.addr falls under "listen".
export const groupOf = (field: string): string => {
  const dot = field.indexOf(".");
  return dot < 0 ? field : field.slice(0, dot);
};

// Groups keep the order the server sent them in, which is the display order.
export function groupSettings(settings: Setting[]): SettingGroup[] {
  const groups = new Map<string, Setting[]>();
  for (const s of settings) {
    const name = groupOf(s.field);
    groups.set(name, [...(groups.get(name) ?? []), s]);
  }
  return [...groups].map(([name, list]) => ({ name, settings: list }));
}

// The field name inside its group, so the table doesn't repeat the group on every row.
export const shortField = (field: string): string => {
  const dot = field.indexOf(".");
  return dot < 0 ? field : field.slice(dot + 1);
};

export const sourceLabel = (source: string): string => source || "unknown";

export const sourceTone = (source: string): string => {
  switch (source) {
    case "flag":
      return "tone-link";
    case "config.json":
      return "tone-ok";
    case "environments.json":
      return "tone-warn";
    default:
      return "tone-muted";
  }
};
