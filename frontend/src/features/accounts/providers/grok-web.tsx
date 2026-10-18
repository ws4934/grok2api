import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { TFunction } from "i18next";
import { ArrowRight, SquareTerminal, Webhook } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useWatch, type UseFormReturn } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { DropdownMenuItem } from "@/components/ui/dropdown-menu";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { AccountTypeText, QuotaModeBar, QuotaNotSynced, QuotaWindowRow, type QuotaWindow } from "@/features/accounts/account-cells";
import { accountTaskKey, isAbortError, type AccountEditForm, type AccountFilterDef, type AccountUIContext, type ProviderAccountUI } from "@/features/accounts/account-ui";
import {
  acceptWebAccountTerms,
  convertWebAccountsToBuild,
  enableWebAccountNSFW,
  runWebAccountScripts,
  setWebAccountBirthDate,
  syncWebAccountsToConsole,
  type AccountDTO,
  type AccountSyncStrategy,
  type AccountTaskProgressDTO,
  type AccountUpdateInput,
  type WebAccountScriptActions,
} from "@/features/accounts/accounts-api";
import { WebAccountScriptsDialog } from "@/features/accounts/web-account-scripts";
import { WebAccountSettingsDialogs, WebAccountSettingsMenu, type WebAccountConfirmationAction, type WebAccountConfirmationTarget } from "@/features/accounts/web-account-settings";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";

const visibleWebQuotaModes = ["auto", "fast", "expert", "heavy"] as const;

const imagineQuotaModes = [
  { mode: "image_pro", labelKey: "imagineModeImagePro" },
  { mode: "image_edit", labelKey: "imagineModeImageEdit" },
  { mode: "video", labelKey: "imagineModeVideo" },
  { mode: "video_720p", labelKey: "imagineModeVideo720p" },
] as const;

const confirmationActions: readonly string[] = ["acceptTerms", "setBirthDate", "enableNSFW"] satisfies WebAccountConfirmationAction[];

// Grok Build and Console accounts filter by their link to a Grok Web account.
export function webAssociationFilter(t: TFunction): AccountFilterDef {
  return { id: "association", label: t("accounts.associationFilter"), options: [
    { value: "webLinked", label: t("accounts.associationWebLinked") },
    { value: "webUnlinked", label: t("accounts.associationWebUnlinked") },
  ] };
}

// Cloudflare cookies are kept per account for the browser-session providers (Web and Console).
export function CloudflareCookieFields({ account, form }: { account: AccountDTO; form: UseFormReturn<AccountEditForm> }) {
  const { t } = useTranslation();
  const clearCloudflareCookies = useWatch({ control: form.control, name: "clearCloudflareCookies" });
  return (
    <div className="space-y-2">
      <Label htmlFor="account-cloudflare-cookie">{t("settings.egress.cloudflareCookie")}</Label>
      <Textarea
        id="account-cloudflare-cookie"
        className="min-h-20 font-mono text-xs"
        autoComplete="new-password"
        spellCheck={false}
        disabled={clearCloudflareCookies}
        placeholder={account.cloudflareCookieConfigured ? t("settings.egress.keepConfigured") : "cf_clearance=..."}
        {...form.register("cloudflareCookies")}
      />
      {account.cloudflareCookieConfigured ? (
        <label className="flex items-center gap-2 text-xs text-muted-foreground">
          <Checkbox checked={clearCloudflareCookies} onCheckedChange={(checked) => form.setValue("clearCloudflareCookies", checked === true)} />
          {t("common.clear")}
        </label>
      ) : null}
      {form.formState.errors.cloudflareCookies ? <p className="text-xs text-destructive">{form.formState.errors.cloudflareCookies.message}</p> : null}
    </div>
  );
}

export function cloudflareCookieInput(values: AccountEditForm): Partial<AccountUpdateInput> {
  if (values.clearCloudflareCookies) return { clearCloudflareCookies: true };
  return values.cloudflareCookies.trim() ? { cloudflareCookies: values.cloudflareCookies } : {};
}

