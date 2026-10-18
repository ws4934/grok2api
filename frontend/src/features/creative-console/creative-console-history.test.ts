import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { historyStorageKey, loadImageHistory, loadVideoHistory, persistImageHistory, persistStoredList, type ImageHistoryRecord } from "./creative-console-history.ts";

function memoryStorage(quotaChars = Number.POSITIVE_INFINITY) {
  const values = new Map<string, string>();
  return {
    values,
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => {
      if (value.length > quotaChars) throw new Error("QuotaExceededError");
      values.set(key, value);
    },
    removeItem: (key: string) => {
      values.delete(key);
    },
  };
}

describe("history storage keys", () => {
  it("keeps the existing chat key and scopes every kind per client key", () => {
    assert.equal(historyStorageKey("chat", "key 1"), "grok2api:creative-console:chat-history:key%201");
    assert.equal(historyStorageKey("image", "7"), "grok2api:creative-console:image-history:7");
    assert.equal(historyStorageKey("video", "7"), "grok2api:creative-console:video-history:7");
  });
});

describe("image history", () => {
  it("drops malformed records and interrupts generations left running", () => {
    const storage = memoryStorage();
    storage.setItem(historyStorageKey("image", "k"), JSON.stringify([
      { id: "old", createdAt: 1, prompt: "dog", model: "grok-imagine-image", params: { count: 1, aspectRatio: "16:9", resolution: "2k" }, status: "failed", images: [], error: { message: "上游服务暂不可用", code: "upstream_unavailable", requestId: "req_1" } },
      { id: "new", createdAt: 2, prompt: "cat", model: "gpt-image-2.5", basicProtocol: true, params: { count: 2, size: "1024x1024", quality: "high" }, status: "generating", images: [] },
      { id: "zero", createdAt: 3, prompt: "x", model: "m", params: { count: 0 }, status: "completed", images: [] },
      { id: "status", createdAt: 3, prompt: "x", model: "m", params: { count: 1 }, status: "queued", images: [] },
      { id: "time", prompt: "x", model: "m", params: { count: 1 }, status: "completed", images: [] },
      "garbage",
    ]));
    const records = loadImageHistory("k", storage);
    assert.deepEqual(records.map((record) => [record.id, record.status, record.basicProtocol]), [["new", "interrupted", true], ["old", "failed", false]]);
    assert.equal(records[0]?.params.size, "1024x1024");
    assert.deepEqual(records[1]?.error, { message: "上游服务暂不可用", code: "upstream_unavailable", requestId: "req_1" });
  });

  it("reads corrupt or foreign storage as empty", () => {
    const storage = memoryStorage();
    storage.setItem(historyStorageKey("image", "k"), "{not json");
    assert.deepEqual(loadImageHistory("k", storage), []);
    storage.setItem(historyStorageKey("image", "k"), JSON.stringify({ id: "x" }));
    assert.deepEqual(loadImageHistory("k", storage), []);
  });

  it("stores image URLs but never inline base64 data", () => {
    const storage = memoryStorage();
    const record: ImageHistoryRecord = {
      id: "r", createdAt: 1, prompt: "cat", model: "m", basicProtocol: true, params: { count: 3 }, status: "completed",
      images: [{ url: "/v1/media/images/abc" }, { url: "data:image/png;base64,AAAA" }, { url: "blob:http://localhost/1" }],
    };
    persistImageHistory("k", [record], storage);
    assert.ok(!storage.values.get(historyStorageKey("image", "k"))?.includes("base64"));
    assert.deepEqual(loadImageHistory("k", storage)[0]?.images, [{ url: "/v1/media/images/abc", revisedPrompt: undefined }]);
    assert.equal(record.images.length, 3, "the in-memory record keeps every image");
  });
});

describe("video history", () => {
  it("keeps polling jobs that have a request ID and interrupts unconfirmed submissions", () => {
    const storage = memoryStorage();
    const base = { prompt: "waves", model: "grok-imagine-video", action: "generate", params: { duration: 6, aspectRatio: "16:9", resolution: "720p" } };
    storage.setItem(historyStorageKey("video", "k"), JSON.stringify([
      { ...base, id: "polling", createdAt: 5, status: "generating", requestId: "vid_1" },
      { ...base, id: "lost", createdAt: 4, status: "generating" },
      { ...base, id: "submitting", createdAt: 3, status: "submitting" },
      { ...base, id: "done", createdAt: 2, status: "completed", requestId: "vid_2", video: { url: "/v1/media/videos/vid_2.mp4", duration: 6 } },
      { ...base, id: "action", createdAt: 1, status: "completed", action: "remix" },
    ]));
    const records = loadVideoHistory("k", storage);
    assert.deepEqual(records.map((record) => [record.id, record.status]), [["polling", "generating"], ["lost", "interrupted"], ["submitting", "interrupted"], ["done", "completed"]]);
    assert.equal(records[0]?.requestId, "vid_1");
    assert.deepEqual(records[3]?.video, { url: "/v1/media/videos/vid_2.mp4", duration: 6 });
    assert.deepEqual(records[3]?.params, { duration: 6, aspectRatio: "16:9", resolution: "720p" });
  });
});

describe("stored list persistence", () => {
  const items = Array.from({ length: 5 }, (_, index) => ({ id: index, text: "x".repeat(100) }));

  it("caps entries and size by dropping the oldest (trailing) items", () => {
    const storage = memoryStorage();
    assert.deepEqual(persistStoredList("k", items, 3, Number.POSITIVE_INFINITY, storage).map((item) => item.id), [0, 1, 2]);
    const twoItemsBytes = JSON.stringify(items.slice(0, 2)).length * 2;
    assert.deepEqual(persistStoredList("k", items, 10, twoItemsBytes, storage).map((item) => item.id), [0, 1]);
    assert.equal(storage.values.get("k"), JSON.stringify(items.slice(0, 2)));
  });

  it("shrinks until the storage quota accepts the list and clears the key when nothing fits", () => {
    const storage = memoryStorage(JSON.stringify(items.slice(0, 2)).length);
    assert.equal(persistStoredList("k", items, 10, Number.POSITIVE_INFINITY, storage).length, 2);

    const full = memoryStorage(1);
    full.values.set("k", "stale");
    assert.deepEqual(persistStoredList("k", items, 10, Number.POSITIVE_INFINITY, full), []);
    assert.equal(full.values.has("k"), false);
  });
});
