import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { galleryQuery, localDayStart, mediaAssetPath, mediaAssetSchema, mediaJobActions, mediaJobSchema, mediaJobsQuery, mediaJobsRefetchInterval, pageSchema, retriedMediaJobSchema, type MediaJobStatus } from "./media-model.ts";

describe("media jobs query", () => {
  it("sends only set filters and pairs sort fields", () => {
    assert.equal(mediaJobsQuery({ page: 1, pageSize: 20 }), "page=1&pageSize=20");
    assert.equal(
      mediaJobsQuery({ page: 2, pageSize: 50, kind: "image", status: "uncertain", provider: "chatgpt", search: "a b", sortBy: "createdAt", sortOrder: "desc" }),
      "page=2&pageSize=50&kind=image&status=uncertain&provider=chatgpt&search=a+b&sortBy=createdAt&sortOrder=desc",
    );
    assert.equal(mediaJobsQuery({ page: 1, pageSize: 20, kind: "", sortBy: "model" }), "page=1&pageSize=20");
  });
});

describe("gallery query", () => {
  it("always sends kind and the provenance filters", () => {
    const query = new URLSearchParams(galleryQuery({ page: 1, pageSize: 20, kind: "video", provider: "grok_console", model: "grok-imagine-video", operation: "video", clientKeyId: "7", accountId: "12", search: "" }));
    assert.deepEqual(Object.fromEntries(query), { page: "1", pageSize: "20", kind: "video", provider: "grok_console", model: "grok-imagine-video", operation: "video", clientKeyId: "7", accountId: "12" });
  });

  it("turns inclusive local days into an RFC3339 [from, to) range", () => {
    const raw = galleryQuery({ page: 1, pageSize: 20, kind: "image", from: "2026-10-18", to: "2026-10-20" });
    const query = new URLSearchParams(raw);
    assert.equal(query.get("from"), new Date(2026, 9, 18).toISOString());
    assert.equal(query.get("to"), new Date(2026, 9, 21).toISOString());
    // Reserved characters are percent-encoded, so a "+" offset could never be read back as a space.
    assert.match(raw, /from=\d{4}-\d\d-\d\dT\d\d%3A\d\d%3A00\.000Z/);
    assert.ok(!raw.includes("+"));
  });

  it("drops partial or malformed days", () => {
    assert.equal(localDayStart("", 0), undefined);
    assert.equal(localDayStart("2026-10", 0), undefined);
    assert.equal(localDayStart("2026-12-31", 1), new Date(2027, 0, 1).toISOString());
    assert.equal(new URLSearchParams(galleryQuery({ page: 1, pageSize: 20, kind: "image", from: "bad" })).has("from"), false);
  });
});

describe("media job actions", () => {
  const job = (status: MediaJobStatus, flags: { cancellable?: boolean; retryable?: boolean; assetIds?: string[]; usagePending?: boolean } = {}) => ({
    status, cancellable: flags.cancellable ?? false, retryable: flags.retryable ?? false, assetIds: flags.assetIds ?? [], usagePending: flags.usagePending ?? false,
  });

  it("maps statuses to the allowed actions", () => {
    assert.deepEqual(mediaJobActions(job("queued", { cancellable: true })), { cancel: true, retry: false, resume: false, delete: false, results: false });
    assert.deepEqual(mediaJobActions(job("in_progress")), { cancel: false, retry: false, resume: false, delete: false, results: false });
    assert.deepEqual(mediaJobActions(job("in_progress", { cancellable: true, assetIds: ["a"] })), { cancel: true, retry: false, resume: false, delete: false, results: true });
    assert.deepEqual(mediaJobActions(job("completed", { assetIds: ["a"] })), { cancel: false, retry: false, resume: false, delete: true, results: true });
    assert.deepEqual(mediaJobActions(job("failed")), { cancel: false, retry: false, resume: false, delete: true, results: false });
    assert.deepEqual(mediaJobActions(job("canceled")), { cancel: false, retry: false, resume: false, delete: true, results: false });
    assert.deepEqual(mediaJobActions(job("uncertain", { retryable: true })), { cancel: false, retry: true, resume: false, delete: true, results: false });
  });

  it("labels a retryable failed job as fetching its result again, not as a retry", () => {
    assert.deepEqual(mediaJobActions(job("failed", { retryable: true, assetIds: ["a"] })), { cancel: false, retry: false, resume: true, delete: true, results: true });
  });

  it("ignores server flags that contradict the status", () => {
    assert.equal(mediaJobActions(job("completed", { cancellable: true })).cancel, false);
    const completed = mediaJobActions(job("completed", { retryable: true }));
    assert.equal(completed.retry || completed.resume, false);
  });

  it("keeps unsettled jobs, without hiding their delivered results", () => {
    assert.deepEqual(mediaJobActions(job("completed", { usagePending: true, assetIds: ["a"] })), { cancel: false, retry: false, resume: false, delete: false, results: true });
    assert.equal(mediaJobActions(job("canceled", { usagePending: true })).delete, false);
  });
});

