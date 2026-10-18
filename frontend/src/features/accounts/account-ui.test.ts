import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { accountEgressScopes, authPollDelayMs, authSessionRemainingMs, connectMethods, orderQuotaWindows, resolveAccountUI } from "./account-ui.ts";

describe("account provider UI", () => {
  it("falls back to the default UI and lets a module override it", () => {
    const fallback = { label: "default", renewAll: false };
    const modules = { grok_build: { renewAll: true } };
    assert.deepEqual(resolveAccountUI("grok_build", modules, fallback), { label: "default", renewAll: true });
    assert.deepEqual(resolveAccountUI("future_ai", modules, fallback), fallback);
  });

  it("derives connect methods from catalog credential flags in menu order", () => {
    assert.deepEqual(connectMethods({ deviceOAuth: true, redirectOAuth: false, import: true }), ["deviceOAuth", "import"]);
    assert.deepEqual(connectMethods({ deviceOAuth: false, redirectOAuth: true, import: true }), ["redirectOAuth", "import"]);
    assert.deepEqual(connectMethods({ deviceOAuth: false, redirectOAuth: false, import: false }), []);
    assert.deepEqual(connectMethods(undefined), []);
  });

  it("binds egress to the provider scope unless the module widens it", () => {
    assert.deepEqual(accountEgressScopes("chatgpt", {}), ["chatgpt"]);
    assert.deepEqual(accountEgressScopes("grok_console", { egressScopes: ["grok_console", "grok_web"] }), ["grok_console", "grok_web"]);
  });

  it("orders quota windows by catalog modes", () => {
    const windows = [{ mode: "file_upload" }, { mode: "reason" }, { mode: "other" }];
    assert.deepEqual(orderQuotaWindows(["reason", "image_gen", "file_upload"], windows).map((window) => window.mode), ["reason", "file_upload"]);
    assert.deepEqual(orderQuotaWindows([], windows), windows);
  });
});

describe("account auth helpers", () => {
  it("polls at the server interval and backs off on slow_down", () => {
    assert.equal(authPollDelayMs(3), 3000);
    assert.equal(authPollDelayMs(3, true), 8000);
    assert.equal(authPollDelayMs(undefined), 5000);
    assert.equal(authPollDelayMs(0), 5000);
  });

  it("measures the remaining session time", () => {
    const now = Date.parse("2026-10-18T00:00:00Z");
    assert.equal(authSessionRemainingMs("2026-10-18T00:10:00Z", now), 600_000);
    assert.equal(authSessionRemainingMs("2026-10-17T23:00:00Z", now), 0);
    assert.equal(authSessionRemainingMs("not a time", now), undefined);
  });
});
