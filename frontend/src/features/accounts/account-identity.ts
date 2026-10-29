import type { AccountDTO } from "./accounts-api.ts";

type AccountIdentity = Pick<AccountDTO, "provider" | "name" | "email" | "userId" | "linkedAccounts">;

// Linked identities are already reconciled by the server; use them for display,
// without copying one provider's identity into another or changing stored names.
export function accountIdentityEmail(account: AccountIdentity): string | undefined {
  return account.email?.trim() || account.linkedAccounts?.find((linked) => linked.email?.trim())?.email?.trim() || undefined;
}

export function accountDisplayName(account: AccountIdentity): string {
  const name = account.name.trim();
  if (name && !isGeneratedName(account, name)) return name;
  return accountIdentityEmail(account) || name || account.userId?.trim() || account.provider;
}

function isGeneratedName(account: AccountIdentity, name: string): boolean {
  const prefix = account.provider === "grok_console" ? "Grok Console " : account.provider === "grok_web" ? "Grok Web " : "";
  if (prefix && name.startsWith(prefix) && /^[a-f0-9]{8}$/i.test(name.slice(prefix.length))) return true;
  const genericPrefix = `${account.provider}-`;
  if (!name.startsWith(genericPrefix)) return false;
  const id = name.slice(genericPrefix.length);
  return /^[a-f0-9]{8}$/i.test(id) || Boolean(account.userId?.trim() && id === account.userId.trim().slice(0, 8));
}
