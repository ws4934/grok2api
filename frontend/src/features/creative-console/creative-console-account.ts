import type { AccountDTO } from "../accounts/accounts-api.ts";

type AccountState = Pick<AccountDTO, "enabled" | "authStatus" | "cooldownUntil" | "quota">;

export function creativeAccountSearch(value: string): string {
  const search = value.trim();
  return /^\d+$/.test(search) ? `#${search}` : search;
}

export function creativeAccountState(account: AccountState, now = Date.now()): "disabled" | "reauth" | "limited" | "cooling" | "active" {
  if (!account.enabled) return "disabled";
  if (account.authStatus !== "active") return "reauth";
  if (account.quota.status !== "active" || (account.quota.limitKnown && account.quota.remaining <= 0)) return "limited";
  if (account.cooldownUntil && Date.parse(account.cooldownUntil) > now) return "cooling";
  return "active";
}

export function canSelectCreativeAccount(account: AccountState): boolean {
  return account.enabled && account.authStatus === "active";
}