function WebAccountType({ account }: { account: AccountDTO }) {
  const { t } = useTranslation();
  const tier = account.webTier;
  const label = tier === "basic" ? t("accountType.free") : tier === "super" ? t("accountType.super") : tier === "heavy" ? t("accountType.heavy") : t("accountType.auto");
  return <AccountTypeText label={label} variant={tier === "basic" ? "free" : "default"} />;
}

function WebQuota({ account, locale }: { account: AccountDTO; locale: string }) {
  const { t } = useTranslation();
  const windows = account.quotaWindows ?? [];
  if (windows.length === 0) return <QuotaNotSynced />;
  const windowsByMode = new Map(windows.map((window) => [window.mode, window]));
  const imagineItems = imagineQuotaModes.flatMap(({ mode, labelKey }) => {
    const window = windowsByMode.get(mode);
    return window ? [{ label: t(`accounts.${labelKey}`), window }] : [];
  });
  const weekly = windowsByMode.get("weekly");
  const fast = windowsByMode.get("fast");

  const mainBlock = weekly ? <WeeklyWebQuota window={weekly} locale={locale} /> : account.webTier === "basic" && fast ? <QuotaModeBar mode="Fast" window={fast} locale={locale} /> : (
    <div className="grid w-full min-w-0 grid-cols-4 divide-x divide-border/70">
      {visibleWebQuotaModes.map((mode) => {
        const window = windowsByMode.get(mode);
        if (!window) {
          return <div key={mode} className="min-w-0 px-2 first:pl-0 last:pr-0"><div className="flex items-center justify-between gap-1 text-[11px]"><span className="truncate capitalize text-muted-foreground">{mode}</span><span className="text-muted-foreground">-</span></div><div className="mt-1.5 h-1.5 rounded-full bg-muted" /></div>;
        }
        return <QuotaModeBar key={mode} mode={mode.charAt(0).toUpperCase() + mode.slice(1)} window={window} locale={locale} compact />;
      })}
    </div>
  );

  if (imagineItems.length === 0) return mainBlock;
  return (
    <div className="grid w-full min-w-0 grid-cols-2 divide-x divide-border/70">
      <div className="min-w-0 pr-2">{mainBlock}</div>
      <div className="min-w-0 pl-2"><QuotaWindowRow items={imagineItems} locale={locale} /></div>
    </div>
  );
}

function WeeklyWebQuota({ window, locale }: { window: QuotaWindow; locale: string }) {
  const { t } = useTranslation();
  const usedPercent = Math.max(0, Math.min(100, window.usagePercent));
  const breakdown = (window.breakdown ?? []).filter((item) => item.usagePercent > 0);
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="block w-full min-w-0 text-left">
          <div className="flex items-center justify-between gap-2 text-[11px]">
            {breakdown.length > 0 ? <div className="flex min-w-0 items-center gap-2.5 overflow-hidden text-muted-foreground">{breakdown.slice(0, 3).map((item) => <span key={item.productCode} className="flex shrink-0 items-center gap-1"><span className={cn("size-1.5 rounded-full", quotaProductColor(item.productCode))} /><span>{quotaProductLabel(item.productCode, t)}</span><span className="tabular-nums text-foreground">{formatNumber(item.usagePercent, locale, 1)}%</span></span>)}{breakdown.length > 3 ? <span className="shrink-0">+{breakdown.length - 3}</span> : null}</div> : <span className="truncate text-muted-foreground">{t("accounts.weeklyQuota")}</span>}
            <span className="shrink-0 tabular-nums">{formatNumber(usedPercent, locale, 1)}%</span>
          </div>
          <div className="mt-1.5 flex h-1.5 overflow-hidden rounded-full bg-muted">{breakdown.length > 0 ? breakdown.map((item) => <div key={item.productCode} className={cn("h-full shrink-0", quotaProductColor(item.productCode))} style={{ width: `${Math.max(0, Math.min(100, item.usagePercent))}%` }} />) : <div className="h-full bg-primary" style={{ width: `${usedPercent}%` }} />}</div>
        </button>
      </TooltipTrigger>
      <TooltipContent>
        <div>{t("accounts.webWeeklyQuotaUsage", { remaining: formatNumber(100 - usedPercent, locale, 1) })}</div>
        <div className="text-muted-foreground">{window.resetAt ? t("accounts.quotaResetAt", { time: formatDateTime(window.resetAt, locale) }) : t("accounts.quotaResetUnknown")}</div>
        {breakdown.length > 0 ? <div className="mt-2 grid gap-1 border-t pt-2">{breakdown.map((item) => <div key={item.productCode} className="flex items-center justify-between gap-4"><span className="flex items-center gap-1.5"><span className={cn("size-2 rounded-full", quotaProductColor(item.productCode))} />{quotaProductLabel(item.productCode, t)}</span><span className="tabular-nums">{formatNumber(item.usagePercent, locale, 1)}%</span></div>)}</div> : null}
      </TooltipContent>
    </Tooltip>
  );
}

