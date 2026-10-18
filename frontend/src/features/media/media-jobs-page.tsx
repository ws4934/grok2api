import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertCircle, Ban, CheckCircle2, CircleHelp, Clock, Eye, Image as ImageIcon, ListChecks, Loader2, RefreshCw, RotateCcw, Search, Trash2, Video } from "lucide-react";
import type { LucideIcon } from "lucide-react";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useSearchParams } from "react-router-dom";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useProviders, type ProviderDTO } from "@/entities/provider/provider-api";
import { cancelMediaJob, deleteMediaJobs, getMediaJobStats, listMediaJobs, retryMediaJob } from "@/features/media/media-api";
import { mediaAssetPath, mediaJobActions, mediaJobStatuses, mediaProviders, type MediaJobDTO, type MediaJobStatsDTO, type MediaJobStatus, type MediaKind } from "@/features/media/media-model";
import { ProviderTag, VideoPreview } from "@/features/media/media-ui";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { PageHeader } from "@/shared/components/page-header";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

const statusTones: Record<MediaJobStatus, { dot: string; text: string; icon: LucideIcon }> = {
  queued: { dot: "bg-amber-500", text: "text-amber-700 dark:text-amber-300", icon: Clock },
  in_progress: { dot: "bg-sky-500", text: "text-sky-700 dark:text-sky-300", icon: Loader2 },
  completed: { dot: "bg-emerald-500", text: "text-emerald-700 dark:text-emerald-300", icon: CheckCircle2 },
  failed: { dot: "bg-red-500", text: "text-red-700 dark:text-red-300", icon: AlertCircle },
  canceled: { dot: "bg-zinc-400", text: "text-muted-foreground", icon: Ban },
  uncertain: { dot: "bg-orange-500", text: "text-orange-700 dark:text-orange-300", icon: CircleHelp },
};

const statsKeys: Record<MediaJobStatus, keyof MediaJobStatsDTO> = {
  queued: "queued", in_progress: "inProgress", completed: "completed", failed: "failed", canceled: "canceled", uncertain: "uncertain",
};

function parseKind(value: string | null): MediaKind | "" {
  return value === "image" || value === "video" ? value : "";
}

