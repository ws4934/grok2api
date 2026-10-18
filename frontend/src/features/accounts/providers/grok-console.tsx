import { useTranslation } from "react-i18next";

import { AccountTypeText, QuotaModeBar, QuotaNotSynced } from "@/features/accounts/account-cells";
import type { ProviderAccountUI } from "@/features/accounts/account-ui";
import type { AccountDTO } from "@/features/accounts/accounts-api";
import { CloudflareCookieFields, cloudflareCookieInput, webAssociationFilter } from "@/features/accounts/providers/grok-web";

const consoleQuotaModes = [
  { mode: "console", label: "Chat" },
  { mode: "console_image", label: "Image" },
  { mode: "console_video", label: "Video" },
] as const;

function ConsoleType() {
  const { t } = useTranslation();
  return <AccountTypeText label={t("accountType.console")} variant="free" />;
}

function ConsoleQuota({ account, locale }: { account: AccountDTO; locale: string }) {
  const windows = account.quotaWindows ?? [];
  if (windows.length === 0) return <QuotaNotSynced />;
  const windowsByMode = new Map(windows.map((window) => [window.mode, window]));
  return (
    <div className="grid w-full min-w-0 grid-cols-[2fr_1fr_1fr] divide-x divide-border/70">
      {consoleQuotaModes.map(({ mode, label }) => {
        const window = windowsByMode.get(mode);
        if (!window) {
          return <div key={mode} className="min-w-0 px-2 first:pl-0 last:pr-0"><div className="flex items-center justify-between gap-1 text-[11px]"><span className="truncate text-muted-foreground">{label}</span><span className="text-muted-foreground">-</span></div><div className="mt-1.5 h-1.5 rounded-full bg-muted" /></div>;
        }
        return <QuotaModeBar key={mode} mode={label} window={window} locale={locale} compact recoveryProbe={mode === "console" && window.remaining === 0} />;
      })}
    </div>
  );
}

export const grokConsoleAccountUI: Partial<ProviderAccountUI> = {
  TypeCell: ConsoleType,
  QuotaCell: ConsoleQuota,
  importTexts: {
    quickImport: "accounts.quickImportSSO", file: "console.importFile", title: "console.quickImportTitle", description: "console.quickImportDescription",
    tokens: "accounts.ssoTokens", placeholder: "accounts.ssoTokenPlaceholder", filename: "grok-console-sso-tokens.txt",
  },
  syncAllDescription: "console.syncAllDescription",
  filters: (t) => [webAssociationFilter(t)],
  linkedProviders: ["grok_web", "grok_build"],
  // Console routing also accepts Grok Web exits.
  egressScopes: ["grok_console", "grok_web"],
  blockingQuotaMode: "console",
  EditFields: CloudflareCookieFields,
  editInput: cloudflareCookieInput,
};
