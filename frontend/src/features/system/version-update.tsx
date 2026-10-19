import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowUpRight, Download, Info, RefreshCw, RotateCcw } from "lucide-react";
import { type ReactNode, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { applyUpdate, checkForUpdates, getVersionInfo, rollbackUpdate } from "@/entities/system/system-api";
import { waitForVersion } from "@/entities/system/version-model";
import { cn } from "@/shared/lib/cn";
import { formatDateTime } from "@/shared/lib/format";

const versionQueryKey = ["system-version"] as const;

function useVersionInfo() {
  return useQuery({
    queryKey: versionQueryKey,
    queryFn: getVersionInfo,
    staleTime: 60_000,
    retry: 1,
  });
}

function useCheckForUpdates() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: checkForUpdates,
    onSuccess: (value) => queryClient.setQueryData(versionQueryKey, value),
  });
}

type UpdateAction = "update" | "rollback";
type UpdateOutcome = { action: UpdateAction; version: string; phase: "restarting" | "timeout" | "manual" };

function useUpdateAction() {
  const [outcome, setOutcome] = useState<UpdateOutcome | null>(null);
  const mutation = useMutation({
    mutationFn: (action: UpdateAction) => (action === "update" ? applyUpdate() : rollbackUpdate()),
    onSuccess: ({ version, restarting }, action) => {
      setOutcome({ action, version, phase: restarting ? "restarting" : "manual" });
      if (!restarting) return;
      void waitForVersion(version, async () => (await getVersionInfo()).currentVersion).then((ready) => {
        if (ready) window.location.reload();
        else setOutcome({ action, version, phase: "timeout" });
      });
    },
  });
  return { mutation, outcome };
}

export function CurrentVersionLabel() {
  const versionQuery = useVersionInfo();
  const version = versionQuery.data?.currentVersion;
  if (!version) return null;
  return <span className="font-mono text-[10px] font-normal text-muted-foreground">{version}</span>;
}

export function VersionUpdateBanner() {
  const { t } = useTranslation();
  const versionQuery = useVersionInfo();
  const checkMutation = useCheckForUpdates();
  const version = versionQuery.data;
  if (!version?.updateAvailable) return null;

  return (
    <section className="flex flex-col gap-3 rounded-lg bg-amber-500/10 px-4 py-3 sm:flex-row sm:items-center sm:justify-between">
      <div className="min-w-0">
        <p className="text-sm font-medium">{t("updates.available", { version: version.latestVersion })}</p>
        <p className="mt-0.5 text-xs text-muted-foreground">{t("updates.currentSummary", { version: version.currentVersion })}</p>
      </div>
      <div className="flex shrink-0 items-center gap-0.5">
        {version.canUpdate ? (
          <>
            <Button variant="ghost" size="sm" className="h-7 px-2.5 text-xs" asChild>
              <Link to="/settings?tab=about"><Download className="size-3.5" />{t("updates.updateNow")}</Link>
            </Button>
            <span className="mx-1 h-3 w-px bg-border/70" />
          </>
        ) : null}
        {version.releaseUrl ? (
          <Button variant="ghost" size="sm" className="h-7 px-2.5 text-xs font-normal text-muted-foreground hover:text-foreground" asChild>
            <a href={version.releaseUrl} target="_blank" rel="noreferrer">{t("updates.viewRelease")}<ArrowUpRight className="size-3.5" /></a>
          </Button>
        ) : null}
        {version.releaseUrl ? <span className="mx-1 h-3 w-px bg-border/70" /> : null}
        <Button variant="ghost" size="sm" className="h-7 px-2.5 text-xs font-normal text-muted-foreground hover:text-foreground" disabled={checkMutation.isPending} onClick={() => checkMutation.mutate()}>
          {checkMutation.isPending ? <Spinner /> : <RefreshCw className="size-3.5" />}{t("updates.checkNow")}
        </Button>
      </div>
    </section>
  );
}

