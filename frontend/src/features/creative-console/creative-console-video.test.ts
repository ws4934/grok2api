import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { parseVideoReferenceURL, videoInputPolicy } from "./creative-console-video.ts";

describe("video reference input policy", () => {
  const console15 = { provider: "grok_console", upstreamModel: "Console/grok-imagine-video-1.5" };
  const classic = { provider: "grok_console", upstreamModel: "Console/grok-imagine-video" };
  const build15 = { provider: "grok_build", upstreamModel: "grok-imagine-video-1.5" };

  it("supports 14 references plus a pinned first frame on Console 1.5", () => {
    assert.deepEqual(videoInputPolicy([console15]), { maxReferenceImages: 14, canCombineFirstFrame: true, canReferenceVoices: true, referenceDurationLimit: 15 });
    assert.deepEqual(videoInputPolicy([{ ...console15, upstreamModel: "grok-imagine-video-1.5" }]), videoInputPolicy([console15]));
  });

  it("keeps the classic Console and Build contracts separate", () => {
    assert.deepEqual(videoInputPolicy([classic]), { maxReferenceImages: 7, canCombineFirstFrame: false, canReferenceVoices: false, referenceDurationLimit: 10 });
    assert.deepEqual(videoInputPolicy([build15]), { maxReferenceImages: 8, canCombineFirstFrame: false, canReferenceVoices: false, referenceDurationLimit: 15 });
  });

  it("uses every actual target of an alias, not the public name or first route", () => {
    assert.deepEqual(videoInputPolicy([build15, classic, console15]), videoInputPolicy([console15]));
    assert.equal(videoInputPolicy([{ ...build15, upstreamModel: console15.upstreamModel }]).canCombineFirstFrame, false);
  });
});

describe("video reference URLs", () => {
  it("accepts remote images and inline images without accepting other protocols", () => {
    for (const url of ["https://example.com/image.png", "http://example.com/image.png", "data:image/png;base64,AAAA"]) {
      assert.equal(parseVideoReferenceURL(` ${url} `), url);
    }
    for (const value of ["", "https://", "not a URL", "javascript:alert(1)", "file:///tmp/image.png", "data:video/mp4;base64,AAAA"]) {
      assert.equal(parseVideoReferenceURL(value), undefined);
    }
  });
});
