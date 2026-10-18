import { useTranslation } from "react-i18next";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { AccountDTO } from "@/features/accounts/accounts-api";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";

// Cell primitives shared by the provider account modules.

export type QuotaWindow = NonNullable<AccountDTO["quotaWindows"]>[number];

export function AccountTypeText({ label, title, variant }: { label: string; title?: string; variant: "default" | "free" | "muted" }) {
  if (variant === "muted") {
    return <span title={title ?? label} className="text-xs text-muted-foreground">{label}</span>;
  }
  return <span title={title ?? label} className={cn("max-w-32 truncate text-xs font-medium", variant === "free" ? "text-emerald-700 dark:text-emerald-300" : "text-primary")}>{label}</span>;
}

export function QuotaNotSynced() {
  const { t } = useTranslation();
  return <span className="text-xs text-muted-foreground">{t("accounts.quotaNotSynced")}</span>;
}

// Used/total bar of one counted window.
export function QuotaModeBar({ mode, window, locale, compact = false, recoveryProbe = false }: { mode: string; window: QuotaWindow; locale: string; compact?: boolean; recoveryProbe?: boolean }) {
  const { t } = useTranslation();
  const used = Math.max(0, window.total - window.remaining);
  const percent = window.total > 0 ? Math.max(0, Math.min(100, used / window.total * 100)) : 0;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className={cn("block w-full min-w-0 text-left", compact && "px-2 first:pl-0 last:pr-0")}>
          <div className="flex items-center justify-between gap-1 text-[11px]"><span className="truncate text-muted-foreground">{mode}</span><span className="shrink-0 tabular-nums">{formatNumber(used, locale, 0)}/{formatNumber(window.total, locale, 0)}</span></div>
          <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${percent}%` }} /></div>
        </button>
      </TooltipTrigger>
      <TooltipContent><div>{t("accounts.webModeQuotaRemaining", { mode, remaining: formatNumber(window.remaining, locale, 0) })}</div><div className="text-muted-foreground">{window.resetAt ? recoveryProbe ? t("console.recoveryProbeAt", { time: formatDateTime(window.resetAt, locale) }) : t("accounts.quotaResetAt", { time: formatDateTime(window.resetAt, locale) }) : t("accounts.quotaResetUnknown")}</div></TooltipContent>
    </Tooltip>
  );
}

// Remaining-count bars (total may be unknown) with the reset time in the tooltip.
export function QuotaWindowRow({ items, locale }: { items: Array<{ label: string; window: QuotaWindow }>; locale: string }) {
  const { t } = useTranslation();
  if (items.length === 0) return null;
  return (
    <div className="grid w-full min-w-0 divide-x divide-border/70" style={{ gridTemplateColumns: `repeat(${items.length}, minmax(0, 1fr))` }}>
      {items.map(({ label, window }) => {
        const exhausted = window.remaining <= 0;
        const hasTotal = window.total > 0;
        const used = hasTotal ? Math.max(0, window.total - window.remaining) : 0;
        const percent = hasTotal ? Math.max(0, Math.min(100, used / window.total * 100)) : 0;
        return (
          <Tooltip key={window.mode}>
            <TooltipTrigger asChild>
              <button type="button" className="block w-full min-w-0 px-2 text-left first:pl-0 last:pr-0">
                <div className="flex items-center justify-between gap-1 text-[11px]">
                  <span className={cn("truncate", exhausted ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground")}>{label}</span>
                  {exhausted ? (
                    <span className="shrink-0 truncate text-amber-600 dark:text-amber-400">{t("accounts.imagineQuotaExhausted")}</span>
                  ) : hasTotal ? (
                    <span className="shrink-0 tabular-nums text-muted-foreground">{formatNumber(window.remaining, locale, 0)}/{formatNumber(window.total, locale, 0)}</span>
                  ) : (
                    <span className="shrink-0 tabular-nums text-muted-foreground">{formatNumber(window.remaining, locale, 0)}/-</span>
                  )}
                </div>
                <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted">
                  {exhausted ? <div className="h-full w-full bg-amber-500" /> : hasTotal ? <div className="h-full bg-primary" style={{ width: `${percent}%` }} /> : null}
                </div>
              </button>
            </TooltipTrigger>
            <TooltipContent>
              <div>{exhausted ? t("accounts.imagineQuotaExhausted") : t("accounts.imagineQuotaRemaining", { remaining: formatNumber(window.remaining, locale, 0) })}</div>
              <div className="text-muted-foreground">{window.resetAt ? t("accounts.quotaResetAt", { time: formatDateTime(window.resetAt, locale) }) : t("accounts.quotaResetUnknown")}</div>
            </TooltipContent>
          </Tooltip>
        );
      })}
    </div>
  );
}