function quotaProductLabel(code: number, t: TFunction): string {
  const keys: Record<number, string> = { 0: "thirdParty", 1: "api", 2: "build", 3: "plugins", 4: "chat", 5: "imagine", 6: "voice" };
  const key = keys[code];
  return key ? t(`quotaProducts.${key}`) : t("quotaProducts.unknown", { code });
}

function quotaProductColor(code: number): string {
  const colors: Record<number, string> = { 0: "bg-quota-product-0", 1: "bg-quota-product-1", 2: "bg-quota-product-2", 3: "bg-quota-product-3", 4: "bg-quota-product-4", 5: "bg-quota-product-5", 6: "bg-quota-product-6" };
  return colors[code] ?? "bg-muted-foreground";
}

function RowMenuItems({ account, ctx }: { account: AccountDTO; ctx: AccountUIContext }) {
  const { t } = useTranslation();
  return (
    <>
      <DropdownMenuItem onClick={() => ctx.openTask({ kind: "convert", targets: [account.id] })}><ArrowRight />{t("accountConversion.action")}</DropdownMenuItem>
      <WebAccountSettingsMenu account={account} disabled={ctx.pending} onConfirm={(target) => ctx.openTask({ kind: target.action, targets: [account.id], account })} />
    </>
  );
}

function BulkActions({ targets, ctx }: { targets: string[] | "all"; ctx: AccountUIContext }) {
  const { t } = useTranslation();
  return (
    <>
      <Button variant="secondary" size="sm" disabled={ctx.pending} onClick={() => ctx.openTask({ kind: "convert", targets })}>{t("accountConversion.action")}</Button>
      <Button variant="secondary" size="sm" disabled={ctx.pending} onClick={() => ctx.openTask({ kind: "scripts", targets })}>{t("webAccountScripts.action")}</Button>
    </>
  );
}

function Dialogs({ ctx }: { ctx: AccountUIContext }) {
  const task = ctx.task;
  return (
    <>
      {task?.kind === "convert" ? <ConversionDialog targets={task.targets} ctx={ctx} /> : null}
      {task?.kind === "scripts" ? <ScriptsDialog targets={task.targets} ctx={ctx} /> : null}
      <ConfirmationDialogs ctx={ctx} />
    </>
  );
}

