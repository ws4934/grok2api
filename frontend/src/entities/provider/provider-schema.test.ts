import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { providerListSchema } from "./provider-schema.ts";

const entry = {
  id: "chatgpt", name: "ChatGPT", shortName: "ChatGPT", order: 3, capabilities: ["responses", "chat", "image"],
  conversation: { responses: true, chatCompletions: true, messages: true },
  media: { imageGeneration: true, imageEdit: true, videoGeneration: false, tts: false, stt: false, realtime: false },
  quota: "remote_window", usage: "estimated", tierScoped: false, egressScopes: ["chatgpt"],
  credential: { authType: "oauth", import: true, refresh: true, deviceOAuth: false, redirectOAuth: true },
  quotaModes: ["reason", "image_gen", "file_upload"], defaultMaxConcurrent: 1,
};

describe("provider catalog schema", () => {
  it("decodes the P2 catalog fields", () => {
    const [provider] = providerListSchema.parse([entry]);
    assert.equal(provider.credential.redirectOAuth, true);
    assert.deepEqual(provider.quotaModes, ["reason", "image_gen", "file_upload"]);
    assert.equal(provider.defaultMaxConcurrent, 1);
  });

  it("defaults fields an older backend omits and accepts any auth type", () => {
    const older: Record<string, unknown> = { ...entry, credential: { authType: "session", import: true, refresh: false, deviceOAuth: false } };
    delete older.quotaModes;
    delete older.defaultMaxConcurrent;
    const [provider] = providerListSchema.parse([older]);
    assert.equal(provider.credential.redirectOAuth, false);
    assert.equal(provider.credential.authType, "session");
    assert.deepEqual(provider.quotaModes, []);
    assert.equal(provider.defaultMaxConcurrent, 8);
  });
});
