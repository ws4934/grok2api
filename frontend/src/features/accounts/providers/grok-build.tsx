import { useMutation } from "@tanstack/react-query";
import { Info, TriangleAlert } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { useWatch, type UseFormReturn } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { AccountTypeText } from "@/features/accounts/account-cells";
import { accountTaskKey, type AccountEditForm, type AccountUIContext, type ProviderAccountUI } from "@/features/accounts/account-ui";
import { detectBuildAccounts, refreshAccountBilling, refreshAllAccountBilling, type AccountDTO, type AccountTaskProgressDTO, type BillingDTO, type BuildDetectItemDTO, type BuildRouteMode, type QuotaDTO } from "@/features/accounts/accounts-api";
import { webAssociationFilter } from "@/features/accounts/providers/grok-web";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber, formatTokenMillions } from "@/shared/lib/format";
import { isAbortError } from "@/shared/api/client";

type DetectCounts = Record<BuildDetectItemDTO["outcome"], number>;

const emptyDetectCounts = (): DetectCounts => ({ ok: 0, invalid: 0, failed: 0 });
const detectResultLimit = 200;

function AccountType({ account }: { account: AccountDTO }) {
  const { t } = useTranslation();
  const quota = account.quota;
  if (quota.type === "unknown") {
    return <AccountTypeText label={t("accountType.pending")} title={t("accountType.pendingDescription")} variant="muted" />;
  }
  const isFree = quota.type === "free";
  return <AccountTypeText label={isFree ? t("accountType.free") : t("accountType.paid")} variant={isFree ? "free" : "default"} />;
}

