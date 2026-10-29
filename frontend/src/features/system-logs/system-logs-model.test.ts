import assert from "node:assert/strict";
import { it } from "node:test";
import { logQuery, logReason, logSummary, systemLogsSchema } from "./system-logs-model.ts";

it("encodes literal log filters and cursor without inventing provider routes", () => {
  const query = new URLSearchParams(logQuery({ search: " CF & 100% ", level: "WARN", provider: "console", accountId: "1364", requestId: "request-1" }, "42"));
  assert.equal(query.get("search"), "CF & 100%"); assert.equal(query.get("accountId"), "1364"); assert.equal(query.get("provider"), "console"); assert.equal(query.get("cursor"), "42"); assert.equal(query.get("limit"), "50");
});
it("validates log pages and preserves structured failure details", () => {
  const page = { items: [{ id: 1, createdAt: "2026-10-29T17:00:00Z", level: "WARN", message: "account_quota_sync_failed", instanceId: "node", provider: "console", accountId: 1364, requestId: "request-1", fields: { error: "出口被 Cloudflare 拦截", stage: "upstream", retry_after_seconds: 600 } }], nextCursor: "", status: { queueDepth: 0, dropped: 0, writeFailures: 0, lastError: "", lastFailureAt: null, lastSavedAt: null, retentionDays: 7, maxEntries: 10000 } };
  assert.equal(logReason(systemLogsSchema.parse(page).items[0]), "出口被 Cloudflare 拦截");
  assert.throws(() => systemLogsSchema.parse({ ...page, items: [{ ...page.items[0], fields: "unsafe shape" }] }));
});

it("shows the HTTP method, route and status instead of a wall of generic request events", () => {
  const entry = { id: 2, createdAt: "2026-10-29T17:00:00Z", level: "ERROR", message: "http_request", instanceId: "node", provider: "", accountId: 0, requestId: "request-2", fields: { method: "POST", path: "/api/admin/v1/accounts/:id/refresh-quota", status: 503, duration_ms: 8 } };
  assert.equal(logSummary(entry), "POST /api/admin/v1/accounts/:id/refresh-quota · HTTP 503 · 8 ms");
});
