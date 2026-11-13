import { describe, expect, test } from "bun:test";
import { create } from "@bufbuild/protobuf";
import { SettingSchema } from "./gen/durable/v1/durable_pb";
import { groupOf, groupSettings, shortField, sourceLabel, sourceTone } from "./server";

const setting = (field: string, value = "v", source = "default") => create(SettingSchema, { field, value, source });

describe("server settings", () => {
  test("a field's group is the part before the first dot", () => {
    expect(groupOf("listen.addr")).toBe("listen");
    expect(groupOf("git.hosts.github.token")).toBe("git");
    expect(groupOf("environments")).toBe("environments");
  });

  test("groups keep the server's order", () => {
    const groups = groupSettings([
      setting("listen.addr"),
      setting("provider.name"),
      setting("listen.heartbeat"),
      setting("environments.go"),
    ]);
    expect(groups.map((g) => g.name)).toEqual(["listen", "provider", "environments"]);
    expect(groups[0]?.settings.map((s) => s.field)).toEqual(["listen.addr", "listen.heartbeat"]);
  });

  test("no settings means no groups", () => {
    expect(groupSettings([])).toEqual([]);
  });

  test("the short field drops the group", () => {
    expect(shortField("git.hosts.github.token")).toBe("hosts.github.token");
    expect(shortField("environments")).toBe("environments");
  });

  test("source labels and tones", () => {
    expect(sourceLabel("flag")).toBe("flag");
    expect(sourceLabel("")).toBe("unknown");
    expect(sourceTone("flag")).toBe("tone-link");
    expect(sourceTone("config.json")).toBe("tone-ok");
    expect(sourceTone("default")).toBe("tone-muted");
  });
});
