import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { apiErrorFromResponse, describeError, getImageJob, imageGenerationBody, imageJobState } from "./creative-console-api.ts";

describe("image generation body", () => {
  it("sends size and quality on basic routes, defaulting both to auto", () => {
    assert.deepEqual(
      imageGenerationBody({ basicProtocol: true, model: "gpt-image-2.5", prompt: "cat", params: { count: 2, size: "1536x1024", quality: "high", aspectRatio: "16:9" } }),
      { model: "gpt-image-2.5", prompt: "cat", n: 2, size: "1536x1024", quality: "high", output_format: "png", response_format: "url" },
    );
    assert.deepEqual(
      imageGenerationBody({ basicProtocol: true, model: "gpt-image-2.5", prompt: "cat", params: { count: 1 } }),
      { model: "gpt-image-2.5", prompt: "cat", n: 1, size: "auto", quality: "auto", output_format: "png", response_format: "url" },
    );
  });

  it("sends aspect ratio and resolution on Grok routes and quality only when set", () => {
    assert.deepEqual(
      imageGenerationBody({ basicProtocol: false, model: "grok-imagine-image-2.0", prompt: "cat", params: { count: 1, aspectRatio: "16:9", resolution: "2k", quality: "low" } }),
      { model: "grok-imagine-image-2.0", prompt: "cat", n: 1, aspect_ratio: "16:9", resolution: "2k", quality: "low", stream: false, response_format: "url" },
    );
    const body = imageGenerationBody({ basicProtocol: false, model: "grok-imagine-image", prompt: "cat", params: { count: 4, aspectRatio: "1:1", resolution: "1k", size: "1024x1024" } });
    assert.equal("quality" in body, false);
    assert.equal("size" in body, false);
  });
});

describe("api errors", () => {
  it("reads the error body and the gateway X-Request-ID header", () => {
    const response = new Response(null, { status: 502, headers: { "X-Request-ID": "req_abc" } });
    const error = apiErrorFromResponse(response, JSON.stringify({ error: { message: "上游服务暂不可用", type: "server_error", code: "upstream_unavailable", param: null } }));
    assert.equal(error.status, 502);
    assert.deepEqual(describeError(error), { message: "上游服务暂不可用", code: "upstream_unavailable", requestId: "req_abc" });
  });

  it("falls back to the raw body, then the status text", () => {
    const plain = apiErrorFromResponse(new Response(null, { status: 504, statusText: "Gateway Timeout" }), "upstream timed out\n");
    assert.deepEqual(describeError(plain), { message: "upstream timed out", code: undefined, requestId: undefined });
    assert.equal(apiErrorFromResponse(new Response(null, { status: 504, statusText: "Gateway Timeout" }), "").message, "Gateway Timeout");
  });

  it("describes non-API failures by message only", () => {
    assert.deepEqual(describeError(new Error("Failed to fetch")), { message: "Failed to fetch" });
    assert.deepEqual(describeError("boom"), { message: "boom" });
  });
});

describe("image job state", () => {
  it("maps Responses statuses to settled outcomes", () => {
    assert.deepEqual(imageJobState({ status: "queued" }, "rec_1"), { done: false, succeeded: false });
    assert.deepEqual(imageJobState({ status: "in_progress" }, "rec_1"), { done: false, succeeded: false });
    assert.deepEqual(imageJobState({ status: "completed" }, "rec_1"), { done: true, succeeded: true });
    assert.deepEqual(imageJobState({ id: "resp_img_1", status: "failed", error: { code: "result_uncertain", message: "maybe drawn" } }, "rec_1"), { done: true, succeeded: false, error: { message: "maybe drawn", code: "result_uncertain", requestId: "rec_1" } });
    assert.deepEqual(imageJobState({ status: "cancelled" }, "rec_1-1"), { done: true, succeeded: false, error: { message: "cancelled", code: undefined, requestId: "rec_1-1" } });
  });

  it("reports the submit request ID, not the job ID, for a vanished job", async (t) => {
    t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ error: { code: "not_found", message: "gone" } }), { status: 404 }));
    assert.deepEqual(await getImageJob({ apiKey: "k", id: "resp_img_1", requestId: "rec_1-2" }), { done: true, succeeded: false, error: { message: "gone", code: "not_found", requestId: "rec_1-2" } });
  });
});