// Converts Web accounts into linked Grok Build accounts or syncs them to Grok Console.
function ConversionDialog({ targets, ctx }: { targets: string[] | "all"; ctx: AccountUIContext }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const abortRef = useRef<AbortController | null>(null);
  const [target, setTarget] = useState<"build" | "console">("build");
  const [strategy, setStrategy] = useState<AccountSyncStrategy>("missing");
  const [progress, setProgress] = useState<AccountTaskProgressDTO | null>(null);

  useEffect(() => () => abortRef.current?.abort(), []);

  const mutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: async () => {
      const controller = new AbortController();
      abortRef.current = controller;
      setProgress(null);
      const input = targets === "all" ? { all: true as const, strategy } : { ids: targets, strategy };
      if (target === "build") return t("accounts.conversionCompleted", await convertWebAccountsToBuild(input, setProgress, controller.signal));
      return t("webConsoleSync.completed", await syncWebAccountsToConsole(input, setProgress, controller.signal));
    },
    onSuccess: (message) => {
      ctx.closeTask();
      ctx.clearSelection();
      toast.success(message);
    },
    onError: (error) => { if (!isAbortError(error)) ctx.showError(error); },
    onSettled: () => {
      abortRef.current = null;
      ctx.invalidate();
      void queryClient.invalidateQueries({ queryKey: ["models"] });
    },
  });

  const pending = mutation.isPending;
  const progressKey = target === "build"
    ? progress?.phase === "syncing" ? "accounts.syncingProgress" : "accounts.convertingProgress"
    : progress?.phase === "syncing" ? "common.syncingProgress" : "common.importingProgress";
  return (
    <AlertDialog open onOpenChange={(open) => { if (!open) ctx.closeTask(); }}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t("accountConversion.title")}</AlertDialogTitle>
          <AlertDialogDescription>{t(targets === "all" ? "accountConversion.allDescription" : "accountConversion.selectedDescription", { count: targets === "all" ? 0 : targets.length })}</AlertDialogDescription>
        </AlertDialogHeader>
        <div className="space-y-2">
          <p id="web-conversion-target" className="text-xs font-medium">{t("accountConversion.target")}</p>
          <Tabs value={target} onValueChange={(value) => setTarget(value as "build" | "console")}>
            <TabsList aria-labelledby="web-conversion-target" className="grid h-10 w-full grid-cols-2 p-1">
              <TabsTrigger value="build" className="h-8 gap-2 font-normal" disabled={pending}><SquareTerminal className="text-quota-product-1" />Grok Build</TabsTrigger>
              <TabsTrigger value="console" className="h-8 gap-2 font-normal" disabled={pending}><Webhook className="text-quota-product-4" />Grok Console</TabsTrigger>
            </TabsList>
          </Tabs>
        </div>
        <div className="space-y-2">
          <p id="web-conversion-strategy" className="text-xs font-medium">{t("accountConversion.strategy")}</p>
          <Tabs value={strategy} onValueChange={(value) => setStrategy(value as AccountSyncStrategy)}>
            <TabsList aria-labelledby="web-conversion-strategy" className="grid h-10 w-full grid-cols-2 p-1">
              <TabsTrigger value="missing" className="h-8 font-normal" disabled={pending}>{t("accountConversion.missing")}</TabsTrigger>
              <TabsTrigger value="all" className="h-8 font-normal" disabled={pending}>{t("accountConversion.all")}</TabsTrigger>
            </TabsList>
          </Tabs>
          <p className="min-h-8 text-xs text-muted-foreground">{t(target === "build"
            ? strategy === "missing" ? "accountBulk.missingStrategyDescription" : "accountBulk.allStrategyDescription"
            : strategy === "missing" ? "webConsoleSync.missingStrategyDescription" : "webConsoleSync.allStrategyDescription")}</p>
        </div>
        <AlertDialogFooter>
          <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
          <AlertDialogAction disabled={pending || (Array.isArray(targets) && targets.length === 0)} onClick={(event) => { event.preventDefault(); mutation.mutate(); }}>
            {pending ? <><Spinner />{progress ? <span className="whitespace-nowrap tabular-nums">{t(progressKey, progress)}</span> : t("common.loading")}</> : t("accountConversion.start")}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function ScriptsDialog({ targets, ctx }: { targets: string[] | "all"; ctx: AccountUIContext }) {
  const { t } = useTranslation();
  const abortRef = useRef<AbortController | null>(null);
  const [progress, setProgress] = useState<AccountTaskProgressDTO | null>(null);

  useEffect(() => () => abortRef.current?.abort(), []);

  const mutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: (actions: WebAccountScriptActions) => {
      const controller = new AbortController();
      abortRef.current = controller;
      setProgress(null);
      return runWebAccountScripts(targets === "all" ? { all: true, actions } : { ids: targets, actions }, setProgress, controller.signal);
    },
    onSuccess: (result) => {
      ctx.closeTask();
      ctx.clearSelection();
      if (result.failed > 0) toast.warning(t("webAccountScripts.completedWithFailures", result));
      else toast.success(t("webAccountScripts.completed", result));
    },
    onError: (error) => { if (!isAbortError(error)) ctx.showError(error); },
    onSettled: () => {
      abortRef.current = null;
      ctx.invalidate();
    },
  });

  return <WebAccountScriptsDialog targets={targets} pending={mutation.isPending} progress={progress} onClose={ctx.closeTask} onRun={(actions) => mutation.mutate(actions)} />;
}

