import { useTranslation } from "react-i18next";

import { AccountTypeText, QuotaNotSynced, QuotaWindowRow } from "@/features/accounts/account-cells";
import { orderQuotaWindows, resolveAccountUI, type ProviderAccountUI } from "@/features/accounts/account-ui";
import type { AccountDTO } from "@/features/accounts/accounts-api";
import { chatgptAccountUI } from "@/features/accounts/providers/chatgpt";
import { grokBuildAccountUI } from "@/features/accounts/providers/grok-build";
import { grokConsoleAccountUI } from "@/features/accounts/providers/grok-console";
import { grokWebAccountUI } from "@/features/accounts/providers/grok-web";

function PlanType({ account }: { account: AccountDTO }) {
  const plan = account.plan?.trim() ?? "";
  if (!plan) return <AccountTypeText label="-" variant="muted" />;
  return <AccountTypeText label={plan.charAt(0).toUpperCase() + plan.slice(1)} variant={plan.toLowerCase() === "free" ? "free" : "default"} />;
}

function WindowQuota({ account, locale, quotaModes }: { account: AccountDTO; locale: string; quotaModes: readonly string[] }) {
  const { t } = useTranslation();
  const windows = orderQuotaWindows(quotaModes, account.quotaWindows ?? []);
  // A synced account may report no limited windows at all; only a missing sync time means "not synced".
  if (windows.length === 0) return account.quotaSyncedAt ? <span className="text-xs text-muted-foreground">{t("accounts.quotaSyncedNoLimits")}</span> : <QuotaNotSynced />;
  return <QuotaWindowRow locale={locale} items={windows.map((window) => ({ label: t(`accounts.quotaModes.${window.mode}`, { defaultValue: window.mode }), window }))} />;
}

// Providers without a module get the plan, per-mode quota windows and the generic import texts.
const defaultAccountUI: ProviderAccountUI = {
  TypeCell: PlanType,
  QuotaCell: WindowQuota,
  importTexts: {
    quickImport: "accounts.quickImport", file: "accounts.importFile", title: "accounts.quickImportProviderTitle", description: "accounts.quickImportProviderDescription",
    tokens: "accounts.credentials", placeholder: "accounts.credentialsPlaceholder", filename: "accounts.txt",
  },
  syncAllDescription: "accounts.syncAllProviderDescription",
};

const providerModules: Readonly<Record<string, Partial<ProviderAccountUI>>> = {
  grok_build: grokBuildAccountUI,
  grok_web: grokWebAccountUI,
  grok_console: grokConsoleAccountUI,
  chatgpt: chatgptAccountUI,
};

export function accountUI(provider: string): ProviderAccountUI {
  return resolveAccountUI(provider, providerModules, defaultAccountUI);
}
