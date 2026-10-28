import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { imageGenerationParams, imageGenerationPolicy, imageReferencePolicy, parseImageReferenceURLs } from "./creative-console-image.ts";

describe("creative image controls and effective request parameters", () => {
  const web20 = { provider: "grok_web", upstreamModel: "grok-imagine-image-2.0" };
  const console20 = { provider: "grok_console", upstreamModel: "grok-imagine-image-2.0" };
  const requested = { count: 2, aspectRatio: "16:9", resolution: "2k", quality: "medium" };

  it("does not advertise or resend Web resolution and quality", () => {
    const policy = imageGenerationPolicy([web20]);
    assert.equal(policy.basicProtocol, false);
    assert.ok(policy.aspectRatios.includes("21:9"));
    assert.ok(policy.aspectRatios.includes("5:2"));
    assert.ok(!policy.aspectRatios.includes("19.5:9"));
    assert.ok(policy.aspectRatios.includes("auto"));
    assert.deepEqual(policy.resolutions, []);
    assert.deepEqual(policy.qualities, []);
    assert.deepEqual(imageGenerationParams(policy, requested), { count: 2, aspectRatio: "16:9" });
  });

  it("keeps Console 2.0 quality and resolution, based on the actual target", () => {
    const policy = imageGenerationPolicy([console20]);
    assert.deepEqual(policy.resolutions, ["1k", "2k"]);
    assert.deepEqual(policy.qualities, ["auto", "low", "medium"]);
    assert.deepEqual(imageGenerationParams(policy, requested), requested);
    assert.deepEqual(imageGenerationParams(policy, { count: 1 }), { count: 1, aspectRatio: "1:1", resolution: "1k", quality: "auto" });
  });

  it("does not offer quality on other Console models", () => {
    const policy = imageGenerationPolicy([{ ...console20, upstreamModel: "grok-imagine-image-quality" }]);
    assert.deepEqual(policy.qualities, []);
    assert.deepEqual(imageGenerationParams(policy, requested), { count: 2, aspectRatio: "16:9", resolution: "2k" });
  });

  it("keeps lite generation distinct from regular Web Imagine", () => {
    const lite = imageGenerationPolicy([{ provider: "grok_web", upstreamModel: "grok-imagine-image" }]);
    assert.deepEqual(lite.aspectRatios, []);
    assert.deepEqual(imageGenerationParams(lite, requested), { count: 2 });
    assert.ok(imageGenerationPolicy([{ ...web20, upstreamModel: "grok-imagine-image-quality" }]).aspectRatios.includes("2:1"));
  });

  it("uses the common controls of every route sharing a public ID", () => {
    const policy = imageGenerationPolicy([console20, web20]);
    assert.deepEqual(policy, imageGenerationPolicy([web20, console20]));
    assert.deepEqual(policy.resolutions, []);
    assert.deepEqual(policy.qualities, []);
    assert.ok(!policy.aspectRatios.includes("19.5:9"));
    assert.deepEqual(imageGenerationParams(policy, requested), { count: 2, aspectRatio: "16:9" });
  });

  it("retains the basic API size/quality contract", () => {
    const policy = imageGenerationPolicy([{ provider: "chatgpt", upstreamModel: "gpt-image-2.5" }]);
    assert.deepEqual(imageGenerationParams(policy, { ...requested, size: "1536x1024", quality: "high" }), { count: 2, size: "1536x1024", quality: "high" });
    assert.deepEqual(imageGenerationParams(policy, { count: 1 }), { count: 1, size: "auto", quality: "auto" });
  });

  it("does not promise pixel size or quality across mixed Grok/basic targets", () => {
    const basic = { provider: "chatgpt", upstreamModel: "gpt-image-2.5" };
    const policy = imageGenerationPolicy([basic, web20]);
    assert.deepEqual(policy, imageGenerationPolicy([web20, basic]));
    assert.deepEqual(imageGenerationParams(policy, { ...requested, size: "1536x1024" }), { count: 2 });
  });

  it("shows no unsupported Grok controls while routes are loading", () => {
    assert.deepEqual(imageGenerationPolicy([]), { basicProtocol: false, aspectRatios: [], resolutions: [], qualities: [] });
  });
});

describe("unified image references", () => {
  it("uses the 2.0 five-image limit and preserves other edit entries", () => {
    const web20 = { provider: "grok_web", upstreamModel: "grok-imagine-image-2.0" };
    assert.deepEqual(imageReferencePolicy([web20]), { limit: 5 });
    assert.deepEqual(imageReferencePolicy([{ ...web20, upstreamModel: "imagine-image-edit" }]), { limit: 8 });
    assert.deepEqual(imageReferencePolicy([web20, { provider: "grok_console", upstreamModel: "grok-imagine-image-2.0" }]), { limit: 3 });
    assert.equal(imageReferencePolicy([]).limit, 0);
  });

  it("accepts ordered HTTPS references without silently dropping invalid input", () => {
    assert.deepEqual(parseImageReferenceURLs(" https://example.com/one.png\n\nhttps://example.com/two.png "), { urls: ["https://example.com/one.png", "https://example.com/two.png"], invalid: false });
    for (const url of ["http://example.com/a.png", "https://", "file:///a.png", "data:image/png;base64,AAAA", "https://user:pass@example.com/a.png"]) {
      assert.equal(parseImageReferenceURLs(url).invalid, true);
    }
    assert.deepEqual(parseImageReferenceURLs(""), { urls: [], invalid: false });
  });
});
