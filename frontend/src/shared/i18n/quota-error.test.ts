import assert from "node:assert/strict";
import { it } from "node:test";

import { i18n } from "./index.ts";

it("uses a provider-neutral quota error for Console and other account providers", () => {
  assert.equal(i18n.getFixedT("zh-CN")("apiErrors.quotaRefreshFailed"), "同步账号额度失败");
  assert.equal(i18n.getFixedT("en")("apiErrors.quotaRefreshFailed"), "Failed to sync the account quota");
});

it("keeps the Cloudflare cause actionable and the log page bilingual", () => {
  assert.match(i18n.getFixedT("zh-CN")("apiErrors.quotaSyncEgressBlocked"), /Cloudflare.*Clearance/);
  assert.equal(i18n.getFixedT("zh-CN")("systemLogs.title"), "系统日志");
  assert.equal(i18n.getFixedT("en")("systemLogs.title"), "System logs");
});
