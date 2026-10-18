import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  extensionEgressScopes,
  isGrokProvider,
  orderedProviderIds,
  providerColor,
  providerLabel,
  providerRank,
  providerShortLabel,
} from "./provider-presentation.ts";

const catalog = [
  { id: "grok_build", name: "Grok Build", shortName: "Build", egressScopes: ["grok_build"] },
  { id: "grok_web", name: "Grok Web", shortName: "Web", egressScopes: ["grok_web", "grok_web_asset"] },
  { id: "grok_console", name: "Grok Console", shortName: "Console", egressScopes: ["grok_console", "grok_console_asset"] },
  { id: "chatgpt", name: "ChatGPT", shortName: "ChatGPT", egressScopes: ["chatgpt"] },
];

describe("provider labels", () => {
  it("prefers catalog names", () => {
    assert.equal(providerLabel("chatgpt", catalog), "ChatGPT");
    assert.equal(providerShortLabel("grok_console", catalog), "Console");
  });

  it("keeps Grok names before the catalog loads", () => {
    assert.equal(providerLabel("grok_web"), "Grok Web");
    assert.equal(providerShortLabel("grok_build", []), "Build");
  });

  it("renders unknown ids raw", () => {
    assert.equal(providerLabel("future_ai", catalog), "future_ai");
    assert.equal(providerShortLabel("future_ai"), "future_ai");
    assert.equal(providerLabel("toString", catalog), "toString");
  });
});

describe("providerColor", () => {
  it("keeps the Grok palette with or without a catalog", () => {
    assert.equal(providerColor("grok_build"), "var(--quota-product-1)");
    assert.equal(providerColor("grok_web", catalog), "var(--quota-product-2)");
    assert.equal(providerColor("grok_console", catalog), "var(--quota-product-4)");
  });

  it("assigns spare entries to other providers by catalog order and stays stable when providers are appended", () => {
    const extended = [...catalog, { id: "a" }, { id: "b" }, { id: "c" }];
    assert.equal(providerColor("chatgpt", catalog), "var(--quota-product-3)");
    assert.equal(providerColor("chatgpt", extended), "var(--quota-product-3)");
    assert.equal(providerColor("a", extended), "var(--quota-product-5)");
    assert.equal(providerColor("b", extended), "var(--quota-product-6)");
    assert.equal(providerColor("c", extended), "var(--quota-product-3)");
  });

  it("uses the neutral entry for ids outside the catalog", () => {
    assert.equal(providerColor("chatgpt"), "var(--quota-product-0)");
    assert.equal(providerColor("future_ai", catalog), "var(--quota-product-0)");
  });
});

describe("provider ordering", () => {
  it("lists catalog providers first and appends unknown ids once", () => {
    assert.deepEqual(orderedProviderIds(catalog, ["future_ai", "grok_web", "future_ai"]), ["grok_build", "grok_web", "grok_console", "chatgpt", "future_ai"]);
  });

  it("falls back to the Grok order without a catalog", () => {
    assert.deepEqual(orderedProviderIds(undefined, ["chatgpt"]), ["grok_build", "grok_web", "grok_console", "chatgpt"]);
    assert.equal(providerRank("grok_console"), 2);
    assert.equal(providerRank("chatgpt", catalog), 3);
    assert.equal(providerRank("future_ai", catalog), 4);
  });
});

describe("provider scopes", () => {
  it("identifies only the built-in Grok providers", () => {
    assert.equal(isGrokProvider("grok_console"), true);
    assert.equal(isGrokProvider("grok_web_asset"), false);
    assert.equal(isGrokProvider("chatgpt"), false);
  });

  it("collects egress scopes declared by non-Grok providers", () => {
    assert.deepEqual(extensionEgressScopes(catalog), ["chatgpt"]);
    assert.deepEqual(extensionEgressScopes([...catalog, { id: "x", egressScopes: ["x", "x_asset", "chatgpt"] }]), ["chatgpt", "x", "x_asset"]);
    assert.deepEqual(extensionEgressScopes(undefined), []);
  });
});