function AccountQuota({ account, locale }: { account: AccountDTO; locale: string }) {
  const { t } = useTranslation();
  const { quota, billing } = account;
  if (quota.type === "unknown") {
    return <span className="text-xs text-muted-foreground">{t("accountType.pending")}</span>;
  }
  if (quota.type !== "free") {
    return <BuildQuota quota={quota} billing={billing} locale={locale} />;
  }

  const percent = Math.min(100, Math.max(0, quota.usagePercent));
  const used = formatTokenMillions(quota.used, locale);
  const limit = formatTokenMillions(quota.limit, locale);
  const isEstimated = !quota.limitKnown;
  const recoveryDescription = quota.nextProbeAt
    ? t("accounts.waitingResetUntil", { time: formatDateTime(quota.nextProbeAt, locale) })
    : quota.status === "probing"
      ? t("accounts.probingQuota")
      : t("accounts.quotaResetUnknown");
  const usage = quota.limit > 0
    ? isEstimated ? t("accounts.freeEstimatedUsage", { used, limit }) : t("accounts.freeConfirmedUsage", { used, limit })
    : t("accounts.freeObservedUsage", { used });

  return (
    <div className="w-full min-w-0 space-y-1.5">
      <div className="flex items-start justify-between gap-3 text-[11px] font-normal">
        <div className="inline-flex min-w-0 items-center gap-1 text-muted-foreground">
          <span>{usage}</span>
          {isEstimated ? (
            <Tooltip>
              <TooltipTrigger asChild>
                <button type="button" className="inline-flex shrink-0 text-muted-foreground transition-colors hover:text-foreground" aria-label={t("accounts.freeEstimatedDescription")}>
                  <Info className="size-3.5" />
                </button>
              </TooltipTrigger>
              <TooltipContent className="max-w-80 space-y-1">
                <div>{t("accounts.freeEstimatedDescription")}</div>
                <div className="text-muted-foreground">{recoveryDescription}</div>
              </TooltipContent>
            </Tooltip>
          ) : null}
        </div>
        <span className="shrink-0 text-muted-foreground">{isEstimated ? "≈" : ""}{formatNumber(quota.usagePercent, locale, 1)}%</span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${percent}%` }} /></div>
    </div>
  );
}

function BuildQuota({ quota, billing, locale }: { quota: QuotaDTO; billing?: BillingDTO; locale: string }) {
  const { t } = useTranslation();
  const percentageQuota = quota.unit === "percent";
  const hasWeekly = percentageQuota || billing?.usagePeriodType === "USAGE_PERIOD_TYPE_WEEKLY";
  const hasMonthly = !percentageQuota && quota.limit > 0;
  if (!hasWeekly && !hasMonthly) return <span className="text-xs text-muted-foreground">{t("accounts.paidQuotaUsage")}</span>;

  const weeklyPercent = Math.max(0, Math.min(100, percentageQuota ? quota.usagePercent : (billing?.creditUsagePercent ?? 0)));
  const monthlyPercent = Math.max(0, Math.min(100, quota.usagePercent));
  const weeklyPeriodEnd = quota.periodEnd ?? billing?.usagePeriodEnd;
  return (
    <div className="w-full min-w-0 space-y-1.5">
      <div className={cn("grid w-full min-w-0 divide-x divide-border/70", hasWeekly && hasMonthly ? "grid-cols-2" : "grid-cols-1")}>
        {hasWeekly ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <button type="button" className="min-w-0 px-2 text-left font-normal first:pl-0 last:pr-0">
                <div className="flex items-center justify-between gap-1 text-[11px]"><span className="truncate text-muted-foreground">{t("accounts.weeklyQuota")}</span><span className="shrink-0 tabular-nums">{formatNumber(weeklyPercent, locale, 1)}%</span></div>
                <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${weeklyPercent}%` }} /></div>
              </button>
            </TooltipTrigger>
            <TooltipContent>
              <div>{t("accounts.weeklyLimit", { percent: formatNumber(100 - weeklyPercent, locale, 1) })}</div>
              <div className="text-muted-foreground">{weeklyPeriodEnd ? t("accounts.quotaResetAt", { time: formatDateTime(weeklyPeriodEnd, locale) }) : t("accounts.quotaResetUnknown")}</div>
            </TooltipContent>
          </Tooltip>
        ) : null}
        {hasMonthly ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <button type="button" className="min-w-0 px-2 text-left font-normal first:pl-0 last:pr-0">
                <div className="flex items-center justify-between gap-1 text-[11px]"><span className="truncate text-muted-foreground">{t("accounts.monthlyQuota")}</span><span className="shrink-0 tabular-nums">{formatNumber(quota.used, locale, 2)}/{formatNumber(quota.limit, locale, 2)}</span></div>
                <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${monthlyPercent}%` }} /></div>
              </button>
            </TooltipTrigger>
            <TooltipContent>
              <div>{t("accounts.paidQuotaDetails", { remaining: formatNumber(quota.remaining, locale, 2) })}</div>
              <div className="text-muted-foreground">{billing?.billingPeriodEnd ? t("accounts.quotaResetAt", { time: formatDateTime(billing.billingPeriodEnd, locale) }) : t("accounts.quotaResetUnknown")}</div>
            </TooltipContent>
          </Tooltip>
        ) : null}
      </div>
    </div>
  );
}

function EditFields({ account, form }: { account: AccountDTO; form: UseFormReturn<AccountEditForm> }) {
  const { t } = useTranslation();
  const buildSuperEntitled = useWatch({ control: form.control, name: "buildSuperEntitled" });
  const buildRouteMode = useWatch({ control: form.control, name: "buildRouteMode" });
  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between gap-4 rounded-md bg-muted/50 p-3">
        <div className="space-y-1">
          <Label htmlFor="account-build-super-entitled">{t("accounts.buildSuperEntitled.label")}</Label>
          <p className="text-xs text-muted-foreground">{t("accounts.buildSuperEntitled.description")}</p>
        </div>
        <Switch id="account-build-super-entitled" checked={buildSuperEntitled} onCheckedChange={(checked) => form.setValue("buildSuperEntitled", checked, { shouldDirty: true })} />
      </div>
      <div className="space-y-2">
        <Label id="account-build-route-mode">{t("accounts.buildRouteMode.label")}</Label>
        <Tabs value={buildRouteMode} onValueChange={(value) => form.setValue("buildRouteMode", value as BuildRouteMode, { shouldDirty: true })}>
          <TabsList aria-labelledby="account-build-route-mode" className="grid h-10 w-full grid-cols-3 p-1">
            {(["auto", "build", "xai"] as BuildRouteMode[]).map((mode) => (
              <TabsTrigger key={mode} value={mode} className="h-8 px-2 font-normal data-[state=active]:font-medium">
                {t(`accounts.buildRouteMode.${mode}`)}
              </TabsTrigger>
            ))}
          </TabsList>
        </Tabs>
        <p className="text-xs text-muted-foreground">{t(`accounts.buildRouteMode.${buildRouteMode}Description`)}</p>
        {buildRouteMode === "xai" && !buildSuperEntitled && !(account.quota.type === "paid" && account.quota.source !== "buildSuperEntitlement") ? (
          <p className="flex items-start gap-1.5 text-xs text-amber-700 dark:text-amber-300"><TriangleAlert className="mt-0.5 size-3.5 shrink-0" />{t("accounts.buildRouteMode.xaiUnconfirmedWarning")}</p>
        ) : null}
      </div>
    </div>
  );
}

function BulkActions({ targets, ctx }: { targets: string[] | "all"; ctx: AccountUIContext }) {
  const { t } = useTranslation();
  return <Button variant="secondary" size="sm" disabled={ctx.pending} onClick={() => ctx.openTask({ kind: "detect", targets })}>{t("accountCredential.detectAction")}</Button>;
}

function Dialogs({ ctx }: { ctx: AccountUIContext }) {
  return ctx.task?.kind === "detect" ? <DetectDialog targets={ctx.task.targets} ctx={ctx} /> : null;
}

function DetectDialog({ targets, ctx }: { targets: string[] | "all"; ctx: AccountUIContext }) {
  const { t } = useTranslation();
  const abortRef = useRef<AbortController | null>(null);
  const outcomeByIDRef = useRef(new Map<string, BuildDetectItemDTO["outcome"]>());
  const [progress, setProgress] = useState<AccountTaskProgressDTO | null>(null);
  const [items, setItems] = useState<BuildDetectItemDTO[]>([]);
  const [counts, setCounts] = useState<DetectCounts>(emptyDetectCounts);
  const mode = targets === "all" ? "all" : "selected";
  const selectedCount = targets === "all" ? 0 : targets.length;

  // Closing the dialog unmounts it and cancels a running detection.
  useEffect(() => () => abortRef.current?.abort(), []);

  const appendItem = useCallback((item: BuildDetectItemDTO) => {
    const previousOutcome = outcomeByIDRef.current.get(item.id);
    outcomeByIDRef.current.set(item.id, item.outcome);
    if (previousOutcome !== item.outcome) {
      setCounts((previous) => ({
        ...previous,
        ...(previousOutcome ? { [previousOutcome]: Math.max(0, previous[previousOutcome] - 1) } : {}),
        [item.outcome]: previous[item.outcome] + 1,
      }));
    }
    setItems((previous) => [item, ...previous.filter((entry) => entry.id !== item.id)].slice(0, detectResultLimit));
  }, []);

  const mutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: () => {
      const controller = new AbortController();
      abortRef.current = controller;
      setProgress(null);
      outcomeByIDRef.current.clear();
      setCounts(emptyDetectCounts());
      setItems([]);
      return detectBuildAccounts(targets === "all" ? { all: true } : { ids: targets }, { onProgress: setProgress, onItem: appendItem }, controller.signal);
    },
    onSuccess: (result) => {
      if (mode === "selected") ctx.clearSelection();
      toast.success(t(mode === "all" ? "accounts.allDetected" : "accounts.batchDetected", result));
    },
    onError: (error) => { if (!isAbortError(error)) ctx.showError(error); },
    onSettled: () => {
      abortRef.current = null;
      ctx.invalidate();
    },
  });

  const total = counts.ok + counts.invalid + counts.failed;
  const visibleItems = mode === "selected" ? items : items.filter((item) => item.outcome === "invalid");
  return (
    <Dialog open onOpenChange={(open) => { if (!open) ctx.closeTask(); }}>
      <DialogContent className="max-w-xl gap-4 sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{mode === "all" ? t("accounts.detectAllTitle") : t("accounts.detectSelectedTitle", { count: selectedCount })}</DialogTitle>
          <DialogDescription>{mode === "all" ? t("accounts.detectAllDescription") : t("accounts.detectSelectedDescription", { count: selectedCount })}</DialogDescription>
        </DialogHeader>
        {(mutation.isPending || progress || visibleItems.length > 0) ? (
          <div className="space-y-3">
            <div className="flex items-center justify-between gap-3 rounded-md border bg-muted/30 px-3 py-2 text-sm">
              <span className="text-muted-foreground">{t("accounts.detectProgressLabel")}</span>
              <span className="tabular-nums font-medium">
                {progress ? `${progress.completed} / ${progress.total}` : mutation.isPending ? t("common.loading") : "—"}
              </span>
            </div>
            {mode === "all" && counts.invalid > 0 ? <p className="text-xs text-muted-foreground">{t("accounts.detectInvalidCount", { count: counts.invalid })}</p> : null}
            {mode === "selected" && total > 0 ? <p className="text-xs text-muted-foreground">{t("accounts.detectSelectedSummary", counts)}</p> : null}
            {total > visibleItems.length ? <p className="text-xs text-muted-foreground">{t("accounts.detectResultsLimited", { count: detectResultLimit })}</p> : null}
            <div className="max-h-64 overflow-y-auto rounded-md border">
              {visibleItems.length === 0 ? (
                <div className="px-3 py-6 text-center text-sm text-muted-foreground">
                  {mutation.isPending
                    ? t(mode === "all" ? "accounts.detectWaitingInvalid" : "accounts.detectWaitingResults")
                    : t(mode === "all" ? "accounts.detectNoInvalid" : "accounts.detectNoResults")}
                </div>
              ) : (
                <ul className="divide-y">
                  {visibleItems.map((item) => (
                    <li key={`${item.id}-${item.outcome}-${item.reason ?? ""}`} className="flex items-start gap-3 px-3 py-2 text-sm">
                      <Badge
                        variant="outline"
                        className={cn(
                          "mt-0.5 shrink-0",
                          item.outcome === "ok" && "border-emerald-500/40 text-emerald-700 dark:text-emerald-300",
                          item.outcome === "invalid" && "border-destructive/40 text-destructive",
                          item.outcome === "failed" && "border-amber-500/40 text-amber-700 dark:text-amber-300",
                        )}
                      >
                        {t(`accounts.detectOutcome.${item.outcome}`)}
                      </Badge>
                      <div className="min-w-0 flex-1">
                        <div className="truncate font-medium">{item.name || item.id}</div>
                        {item.email ? <div className="truncate text-xs text-muted-foreground">{item.email}</div> : null}
                        {item.reason ? <div className="mt-0.5 break-all text-xs text-muted-foreground">{item.reason}</div> : null}
                      </div>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          </div>
        ) : null}
        <DialogFooter>
          <Button variant="outline" onClick={ctx.closeTask}>{mutation.isPending ? t("common.cancel") : t("common.close")}</Button>
          <Button disabled={mutation.isPending || (mode === "selected" && selectedCount === 0)} onClick={() => mutation.mutate()}>
            {mutation.isPending ? (
              <>
                <Spinner />
                {progress ? <span className="tabular-nums">{progress.completed} / {progress.total}</span> : t("common.loading")}
              </>
            ) : t("accounts.detectAll")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export const grokBuildAccountUI: Partial<ProviderAccountUI> = {
  TypeCell: AccountType,
  QuotaCell: AccountQuota,
  importTexts: {
    quickImport: "accounts.quickImportRT", file: "accounts.importAuth", title: "accounts.quickImportRTTitle", description: "accounts.quickImportRTDescription",
    tokens: "accounts.refreshTokens", placeholder: "accounts.refreshTokenPlaceholder", filename: "grok-build-refresh-tokens.txt",
  },
  syncAllDescription: "accountQuotaTask.allDescription",
  typeOptions: (t) => [
    { value: "free", label: t("accountType.free") },
    { value: "paid", label: t("accountType.paid") },
    { value: "unknown", label: t("accountType.pending") },
  ],
  filters: (t) => [
    { id: "renewal", label: t("accountCredential.label"), options: [
      { value: "refreshable", label: t("accountCredential.autoRefresh") },
      { value: "unrefreshable", label: t("accountCredential.noAutoRefresh") },
    ] },
    { id: "risk", label: t("accounts.riskFilter"), options: [
      { value: "flagged", label: t("accounts.botRisk") },
      { value: "normal", label: t("accounts.riskNormal") },
    ] },
    webAssociationFilter(t),
  ],
  linkedProviders: ["grok_web", "grok_console"],
  syncAll: refreshAllAccountBilling,
  refreshQuota: refreshAccountBilling,
  quotaReset: true,
  renewAll: true,
  EditFields,
  editInput: (values, account) => ({
    buildRouteMode: values.buildRouteMode,
    ...(values.buildSuperEntitled !== account.buildSuperEntitled ? { buildSuperEntitled: values.buildSuperEntitled } : {}),
  }),
  BulkActions,
  Dialogs,
};
