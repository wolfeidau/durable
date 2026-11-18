import { describe, expect, test } from "bun:test";
import { create } from "@bufbuild/protobuf";
import { EnvironmentInfoSchema } from "../gen/durable/v1/durable_pb";
import { approvalsLabel, environmentHint, environmentLabel } from "./environments";

describe("environmentHint", () => {
  test("lists the image and grants", () => {
    const e = create(EnvironmentInfoSchema, { name: "go", image: "durable-worker-go:dev", grants: ["go-modules"] });
    expect(environmentHint(e)).toBe("durable-worker-go:dev · go-modules");
  });

  test("says so when there are no grants", () => {
    const e = create(EnvironmentInfoSchema, { name: "default", image: "durable-worker:dev" });
    expect(environmentHint(e)).toBe("durable-worker:dev · no network grants");
  });
});

describe("environment approvals labels", () => {
  test("auto is marked in the picker and the table", () => {
    const e = create(EnvironmentInfoSchema, { name: "go", image: "i", approvals: "auto" });
    expect(environmentLabel(e)).toBe("go · auto-approve");
    expect(approvalsLabel(e)).toBe("auto");
  });

  test("ask and unset read as ask with no marker", () => {
    for (const approvals of ["", "ask"]) {
      const e = create(EnvironmentInfoSchema, { name: "go", image: "i", approvals });
      expect(environmentLabel(e)).toBe("go");
      expect(approvalsLabel(e)).toBe("ask");
    }
  });
});
