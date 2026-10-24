import { useTranslation } from "react-i18next";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { AccountTypeText, QuotaNotSynced } from "@/features/accounts/account-cells";
import type { ProviderAccountUI } from "@/features/accounts/account-ui";
import type { AccountDTO } from "@/features/accounts/accounts-api";
import { CloudflareCookieFields, cloudflareCookieInput, webAssociationFilter } from "@/features/accounts/providers/grok-web";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";

function ConsoleType() {
  const { t } = useTranslation();
  return <AccountTypeText label={t("accountType.console")} variant="free" />;
}

// Console chat, image and video all draw on one weekly included-usage pool
// measured in cents. The backend mirrors that pool into the three Console
// modes, so it is rendered once as dollars.
function ConsoleQuota({ account, locale }: { account: AccountDTO; locale: string }) {
  const { t } = useTranslation();
  const windows = account.quotaWindows ?? [];
  const window = ["console", "console_image", "console_video"]
    .map((mode) => windows.find((item) => item.mode === mode))
    .find((item) => item !== undefined);
  if (!window) return <QuotaNotSynced />;
  const usedCents = Math.max(0, window.total - window.remaining);
  const percent = Math.max(0, Math.min(100, window.usagePercent));
  const exhausted = window.remaining <= 0;
  const dollars = (cents: number) => `$${formatNumber(cents / 100, locale, 2)}`;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="block w-full min-w-0 text-left font-normal">
          <div className="flex items-center justify-between gap-2 text-[11px]">
            <span className={cn("truncate", exhausted ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground")}>{t("accounts.weeklyQuota")}</span>
            <span className="shrink-0 tabular-nums">{dollars(usedCents)}/{dollars(window.total)}</span>
          </div>
          <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted"><div className={cn("h-full", exhausted ? "bg-amber-500" : "bg-primary")} style={{ width: `${percent}%` }} /></div>
        </button>
      </TooltipTrigger>
      <TooltipContent>
        <div>{t("accounts.consoleWeeklyUsage", { remaining: dollars(window.remaining), percent: formatNumber(percent, locale, 1) })}</div>
        <div className="text-muted-foreground">{window.resetAt ? t("accounts.quotaResetAt", { time: formatDateTime(window.resetAt, locale) }) : t("accounts.quotaResetUnknown")}</div>
      </TooltipContent>
    </Tooltip>
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
