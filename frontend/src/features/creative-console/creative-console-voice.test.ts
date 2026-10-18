import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { resolveVoice } from "./creative-console-voice.ts";

const grokVoices = [{ voiceId: "ara", name: "Ara" }, { voiceId: "eve", name: "Eve" }];
const chatgptVoices = [{ voiceId: "glimmer", name: "Sol", language: "American English" }];

describe("resolveVoice", () => {
  it("keeps a choice the model offers", () => {
    assert.equal(resolveVoice(grokVoices, "eve", true).voiceId, "eve");
  });

  it("replaces a voice from another model with the list's first voice", () => {
    assert.equal(resolveVoice(grokVoices, "glimmer", true).voiceId, "ara");
    assert.equal(resolveVoice(chatgptVoices, "eve", false).voiceId, "glimmer");
  });

  it("falls back to eve only for Grok while the list is empty", () => {
    assert.deepEqual(resolveVoice([], "glimmer", true), { options: [{ voiceId: "eve", name: "eve" }], voiceId: "eve" });
    assert.deepEqual(resolveVoice([], "eve", false), { options: [], voiceId: "" });
  });
});