function ConfirmationDialogs({ ctx }: { ctx: AccountUIContext }) {
  const { t } = useTranslation();
  const task = ctx.task;
  const target: WebAccountConfirmationTarget | null = task?.account && confirmationActions.includes(task.kind)
    ? { account: task.account, action: task.kind as WebAccountConfirmationAction }
    : null;
  const mutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: ({ account, action }: WebAccountConfirmationTarget) => {
      if (action === "acceptTerms") return acceptWebAccountTerms(account.id);
      if (action === "setBirthDate") return setWebAccountBirthDate(account.id);
      return enableWebAccountNSFW(account.id);
    },
    onSuccess: (_, { action }) => {
      ctx.closeTask();
      toast.success(t(action === "acceptTerms" ? "webAccountSettings.termsAccepted" : action === "setBirthDate" ? "webAccountSettings.birthDateSaved" : "webAccountSettings.nsfwEnabled"));
    },
    onError: (error) => ctx.showError(error),
    onSettled: () => ctx.invalidate(),
  });
  return <WebAccountSettingsDialogs confirmationTarget={target} confirmationPending={mutation.isPending} onConfirmationClose={ctx.closeTask} onConfirm={(value) => mutation.mutate(value)} />;
}

export const grokWebAccountUI: Partial<ProviderAccountUI> = {
  TypeCell: WebAccountType,
  QuotaCell: WebQuota,
  importTexts: {
    quickImport: "accounts.quickImportSSO", file: "accounts.importWebFile", title: "accounts.quickImportTitle", description: "accounts.quickImportDescription",
    tokens: "accounts.ssoTokens", placeholder: "accounts.ssoTokenPlaceholder", filename: "grok-web-sso-tokens.txt",
  },
  syncAllDescription: "accounts.syncAllWebDescription",
  typeOptions: (t) => [
    { value: "auto", label: t("accountType.auto") },
    { value: "basic", label: t("accountType.free") },
    { value: "super", label: t("accountType.super") },
    { value: "heavy", label: t("accountType.heavy") },
  ],
  filters: (t) => [
    { id: "agreement", label: t("accounts.agreementFilter"), options: [
      { value: "nsfwEnabled", label: t("accounts.agreementNsfwEnabled") },
      { value: "nsfwDisabled", label: t("accounts.agreementNsfwDisabled") },
      { value: "termsAccepted", label: t("accounts.agreementTermsAccepted") },
      { value: "termsNotAccepted", label: t("accounts.agreementTermsNotAccepted") },
      { value: "allAccepted", label: t("accounts.agreementAllAccepted") },
      { value: "allNotAccepted", label: t("accounts.agreementAllNotAccepted") },
    ] },
    { id: "association", label: t("accounts.associationFilter"), options: [
      { value: "buildLinked", label: t("accounts.associationBuildLinked") },
      { value: "buildUnlinked", label: t("accounts.associationBuildUnlinked") },
      { value: "consoleLinked", label: t("accounts.associationConsoleLinked") },
      { value: "consoleUnlinked", label: t("accounts.associationConsoleUnlinked") },
      { value: "allLinked", label: t("accounts.associationAllLinked") },
      { value: "allUnlinked", label: t("accounts.associationAllUnlinked") },
    ] },
  ],
  linkedProviders: ["grok_build", "grok_console"],
  EditFields: CloudflareCookieFields,
  editInput: cloudflareCookieInput,
  RowMenuItems,
  BulkActions,
  Dialogs,
};
