import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { updateResultSchema, versionInfoSchema, waitForVersion, type Clock } from "./version-model.ts";

const snapshot = {
  currentVersion: "v1.2.0",
  latestVersion: "v1.3.0",
  updateAvailable: true,
  status: "update_available",
  checkedAt: null,
  releaseUrl: "https://github.com/ws4934/many2api/releases/tag/v1.3.0",
  releaseNotes: "",
  error: "",
  canUpdate: false,
  updateBlockedReason: "当前以容器方式运行，请拉取新镜像更新",
  canRestart: true,
  rollbackAvailable: true,
};

describe("version info", () => {
  it("decodes the update capability fields", () => {
    assert.deepEqual(versionInfoSchema.parse(snapshot), snapshot);
  });

  it("rejects a snapshot missing the update capability fields", () => {
    const legacy: Record<string, unknown> = { ...snapshot };
    delete legacy.canUpdate;
    assert.throws(() => versionInfoSchema.parse(legacy));
    assert.throws(() => versionInfoSchema.parse({ ...snapshot, rollbackAvailable: "yes" }));
  });

  it("decodes the apply/rollback result", () => {
    assert.deepEqual(updateResultSchema.parse({ version: "v1.3.0", restarting: true }), { version: "v1.3.0", restarting: true });
    assert.throws(() => updateResultSchema.parse({ version: "v1.3.0" }));
  });
});

function fakeClock(): Clock & { elapsed: () => number } {
  let now = 0;
  return { now: () => now, sleep: async (ms) => { now += ms; }, elapsed: () => now };
}

describe("waitForVersion", () => {
  it("returns once the server reports the target version", async () => {
    const clock = fakeClock();
    const versions = ["v1.2.0", "v1.2.0", "v1.3.0"];
    assert.equal(await waitForVersion("v1.3.0", async () => versions.shift() ?? "", clock), true);
    assert.equal(clock.elapsed(), 6_000);
  });

  it("keeps polling through errors while the server restarts", async () => {
    const clock = fakeClock();
    let calls = 0;
    const fetchVersion = async () => {
      calls += 1;
      if (calls < 4) throw new TypeError("Failed to fetch");
      return "v1.3.0";
    };
    assert.equal(await waitForVersion("v1.3.0", fetchVersion, clock), true);
    assert.equal(calls, 4);
  });

  it("gives up after the deadline", async () => {
    const clock = fakeClock();
    let calls = 0;
    const fetchVersion = async () => {
      calls += 1;
      throw new Error("HTTP 502");
    };
    assert.equal(await waitForVersion("v1.3.0", fetchVersion, clock, 2_000, 10_000), false);
    assert.equal(calls, 5);
    assert.equal(clock.elapsed(), 10_000);
  });
});