export function MediaJobsPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  // The gallery links here with ?kind=&search=<jobId>.
  const [searchParams] = useSearchParams();
  const providers = useProviders().data;
  const [kind, setKind] = useState<MediaKind | "">(() => parseKind(searchParams.get("kind")));
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState(() => searchParams.get("search") ?? "");
  const [statusFilter, setStatusFilter] = useState<MediaJobStatus | "">("");
  const [providerFilter, setProviderFilter] = useState("");
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [cancelTarget, setCancelTarget] = useState<MediaJobDTO | null>(null);
  const [viewing, setViewing] = useState<MediaJobDTO | null>(null);
  const [sort, setSort] = useState<TableSort>({ field: "createdAt", order: "desc" });
  const debouncedSearch = useDebouncedValue(search);
  const normalizedSearch = debouncedSearch.trim();

  const jobsQuery = useQuery({
    queryKey: ["media", "jobs", kind, page, pageSize, statusFilter, providerFilter, normalizedSearch, sort.field, sort.order],
    queryFn: () => listMediaJobs({ page, pageSize, kind, status: statusFilter, provider: providerFilter, search: normalizedSearch || undefined, sortBy: sort.field, sortOrder: sort.order }),
  });
  const statsQuery = useQuery({
    queryKey: ["media", "jobs", "stats", kind],
    queryFn: () => getMediaJobStats(kind),
    staleTime: 30_000,
  });

  const result = jobsQuery.data;
  const refreshing = jobsQuery.isFetching || statsQuery.isFetching;
  const pageIDs = result?.items.filter((job) => mediaJobActions(job).delete).map((job) => job.id) ?? [];
  const selectedOnPage = pageIDs.filter((id) => selected.has(id));
  const allPageSelected = pageIDs.length > 0 && selectedOnPage.length === pageIDs.length;

  function invalidate(): void {
    // Deleting video jobs also removes their gallery videos.
    void queryClient.invalidateQueries({ queryKey: ["media"] });
  }

  function failed(error: unknown): void {
    invalidate();
    toast.error(error instanceof Error ? error.message : t("errors.generic"));
  }

  const deleteMutation = useMutation({
    mutationFn: () => deleteMediaJobs([...selected]),
    onSuccess: (deleteResult) => {
      if (result && selectedOnPage.length === result.items.length && page > 1) setPage(page - 1);
      if (viewing && selected.has(viewing.id)) setViewing(null);
      setSelected(new Set());
      setDeleteOpen(false);
      invalidate();
      toast.success(t("media.jobs.deleted", { count: deleteResult.deleted }));
    },
    onError: failed,
  });
  const cancelMutation = useMutation({
    mutationFn: (id: string) => cancelMediaJob(id),
    onSuccess: (job) => {
      setCancelTarget(null);
      invalidate();
      // An already ended job comes back unchanged.
      if (job.status === "canceled") toast.success(t("media.jobs.canceled"));
      else toast.info(t("media.jobs.alreadyEnded", { status: t(`media.jobStatus.${job.status}`) }));
    },
    onError: (error) => {
      setCancelTarget(null);
      failed(error);
    },
  });
  const retryMutation = useMutation({
    mutationFn: (id: string) => retryMediaJob(id),
    onSuccess: (job) => {
      invalidate();
      toast.success(t("media.jobs.retried", { id: job.id }));
    },
    onError: failed,
  });

  function refreshAll(): void {
    void jobsQuery.refetch();
    void statsQuery.refetch();
  }

  function changeSort(field: string, initialOrder: SortOrder): void {
    setSort((current) => nextTableSort(current, field, initialOrder));
    setPage(1);
  }

  function togglePage(checked: boolean): void {
    setSelected((current) => {
      const next = new Set(current);
      for (const id of pageIDs) {
        if (checked) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  }

  function toggleJob(id: string, checked: boolean): void {
    setSelected((current) => {
      const next = new Set(current);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });
  }

  return (
    <div className="space-y-5">
      <PageHeader
        title={t("media.jobs.title")}
        description={t("media.jobs.description")}
        actions={(
          <>
            <Tabs value={kind || "all"} onValueChange={(value) => { setKind(parseKind(value)); setPage(1); }}>
              <TabsList aria-label={t("media.kindLabel")}>
                <TabsTrigger value="all" className="font-normal">{t("common.all")}</TabsTrigger>
                <TabsTrigger value="image" className="font-normal">{t("media.kind.image")}</TabsTrigger>
                <TabsTrigger value="video" className="font-normal">{t("media.kind.video")}</TabsTrigger>
              </TabsList>
            </Tabs>
            <Button variant="secondary" size="sm" onClick={refreshAll} disabled={refreshing}>
              <RefreshCw className={refreshing ? "animate-spin" : undefined} />
              {t("common.refresh")}
            </Button>
          </>
        )}
      />

      <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-72 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input
                  className="h-8 pl-9 text-xs"
                  value={search}
                  onChange={(event) => { setSearch(event.target.value); setPage(1); }}
                  placeholder={t("media.jobs.search")}
                  aria-label={t("media.jobs.search")}
                />
              </div>
              <DataTableFilters filters={[
                {
                  id: "status",
                  label: t("media.jobs.status"),
                  value: statusFilter,
                  onChange: (value) => { setStatusFilter(value as MediaJobStatus | ""); setPage(1); },
                  options: mediaJobStatuses.map((status) => ({ value: status, label: t(`media.jobStatus.${status}`) })),
                },
                {
                  id: "provider",
                  label: t("media.provider"),
                  value: providerFilter,
                  onChange: (value) => { setProviderFilter(value); setPage(1); },
                  options: mediaProviders(providers).map((provider) => ({ value: provider.id, label: provider.name })),
                },
              ]} />
              {(normalizedSearch || statusFilter || providerFilter) && result ? <span className="hidden whitespace-nowrap text-xs tabular-nums text-muted-foreground md:inline">{t("media.jobs.pageSummary", { count: result.items.length, total: result.total })}</span> : null}
            </div>
            {selected.size > 0 ? (
              <div className="flex h-8 items-center gap-2">
                <span className="text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
                <Button variant="secondary" size="sm" className="text-destructive hover:text-destructive" onClick={() => setDeleteOpen(true)}><Trash2 />{t("common.delete")}</Button>
              </div>
            ) : <JobSummary stats={statsQuery.data} loading={statsQuery.isPending} unavailable={statsQuery.isError} locale={i18n.language} />}
          </>
        )}
        footer={result && result.total > 0 ? (
          <Pagination
            page={result.page}
            pageSize={result.pageSize}
            total={result.total}
            onPageChange={setPage}
            onPageSizeChange={(value) => { setPageSize(value); setPage(1); }}
          />
        ) : undefined}
      >
        {jobsQuery.isError ? <ErrorState message={jobsQuery.error.message} onRetry={() => void jobsQuery.refetch()} /> : null}
        {result && result.items.length === 0 ? <EmptyState message={t("media.jobs.empty")} /> : null}
        {jobsQuery.isPending || (result && result.items.length > 0) ? (
          <Table viewportRows={20} rowHeight={72} className="min-w-[1176px] table-fixed text-xs">
            <colgroup>
              <col className="w-10" />
              <col className="w-64" />
              <col className="w-44" />
              <col className="w-40" />
              <col className="w-28" />
              <col className="w-40" />
              <col className="w-44" />
              <col className="w-24" />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead><Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} disabled={pageIDs.length === 0} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} /></TableHead>
                <SortableTableHead field="prompt" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.prompt")}</SortableTableHead>
                <SortableTableHead field="model" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.model")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.jobs.statusProgress")}</SortableTableHead>
                <SortableTableHead field="spec" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.jobs.spec")}</SortableTableHead>
                <SortableTableHead field="account" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.jobs.owner")}</SortableTableHead>
                <SortableTableHead field="createdAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("media.jobs.time")}</SortableTableHead>
                <TableActionHead />
              </TableRow>
            </TableHeader>
            {jobsQuery.isPending ? (
              <TableBody><TableLoadingRow colSpan={8} /></TableBody>
            ) : (
              <VirtualTableBody items={result?.items ?? []} colSpan={8} rowHeight={72} renderRow={(job) => {
                const actions = mediaJobActions(job);
                const KindIcon = job.kind === "video" ? Video : ImageIcon;
                return (
                  <TableRow className="group h-[72px]" data-state={selected.has(job.id) ? "selected" : undefined} key={job.id}>
                    <TableCell>
                      <Checkbox checked={selected.has(job.id)} disabled={!actions.delete} onCheckedChange={(checked) => toggleJob(job.id, checked === true)} aria-label={t("common.selectItem", { name: job.id })} />
                    </TableCell>
                    <TableCell className="min-w-0">
                      <div className="min-w-0">
                        <span className="block truncate text-xs font-medium" title={job.prompt}>{job.prompt || "-"}</span>
                        <span className="mt-0.5 flex min-w-0 items-center gap-1 text-[10px] text-muted-foreground">
                          <KindIcon className="size-3 shrink-0" aria-label={t(`media.kind.${job.kind}`)} />
                          <span className="truncate font-mono" title={job.id}>{job.id}</span>
                        </span>
                      </div>
                    </TableCell>
                    <TableCell className="min-w-0">
                      <div className="min-w-0 space-y-0.5">
                        <span className="block truncate" title={job.model}>{job.model || "-"}</span>
                        <span className="block text-[11px] text-muted-foreground"><ProviderTag id={job.provider} providers={providers} short /></span>
                      </div>
                    </TableCell>
                    <TableCell><JobProgress job={job} locale={i18n.language} /></TableCell>
                    <TableCell>
                      <div className="space-y-0.5 text-xs">
                        <span className="block truncate" title={formatSpec(job)}>{formatSpec(job)}</span>
                        <span className="block text-[11px] text-muted-foreground">{job.kind === "video" ? t("media.jobs.seconds", { count: job.seconds }) : t("media.jobs.images", { count: job.itemCount })}</span>
                      </div>
                    </TableCell>
                    <TableCell className="min-w-0">
                      <div className="min-w-0 space-y-0.5">
                        <span className="block truncate" title={job.accountName}>{job.accountName || "-"}</span>
                        <span className="block truncate text-[11px] text-muted-foreground" title={job.clientKeyName}>{job.clientKeyName || "-"}</span>
                      </div>
                    </TableCell>
                    <TableCell><JobTimes job={job} locale={i18n.language} /></TableCell>
                    <TableActionCell>
                      <div className="flex items-center justify-end gap-0.5">
                        {actions.retry ? <Button type="button" variant="ghost" size="icon" className="size-8" disabled={retryMutation.isPending} title={t("media.jobs.retry")} aria-label={t("media.jobs.retry")} onClick={() => retryMutation.mutate(job.id)}><RotateCcw /></Button> : null}
                        {actions.cancel ? <Button type="button" variant="ghost" size="icon" className="size-8 text-destructive hover:text-destructive" title={t("media.jobs.cancel")} aria-label={t("media.jobs.cancel")} onClick={() => setCancelTarget(job)}><Ban /></Button> : null}
                        <Button type="button" variant="ghost" size="icon" className="size-8" title={t("media.jobs.details")} aria-label={t("media.jobs.details")} onClick={() => setViewing(job)}><Eye /></Button>
                      </div>
                    </TableActionCell>
                  </TableRow>
                );
              }} />
            )}
          </Table>
        ) : null}
      </DataTableShell>

      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("media.jobs.deleteTitle", { count: selected.size })}</AlertDialogTitle>
            <AlertDialogDescription>{t("media.jobs.deleteDescription")}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={deleteMutation.isPending} onClick={() => deleteMutation.mutate()}>
              {deleteMutation.isPending ? <Spinner /> : <Trash2 />}
              {t("common.delete")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={Boolean(cancelTarget)} onOpenChange={(open) => !open && setCancelTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("media.jobs.cancelTitle")}</AlertDialogTitle>
            <AlertDialogDescription>{t("media.jobs.cancelDescription")}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.close")}</AlertDialogCancel>
            <AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={cancelMutation.isPending} onClick={(event) => { event.preventDefault(); if (cancelTarget) cancelMutation.mutate(cancelTarget.id); }}>
              {cancelMutation.isPending ? <Spinner /> : <Ban />}
              {t("media.jobs.cancel")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog open={Boolean(viewing)} onOpenChange={(open) => !open && setViewing(null)}>
        <DialogContent className="max-h-[calc(100vh-2rem)] max-w-4xl overflow-y-auto">
          <DialogHeader className="min-w-0 pr-8">
            <DialogTitle>{t("media.jobs.details")}</DialogTitle>
            <DialogDescription className="min-w-0 truncate font-mono" title={viewing?.id}>{viewing?.id}</DialogDescription>
          </DialogHeader>
          {viewing ? <JobDetails job={viewing} providers={providers} locale={i18n.language} /> : null}
        </DialogContent>
      </Dialog>
    </div>
  );
}

