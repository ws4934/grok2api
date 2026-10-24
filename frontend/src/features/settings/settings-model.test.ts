import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { settingsSchema } from "./settings-model.ts";

const chatgpt = settingsSchema.shape.providerChatGPT;
const defaults = { imageConversationModel: "", outputTokenLimit: 16384, visionTokens: 256, defaultImageModel: "", cloudflareClearance: false };

describe("ChatGPT provider settings", () => {
  it("accepts the backend defaults and trims model names", () => {
    assert.deepEqual(chatgpt.parse(defaults), defaults);
    assert.equal(chatgpt.parse({ ...defaults, imageConversationModel: " gpt-5 " }).imageConversationModel, "gpt-5");
  });

  it("enforces the backend ranges", () => {
    for (const outputTokenLimit of [0, 131_073, 1.5]) assert.equal(chatgpt.safeParse({ ...defaults, outputTokenLimit }).success, false);
    for (const visionTokens of [-1, 100_001]) assert.equal(chatgpt.safeParse({ ...defaults, visionTokens }).success, false);
    assert.equal(chatgpt.safeParse({ ...defaults, outputTokenLimit: 131_072, visionTokens: 0 }).success, true);
  });
});

const adobe = settingsSchema.shape.providerAdobe;
const minute = { value: 1, unit: "m" } as const;

describe("Adobe provider settings", () => {
  it("accepts an empty, internal or public HTTPS mint URL", () => {
    for (const sherlockMintAPI of ["", "http://mint:8777", "http://127.0.0.1:8777", "https://mint.example.com"]) {
      assert.equal(adobe.safeParse({ sherlockMintAPI, timeout: minute }).success, true, sherlockMintAPI);
    }
  });

  it("matches the backend URL policy and timeout range", () => {
    for (const sherlockMintAPI of ["http://mint.example.com:8777", "http://mint:8777/?a=1", "mint:8777"]) {
      assert.equal(adobe.safeParse({ sherlockMintAPI, timeout: minute }).success, false, sherlockMintAPI);
    }
    for (const timeout of [{ value: 4, unit: "s" }, { value: 31, unit: "m" }] as const) {
      assert.equal(adobe.safeParse({ sherlockMintAPI: "", timeout }).success, false);
    }
  });
});

const muse = settingsSchema.shape.providerMuse;

describe("Muse provider settings", () => {
  it("accepts an empty, internal or public HTTPS browser URL", () => {
    for (const browserURL of ["", "http://muse-browser:9222", "http://127.0.0.1:9222", "https://browser.example.com"]) {
      assert.equal(muse.safeParse({ browserURL, maxPages: 2 }).success, true, browserURL);
    }
  });

  it("matches the backend URL policy and page range", () => {
    for (const browserURL of ["http://browser.example.com:9222", "muse-browser:9222"]) {
      assert.equal(muse.safeParse({ browserURL, maxPages: 2 }).success, false, browserURL);
    }
    for (const maxPages of [0, 17, 1.5]) assert.equal(muse.safeParse({ browserURL: "", maxPages }).success, false);
    assert.equal(muse.safeParse({ browserURL: "", maxPages: 16 }).success, true);
  });
});
