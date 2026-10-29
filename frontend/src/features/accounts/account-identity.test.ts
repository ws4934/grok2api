import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { accountDisplayName, accountIdentityEmail } from "./account-identity.ts";

describe("account identity labels", () => {
  const consoleAccount = { provider: "grok_console", name: "Grok Console cbb90abd" };
  it("prefers an available email to a generated name", () => {
    assert.equal(accountDisplayName({ ...consoleAccount, email: " primary@example.test " }), "primary@example.test");
    assert.equal(accountDisplayName({ provider: "grok_web", name: "Grok Web cbb90abd", email: "web@example.test" }), "web@example.test");
    assert.equal(accountDisplayName({ provider: "chatgpt", name: "chatgpt-cbb90abd", email: "gpt@example.test" }), "gpt@example.test");
    assert.equal(accountDisplayName({ provider: "other", name: "other-user-123", userId: "user-123456789", email: "user@example.test" }), "user@example.test");
  });
  it("uses linked email only when this account has none, including identities synchronized later", () => {
    const account = { ...consoleAccount, linkedAccounts: [{ id: "web1", provider: "grok_web", name: "Grok Web cbb90abd", email: "linked@example.test" }] };
    assert.equal(accountDisplayName(account), "linked@example.test");
    assert.equal(accountIdentityEmail({ ...account, email: " own@example.test " }), "own@example.test");
    assert.equal(accountDisplayName({ ...account, email: "own@example.test" }), "own@example.test");
  });
  it("keeps custom names and anonymous accounts intact", () => {
    for (const name of ["生产账号", "Grok Console primary", "Grok Web campaign", "team-12345678", "renamed@example.test"]) {
      assert.equal(accountDisplayName({ ...consoleAccount, name, email: "identity@example.test" }), name);
    }
    assert.equal(accountDisplayName(consoleAccount), consoleAccount.name);
    assert.equal(accountDisplayName({ ...consoleAccount, email: "  " }), consoleAccount.name);
    assert.equal(accountDisplayName({ ...consoleAccount, name: "", email: "user@example.test" }), "user@example.test");
  });
});
