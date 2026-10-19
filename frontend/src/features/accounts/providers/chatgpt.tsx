import { useTranslation } from "react-i18next";

import { QuotaNotSynced, QuotaWindowRow } from "@/features/accounts/account-cells";
import { orderQuotaWindows, type ProviderAccountUI } from "@/features/accounts/account-ui";
import type { AccountDTO } from "@/features/accounts/accounts-api";
import { formatDateTime, formatNumber } from "@/shared/lib/format";

// The plan badge comes from the default UI; the quota cell merges the Codex windows.

function AuthHelp() {
  const { t } = useTranslation();
  return (
    <p className="text-xs leading-5 text-muted-foreground">
      {t("accounts.chatgpt.authHelp")}
      <code className="mt-1 block break-all font-mono text-[11px] text-foreground">https://platform.openai.com/auth/callback?code=…&amp;state=…</code>
    </p>
  );
}

const codexParts = ["codex_5h", "codex_weekly"];

// Web windows plus one Codex cell: "codex" is the tighter of the 5-hour and
// weekly Codex windows, which its tooltip lists.
function ChatGPTQuota({ account, locale, quotaModes }: { account: AccountDTO; locale: string; quotaModes: readonly string[] }) {
  const { t } = useTranslation();
  const all = account.quotaWindows ?? [];
  // Filter windows, not modes: before the catalog loads every window is shown.
  const windows = orderQuotaWindows(quotaModes, all.filter((window) => !codexParts.includes(window.mode)));
  if (windows.length === 0) return account.quotaSyncedAt ? <span className="text-xs text-muted-foreground">{t("accounts.quotaSyncedNoLimits")}</span> : <QuotaNotSynced />;
  const details = orderQuotaWindows(codexParts, all).map((part) => t("accounts.codexQuotaPart", {
    name: t(`accounts.quotaModes.${part.mode}`), remaining: formatNumber(part.remaining, locale, 0), time: part.resetAt ? formatDateTime(part.resetAt, locale) : "-",
  }));
  return <QuotaWindowRow locale={locale} items={windows.map((window) => ({
    label: t(`accounts.quotaModes.${window.mode}`, { defaultValue: window.mode }), window, ...(window.mode === "codex" ? { percent: true, details } : {}),
  }))} />;
}

export const chatgptAccountUI: Partial<ProviderAccountUI> = {
  importTexts: {
    quickImport: "accounts.chatgpt.quickImport", file: "accounts.importFile", title: "accounts.quickImportProviderTitle", description: "accounts.chatgpt.quickImportDescription",
    tokens: "accounts.chatgpt.tokens", placeholder: "accounts.chatgpt.tokenPlaceholder", filename: "chatgpt-accounts.txt",
    formats: ["accounts.chatgpt.importSub2api", "accounts.chatgpt.importCPA"],
  },
  QuotaCell: ChatGPTQuota,
  AuthHelp,
};
