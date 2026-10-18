import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { availableVariants, createExamples, resolveExample, type EndpointDefinition } from "./api-docs-examples.ts";

const definition: EndpointDefinition = {
  key: "voice/stt", category: "Voice", title: "Speech to text", method: "POST", path: "/stt",
  descriptionKey: "docs.endpointSTT", capabilities: ["stt"], fields: [], noteKeys: ["grok"],
  request: (model) => ({ model, url: "https://example.com/sample.wav" }),
  response: { text: "grok" },
  variants: { tools: { request: (model) => ({ model, tools: [] }), response: { variant: "tools" } } },
  chatgpt: { request: (model) => ({ model, file: "sample.wav" }), response: { text: "chatgpt" }, multipart: true, noteKeys: ["chatgpt"] },
};

describe("api docs examples", () => {
  it("uses the chatgpt example and notes only for chatgpt models", () => {
    for (const provider of [undefined, "grok_console"]) {
      const { example, noteKeys } = resolveExample(definition, "basic", provider);
      assert.deepEqual(example.request("m"), { model: "m", url: "https://example.com/sample.wav" });
      assert.equal(example.multipart, undefined);
      assert.deepEqual(noteKeys, ["grok"]);
    }
    const { example, noteKeys } = resolveExample(definition, "basic", "chatgpt");
    assert.deepEqual(example.request("m"), { model: "m", file: "sample.wav" });
    assert.deepEqual(example.response, { text: "chatgpt" });
    assert.deepEqual(noteKeys, ["chatgpt"]);
  });

  it("keeps variants provider-neutral and falls back to the basic example", () => {
    assert.deepEqual(availableVariants(definition), ["basic", "tools"]);
    assert.deepEqual(resolveExample(definition, "tools", "chatgpt").example.response, { variant: "tools" });
    assert.deepEqual(resolveExample(definition, "search", "grok_web").example.response, { text: "grok" });
  });

  it("renders multipart uploads without a JSON content type", () => {
    const { example } = resolveExample(definition, "basic", "chatgpt");
    const code = createExamples(definition, example, "https://api.test/v1", "chatgpt-transcribe");
    assert.ok(code.curl.endsWith('-F "file=@sample.wav" \\\n  -F "model=chatgpt-transcribe"'));
    assert.ok(!code.curl.includes("Content-Type"));
    assert.ok(code.python.includes('data={"model":"chatgpt-transcribe"}'));
    assert.ok(code.javascript.includes('form.append("model", "chatgpt-transcribe");'));
  });

  it("fills the response id path parameter", () => {
    const code = createExamples({ key: "image/responses-cancel", method: "POST", path: "/responses/{response_id}/cancel" }, { request: () => undefined, response: {} }, "https://api.test/v1", "gpt-image-2");
    assert.ok(code.curl.includes('"https://api.test/v1/responses/resp_img_example/cancel"'));
  });
});
