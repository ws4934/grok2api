import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { parseVideoReferenceURL, snapshotVideoFrame, videoFrameIssue, videoInputPolicy } from "./creative-console-video.ts";

describe("video reference input policy", () => {
  const console15 = { provider: "grok_console", upstreamModel: "Console/grok-imagine-video-1.5" };
  const classic = { provider: "grok_console", upstreamModel: "Console/grok-imagine-video" };
  const build15 = { provider: "grok_build", upstreamModel: "grok-imagine-video-1.5" };

  it("supports 14 references plus a pinned first frame on Console 1.5", () => {
    assert.deepEqual(videoInputPolicy([console15]), { maxReferenceImages: 14, canCombineFirstFrame: true, canReferenceVoices: true, canPinFrames: true, canGenerateAudio: true, referenceDurationLimit: 15 });
    assert.deepEqual(videoInputPolicy([{ ...console15, upstreamModel: "grok-imagine-video-1.5" }]), videoInputPolicy([console15]));
  });

  it("keeps the classic Console and Build contracts separate", () => {
    assert.deepEqual(videoInputPolicy([classic]), { maxReferenceImages: 7, canCombineFirstFrame: false, canReferenceVoices: false, canPinFrames: false, canGenerateAudio: true, referenceDurationLimit: 10 });
    assert.deepEqual(videoInputPolicy([build15]), { maxReferenceImages: 8, canCombineFirstFrame: false, canReferenceVoices: false, canPinFrames: false, canGenerateAudio: false, referenceDurationLimit: 15 });
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


describe("pinned video frames", () => {
  const image = { url: "https://example.com/frame.png" };
  const frame = (id: string, timestamp: string) => ({ ...image, id, timestamp });
  it("allows a standalone last frame or up to four distinct interior keyframes", () => {
    assert.equal(videoFrameIssue(image, [], 6, true), undefined);
    assert.equal(videoFrameIssue(undefined, [frame("1", "0.333"), frame("2", "2"), frame("3", "3"), frame("4", "5")], 6, true), undefined);
    assert.equal(videoFrameIssue(undefined, [], 6, false), undefined);
    assert.equal(videoFrameIssue(image, [], 6, false), "unsupported");
    assert.equal(videoFrameIssue(undefined, [frame("1", "1"), frame("2", "1.1")], 6, true), "duplicate");
    for (const timestamp of ["", "0", "6", "7", "NaN", "Infinity"]) assert.equal(videoFrameIssue(undefined, [frame("1", timestamp)], 6, true), "time");
    assert.equal(videoFrameIssue(undefined, Array.from({ length: 5 }, (_, i) => frame(String(i), String(i + 1))), 8, true), "limit");
  });
  it("keeps missing local-image placeholders invalid until re-uploaded", () => {
    assert.equal(videoFrameIssue({ name: "tail.png" }, [], 6, true), "image");
    assert.equal(videoFrameIssue({ file: new File(["png"], "tail.png") }, [], 6, true), undefined);
    assert.deepEqual(snapshotVideoFrame({ file: new File(["png"], "tail.png") }), { name: "tail.png" });
    assert.deepEqual(snapshotVideoFrame({ url: "data:image/png;base64,AAAA" }), {});
    assert.deepEqual(snapshotVideoFrame(image), image);
  });
});