export function VersionUpdateSection() {
  const { t, i18n } = useTranslation();
  const versionQuery = useVersionInfo();
  const checkMutation = useCheckForUpdates();
  const { mutation: updateMutation, outcome } = useUpdateAction();
  const [confirming, setConfirming] = useState<UpdateAction | null>(null);
  const version = versionQuery.data;
  const requestError = versionQuery.error instanceof Error ? versionQuery.error.message : "";
  const checkError = checkMutation.error instanceof Error ? checkMutation.error.message : "";
  const updateError = updateMutation.error instanceof Error ? updateMutation.error.message : "";
  const error = updateError || version?.error || checkError || requestError;
  // Locked until reload: the running binary has changed or is being replaced.
  const locked = updateMutation.isPending || outcome !== null;
  const pendingAction = updateMutation.isPending ? updateMutation.variables : null;

  return (
    <div className="w-full space-y-8">
      <section className="space-y-3">
        <div className="flex min-h-8 items-center px-1">
          <h2 className="text-sm font-medium tracking-tight">{t("updates.title")}</h2>
        </div>
        <div className="flex items-start gap-3 rounded-md bg-amber-500/10 px-4 py-3">
          <Info className="mt-0.5 size-4 shrink-0 text-amber-700 dark:text-amber-300" />
          <div className="min-w-0">
            <p className="text-xs font-medium">{t("updates.noteTitle")}</p>
            <p className="mt-1 text-xs leading-5 text-muted-foreground">{t("updates.noteDescription")}</p>
          </div>
        </div>
        <div className="space-y-0">
          <VersionField label={t("updates.currentVersion")} description={t("updates.currentVersionHelp")}>
            <div className="flex min-w-0 items-center gap-2">
              <div className="min-w-0 flex-1"><VersionValue>{version?.currentVersion || "-"}</VersionValue></div>
              <Button type="button" variant="secondary" size="sm" className="shrink-0" disabled={versionQuery.isPending || checkMutation.isPending || locked} onClick={() => checkMutation.mutate()}>
                {versionQuery.isPending || checkMutation.isPending ? <Spinner /> : <RefreshCw />}{t("updates.checkNow")}
              </Button>
            </div>
          </VersionField>
          <VersionField label={t("updates.latestVersion")} description={t("updates.latestVersionHelp")}>
            <div className="flex min-w-0 items-center gap-2">
              <div className="min-w-0 flex-1"><VersionValue>{version?.latestVersion || t("updates.notChecked")}</VersionValue></div>
              {version?.updateAvailable && version.canUpdate ? (
                <Button type="button" size="sm" className="shrink-0" disabled={locked} onClick={() => setConfirming("update")}>
                  {pendingAction === "update" ? <Spinner /> : <Download />}{pendingAction === "update" ? t("updates.applying") : t("updates.updateNow")}
                </Button>
              ) : null}
            </div>
          </VersionField>
          <VersionField label={t("updates.statusLabel")} description={t("updates.statusLabelHelp")}>
            <VersionValue>
              {version?.status ? <span className={cn("size-1.5 shrink-0 rounded-full bg-muted-foreground", version.status === "up_to_date" && "bg-emerald-500", version.status === "update_available" && "bg-amber-500", version.status === "check_failed" && "bg-destructive")} /> : null}
              <span>{version ? t(`updates.status.${version.status}`) : t("common.loading")}</span>
            </VersionValue>
            {version?.updateAvailable && !version.canUpdate && version.updateBlockedReason ? <p className="mt-1.5 px-1 text-xs leading-5 text-muted-foreground">{version.updateBlockedReason}</p> : null}
          </VersionField>
          <VersionField label={t("updates.checkedAt")} description={t("updates.checkedAtHelp")}>
            <VersionValue>{version?.checkedAt ? formatDateTime(version.checkedAt, i18n.language) : t("updates.neverChecked")}</VersionValue>
          </VersionField>
          {version?.rollbackAvailable ? (
            <VersionField label={t("updates.rollback")} description={t("updates.rollbackHelp")}>
              <Button type="button" variant="secondary" size="sm" disabled={locked} onClick={() => setConfirming("rollback")}>
                {pendingAction === "rollback" ? <Spinner /> : <RotateCcw />}{pendingAction === "rollback" ? t("updates.rollingBack") : t("updates.rollbackNow")}
              </Button>
            </VersionField>
          ) : null}
        </div>
        {outcome ? (
          <div className="flex items-center gap-2 rounded-md bg-secondary/35 px-4 py-3 text-xs leading-5">
            {outcome.phase === "restarting" ? <Spinner /> : null}
            <span>
              {outcome.phase === "restarting"
                ? t("updates.restarting", { version: outcome.version })
                : outcome.phase === "timeout"
                  ? t("updates.restartTimeout")
                  : outcome.action === "update"
                    ? t("updates.updatedManualRestart", { version: outcome.version })
                    : t("updates.rolledBackManualRestart", { version: outcome.version })}
            </span>
          </div>
        ) : null}
        {error ? <p className="text-xs leading-5 text-destructive">{error}</p> : null}
        <AlertDialog open={confirming !== null} onOpenChange={(open) => { if (!open) setConfirming(null); }}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{confirming === "rollback" ? t("updates.rollbackConfirmTitle") : t("updates.updateConfirmTitle", { version: version?.latestVersion })}</AlertDialogTitle>
              <AlertDialogDescription>
                {confirming === "rollback" ? t("updates.rollbackConfirmDescription") : t("updates.updateConfirmDescription", { version: version?.latestVersion })}
                <span className="mt-1 block">{version?.canRestart ? t("updates.restartNote") : t("updates.manualRestartNote")}</span>
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
              <AlertDialogAction onClick={() => confirming && updateMutation.mutate(confirming)}>
                {confirming === "rollback" ? t("updates.rollbackNow") : t("updates.updateNow")}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </section>

      {version?.releaseNotes || version?.releaseUrl ? (
        <section className="space-y-3">
          <div className="flex min-h-8 items-center justify-between gap-3 px-1">
            <div>
              <h3 className="text-sm font-medium tracking-tight">{t("updates.releaseNotes")}</h3>
              <p className="mt-1 text-xs leading-5 text-muted-foreground">{t("updates.releaseNotesHelp")}</p>
            </div>
            {version.releaseUrl ? (
              <Button type="button" variant="secondary" size="sm" asChild>
                <a href={version.releaseUrl} target="_blank" rel="noreferrer">{t("updates.openRelease")}<ArrowUpRight /></a>
              </Button>
            ) : null}
          </div>
          <div className="min-w-0 rounded-md bg-secondary/35 px-4 py-3">
            <p className="whitespace-pre-wrap break-words text-xs leading-5 text-muted-foreground">
              {version.releaseNotes || t("updates.noReleaseNotes")}
            </p>
          </div>
        </section>
      ) : null}
    </div>
  );
}

function VersionField({ label, description, children }: { label: string; description: string; children: ReactNode }) {
  return (
    <div className="min-w-0 py-4">
      <div className="grid min-w-0 gap-2.5 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)] sm:items-center sm:gap-8">
        <div className="min-w-0">
          <p className="text-xs font-medium">{label}</p>
          <p className="mt-1 max-w-xl text-xs leading-5 text-muted-foreground">{description}</p>
        </div>
        <div className="min-w-0">{children}</div>
      </div>
    </div>
  );
}

function VersionValue({ children }: { children: ReactNode }) {
  return <div className="flex min-h-8 min-w-0 items-center gap-2 rounded-md bg-secondary/55 px-3 py-1 text-xs font-medium">{children}</div>;
}
