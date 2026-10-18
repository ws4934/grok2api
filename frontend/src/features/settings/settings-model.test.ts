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