function JobDetails({ job, providers, locale }: { job: MediaJobDTO; providers?: ProviderDTO[]; locale: string }) {
  const { t } = useTranslation();
  const fields: Array<[string, ReactNode]> = [
    [t("media.jobs.status"), <JobStatus key="status" status={job.status} />],
    [t("media.provider"), <ProviderTag key="provider" id={job.provider} providers={providers} />],
    [t("media.model"), job.model || "-"],
    [t("media.operation"), `${t(`media.kind.${job.kind}`)} · ${job.operation ? t(`media.jobOperation.${job.operation}`, { defaultValue: job.operation }) : "-"}`],
    [t("media.jobs.spec"), `${formatSpec(job)} · ${job.kind === "video" ? t("media.jobs.seconds", { count: job.seconds }) : t("media.jobs.images", { count: job.itemCount })}`],
    [t("media.account"), job.accountName || "-"],
    [t("media.key"), job.clientKeyName || "-"],
    [t("media.createdAt"), formatDateTime(job.createdAt, locale)],
    [t("media.jobs.completedAt"), formatDateTime(job.completedAt, locale)],
  ];
  return (
    <div className="min-w-0 space-y-4 text-xs">
      {job.errorCode || job.errorMessage ? (
        <div className="rounded-md bg-destructive/10 px-3 py-2 text-destructive">
          {job.errorCode ? <p className="font-mono text-[11px]">{job.errorCode}</p> : null}
          {job.errorMessage ? <p className="mt-0.5 whitespace-pre-wrap break-words">{job.errorMessage}</p> : null}
        </div>
      ) : null}
      <dl className="grid grid-cols-[6rem_minmax(0,1fr)] gap-x-3 gap-y-2 sm:grid-cols-[6rem_minmax(0,1fr)_6rem_minmax(0,1fr)]">
        {fields.map(([label, value]) => (
          <div className="contents" key={label}>
            <dt className="text-muted-foreground">{label}</dt>
            <dd className="min-w-0 truncate">{value}</dd>
          </div>
        ))}
      </dl>
      <div className="space-y-1">
        <p className="text-muted-foreground">{t("media.prompt")}</p>
        <p className="max-h-32 overflow-y-auto whitespace-pre-wrap break-words rounded-md bg-muted/50 px-3 py-2">{job.prompt || "-"}</p>
      </div>
      <div className="space-y-2">
        <p className="text-muted-foreground">{t("media.jobs.results")}</p>
        {job.assetIds.length === 0 ? <p className="text-muted-foreground">{t("media.jobs.noResults")}</p> : job.kind === "video" ? (
          <VideoPreview key={job.assetIds[0]} assetId={job.assetIds[0]} />
        ) : (
          <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
            {job.assetIds.map((id, index) => (
              <a key={id} href={mediaAssetPath("image", id)} target="_blank" rel="noreferrer" className="block aspect-square overflow-hidden rounded-md bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40" aria-label={t("media.jobs.openResult", { index: index + 1 })}>
                <img src={mediaAssetPath("image", id)} alt={id} loading="lazy" decoding="async" className="size-full object-cover" />
              </a>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function JobSummary({ stats, loading, unavailable, locale }: { stats?: MediaJobStatsDTO; loading: boolean; unavailable: boolean; locale: string }) {
  const { t } = useTranslation();
  if (loading) return <div className="flex h-8 items-center"><Spinner className="size-3.5" /></div>;
  const value = (count: number | undefined) => unavailable ? "-" : formatNumber(count ?? 0, locale, 0);
  return (
    <div className="flex h-8 w-full items-center gap-4 overflow-x-auto whitespace-nowrap text-xs sm:w-auto">
      <SummaryItem icon={ListChecks} label={t("media.jobs.totalJobs")} value={value(stats?.totalJobs)} tone="text-muted-foreground" />
      <span className="h-3 w-px shrink-0 bg-border" aria-hidden="true" />
      {mediaJobStatuses.map((status) => (
        <SummaryItem key={status} icon={statusTones[status].icon} label={t(`media.jobStatus.${status}`)} value={value(stats?.[statsKeys[status]])} tone={statusTones[status].text} />
      ))}
    </div>
  );
}

function SummaryItem({ icon: Icon, label, value, tone }: { icon: LucideIcon; label: string; value: string; tone: string }) {
  return (
    <span className="inline-flex shrink-0 items-center gap-1.5">
      <Icon className={cn("size-3.5", tone)} />
      <span className="text-muted-foreground">{label}</span>
      <strong className="font-medium tabular-nums">{value}</strong>
    </span>
  );
}

function JobStatus({ status }: { status: MediaJobStatus }) {
  const { t } = useTranslation();
  const tone = statusTones[status];
  return (
    <span className={cn("inline-flex items-center gap-1.5 whitespace-nowrap text-xs", tone.text)}>
      <span className={cn("size-1.5 rounded-full", tone.dot)} />
      {t(`media.jobStatus.${status}`)}
    </span>
  );
}

function JobProgress({ job, locale }: { job: MediaJobDTO; locale: string }) {
  const normalized = Math.max(0, Math.min(100, job.progress));
  const error = [job.errorCode, job.errorMessage].filter(Boolean).join(": ");
  return (
    <div className="w-32 space-y-2">
      <div className="flex items-center justify-between gap-2">
        {error ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="inline-flex cursor-help" tabIndex={0}><JobStatus status={job.status} /></span>
            </TooltipTrigger>
            <TooltipContent side="top" className="max-w-80 whitespace-normal break-words text-left leading-relaxed">{error}</TooltipContent>
          </Tooltip>
        ) : <JobStatus status={job.status} />}
        <span className="shrink-0 text-[11px] tabular-nums text-muted-foreground">{formatNumber(normalized, locale, 0)}%</span>
      </div>
      <span className="block h-1 w-full overflow-hidden rounded-full bg-muted">
        <span className={cn("block h-full rounded-full", statusTones[job.status].dot)} style={{ width: `${normalized}%` }} />
      </span>
    </div>
  );
}

function JobTimes({ job, locale }: { job: MediaJobDTO; locale: string }) {
  const { t } = useTranslation();
  return (
    <div className="space-y-1 whitespace-nowrap text-[11px]">
      <div className="flex items-center gap-1.5"><span className="w-7 text-muted-foreground">{t("media.jobs.createdShort")}</span><span>{formatDateTime(job.createdAt, locale)}</span></div>
      <div className="flex items-center gap-1.5"><span className="w-7 text-muted-foreground">{t("media.jobs.completedShort")}</span><span className={job.completedAt ? undefined : "text-muted-foreground"}>{formatDateTime(job.completedAt, locale)}</span></div>
    </div>
  );
}

function formatSpec(job: MediaJobDTO): string {
  return [job.size, job.quality].filter(Boolean).join(" · ") || "-";
}
