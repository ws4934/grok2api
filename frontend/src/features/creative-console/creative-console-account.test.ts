import assert from "node:assert/strict";
import { it } from "node:test";

import { canSelectCreativeAccount, creativeAccountSearch, creativeAccountState } from "./creative-console-account.ts";
import type { AccountDTO } from "../accounts/accounts-api.ts";

it("uses the existing exact-ID lookup without changing email or name searches", () => {
  assert.equal(creativeAccountSearch(" 42 "), "#42");
  assert.equal(creativeAccountSearch("#42"), "#42");
  assert.equal(creativeAccountSearch(" user@example.test "), "user@example.test");
  assert.equal(creativeAccountSearch("账号名称"), "账号名称");
});

it("keeps limited and cooling accounts selectable without enabling disabled credentials", () => {
  const account = { enabled: true, authStatus: "active", quota: { status: "waitingReset", limitKnown: true, remaining: 0 } } as AccountDTO;
  assert.equal(creativeAccountState(account), "limited");
  assert.equal(canSelectCreativeAccount(account), true);
  const cooling = { ...account, cooldownUntil: "2030-01-01T00:00:00Z", quota: { ...account.quota, status: "active" as const, remaining: 10 } };
  assert.equal(creativeAccountState(cooling, Date.parse("2026-10-29")), "cooling");
  assert.equal(canSelectCreativeAccount(cooling), true);
  assert.equal(creativeAccountState({ ...account, enabled: false }), "disabled");
  assert.equal(canSelectCreativeAccount({ ...account, enabled: false }), false);
  assert.equal(canSelectCreativeAccount({ ...account, authStatus: "reauthRequired" }), false);
});