describe("media jobs polling", () => {
  it("polls while a listed job is running or awaiting settlement", () => {
    assert.equal(mediaJobsRefetchInterval(), false);
    assert.equal(mediaJobsRefetchInterval([{ status: "completed", usagePending: false }, { status: "failed", usagePending: false }, { status: "uncertain", usagePending: false }]), false);
    assert.equal(mediaJobsRefetchInterval([{ status: "completed", usagePending: false }, { status: "queued", usagePending: false }]), 5_000);
    assert.equal(mediaJobsRefetchInterval([{ status: "in_progress", usagePending: false }]), 5_000);
    assert.equal(mediaJobsRefetchInterval([{ status: "completed", usagePending: true }]), 5_000);
    assert.equal(mediaJobsRefetchInterval([{ status: "canceled", usagePending: true }]), 5_000);
  });
});

// The job payload the server sends: every field present, empty values where unset.
const jobPayload = {
  id: "img_1", kind: "image", provider: "", model: "", operation: "", prompt: "", status: "queued", progress: 0, seconds: 0,
  size: "", quality: "", assetIds: [], accountName: "", clientKeyName: "", createdAt: "2026-10-18T00:00:00Z",
  completedAt: null, errorCode: "", errorMessage: "", cancellable: true, retryable: false, usagePending: false,
};

describe("media job retry result", () => {
  it("tells a resumed job from a new one", () => {
    assert.equal(retriedMediaJobSchema.parse({ ...jobPayload, resumed: true }).resumed, true);
    assert.equal(retriedMediaJobSchema.parse({ ...jobPayload, resumed: false }).resumed, false);
  });
});

describe("media decoders", () => {
  it("decodes a job and rejects one missing a field", () => {
    assert.equal(mediaJobSchema.parse(jobPayload).completedAt, null);
    const missing: Record<string, unknown> = { ...jobPayload };
    delete missing.errorCode;
    assert.throws(() => mediaJobSchema.parse(missing));
  });

  it("rejects unknown statuses and kinds", () => {
    assert.throws(() => mediaJobSchema.parse({ ...jobPayload, status: "paused" }));
    assert.throws(() => mediaJobSchema.parse({ ...jobPayload, kind: "audio" }));
  });

  it("decodes an uploaded asset with empty provenance", () => {
    const page = pageSchema(mediaAssetSchema).parse({
      items: [{
        id: "a", kind: "image", mimeType: "image/png", sizeBytes: 10, sha256: "h", createdAt: "2026-10-18T00:00:00Z", url: "http://x/v1/media/images/a",
        provider: "", model: "", upstreamModel: "", clientKeyId: 0, clientKeyName: "", accountId: 0, accountName: "", operation: "", jobId: "", requestId: "", prompt: "",
      }],
      page: 1, pageSize: 20, total: 1,
    });
    assert.equal(page.items[0].jobId, "");
  });
});

describe("media asset path", () => {
  it("routes by kind and escapes the id", () => {
    assert.equal(mediaAssetPath("image", "a/b"), "/v1/media/images/a%2Fb");
    assert.equal(mediaAssetPath("video", "v1"), "/v1/media/videos/v1");
  });
});
