import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, ChevronLeft, ChevronRight, Copy, Pause, Play, RefreshCw, Search } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { useSearchParams } from "react-router-dom";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useProviders } from "@/entities/provider/provider-api";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { PageHeader } from "@/shared/components/page-header";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { formatCompactDateTime, formatNumber } from "@/shared/lib/format";
import { getSystemLogs } from "./system-logs-api";
import { logReason, logSummary, type LogFilters, type SystemLog } from "./system-logs-model";

export function SystemLogsPage() {
  const { t, i18n } = useTranslation();
  const [params, setParams] = useSearchParams();
  const filters = useMemo<LogFilters>(() => ({ search: params.get("search") ?? "", level: params.get("level") ?? "", provider: params.get("provider") ?? "", accountId: params.get("accountId") ?? "", requestId: params.get("requestId") ?? "" }), [params]);
  const debounced = useDebouncedValue(filters);
  const scope = JSON.stringify(debounced);
  const [pagination, setPagination] = useState({ scope, cursors: [""] });
  if (pagination.scope !== scope) setPagination({ scope, cursors: [""] });
  const cursors = pagination.scope === scope ? pagination.cursors : [""];
  const cursor = cursors[cursors.length - 1];
  const [live, setLive] = useState(true);
  const [selected, setSelected] = useState<SystemLog | null>(null);
  const providers = useProviders().data ?? [];
  const query = useQuery({
    queryKey: ["system-logs", scope, cursor],
    queryFn: ({ signal }) => getSystemLogs(debounced, cursor, signal),
    refetchInterval: live && !cursor && !selected ? 5000 : false,
    gcTime: 60_000,
  });
  const status = query.data?.status;
  const change = (key: keyof LogFilters, value: string) => setParams((current) => {
    const next = new URLSearchParams(current); if (value) next.set(key, value); else next.delete(key); return next;
  }, { replace: true });
  const eventLabel = (entry: SystemLog) => t(`systemLogs.events.${entry.message}`, { defaultValue: entry.message });
  const providerLabel = (value: string) => providers.find((provider) => provider.id === value)?.name ?? value;

  async function copy(entry: SystemLog) {
    try { await navigator.clipboard.writeText(JSON.stringify(entry, null, 2)); toast.success(t("common.copied")); }
    catch { toast.error(t("systemLogs.copyFailed")); }
  }

  return <div className="space-y-6">
    <PageHeader title={t("systemLogs.title")} description={t("systemLogs.description")} actions={<>
      <Button variant="secondary" size="sm" onClick={() => setLive((current) => !current)} aria-pressed={live}>
        {live ? <Pause /> : <Play />}{t(live ? "systemLogs.pause" : "systemLogs.resume")}
      </Button>
      <Button variant="secondary" size="sm" onClick={() => { if (cursor) setPagination({ scope, cursors: [""] }); else void query.refetch(); }} disabled={query.isFetching}>
        <RefreshCw className={cn(query.isFetching && "animate-spin")} />{t("common.refresh")}
      </Button>
    </>} />
    <div className="flex flex-wrap items-center justify-between gap-3 text-sm text-muted-foreground">
      <p>{t("systemLogs.retention", { days: status?.retentionDays ?? 7, count: formatNumber(status?.maxEntries ?? 10000, i18n.language, 0) })}</p>
      <span className="flex items-center gap-2"><span className={cn("size-1.5 rounded-full", live && !cursor && !selected ? "bg-emerald-500" : "bg-muted-foreground/50")} />
        {t(!live ? "systemLogs.paused" : cursor ? "systemLogs.historyPaused" : selected ? "systemLogs.detailPaused" : "systemLogs.live")}
      </span>
    </div>
    {status && (status.dropped > 0 || status.writeFailures > 0) && <div role="status" className="flex gap-3 rounded-lg bg-amber-500/10 p-4 text-sm text-amber-800 dark:text-amber-300">
      <AlertTriangle className="mt-0.5 size-4 shrink-0" /><div className="min-w-0 space-y-1">
        <p>{t("systemLogs.dropped", { count: status.dropped, failures: status.writeFailures })}</p>
        {status.lastError && <p className="break-words font-mono text-xs">{formatCompactDateTime(status.lastFailureAt, i18n.language)} · {status.lastError}</p>}
      </div>
    </div>}
    <DataTableShell toolbar={<div className="flex w-full flex-wrap gap-3">
      <div className="relative min-w-56 flex-1"><Search className="pointer-events-none absolute left-3 top-2.5 size-4 text-muted-foreground" /><Input aria-label={t("systemLogs.search")} placeholder={t("systemLogs.search")} value={filters.search} maxLength={200} onChange={(event) => change("search", event.target.value)} className="bg-secondary/50 pl-9" /></div>
      <Select value={filters.level || "all"} onValueChange={(value) => change("level", value === "all" ? "" : value)}>
        <SelectTrigger className="w-32 bg-secondary/50" aria-label={t("systemLogs.level")}><SelectValue /></SelectTrigger>
        <SelectContent>{["all", "ERROR", "WARN", "INFO"].map((level) => <SelectItem key={level} value={level}>{t(`systemLogs.levels.${level}`)}</SelectItem>)}</SelectContent>
      </Select>
      <Select value={filters.provider || "all"} onValueChange={(value) => change("provider", value === "all" ? "" : value)}>
        <SelectTrigger className="w-40 bg-secondary/50" aria-label={t("systemLogs.provider")}><SelectValue /></SelectTrigger>
        <SelectContent><SelectItem value="all">{t("systemLogs.allProviders")}</SelectItem>{providers.map((provider) => <SelectItem key={provider.id} value={provider.id}>{provider.name}</SelectItem>)}</SelectContent>
      </Select>
      <Input className="w-32 bg-secondary/50" aria-label={t("systemLogs.accountId")} placeholder={t("systemLogs.accountId")} value={filters.accountId} inputMode="numeric" maxLength={18} onChange={(event) => change("accountId", event.target.value.replace(/\D/g, ""))} />
      <Input className="w-44 bg-secondary/50" aria-label={t("systemLogs.requestId")} placeholder={t("systemLogs.requestId")} value={filters.requestId} maxLength={64} onChange={(event) => change("requestId", event.target.value)} />
      {Object.values(filters).some(Boolean) && <Button variant="ghost" size="sm" onClick={() => setParams({}, { replace: true })}>{t("systemLogs.reset")}</Button>}
    </div>} footer={<div className="flex w-full flex-wrap items-center justify-between gap-3 text-sm text-muted-foreground">
      <div className="flex items-center gap-2"><Button variant="ghost" size="icon" disabled={cursors.length === 1 || query.isFetching} aria-label={t("common.previousPage")} onClick={() => setPagination({ scope, cursors: cursors.slice(0, -1) })}><ChevronLeft /></Button>
        <span>{t("systemLogs.page", { page: cursors.length })}</span>
        <Button variant="ghost" size="icon" disabled={!query.data?.nextCursor || query.isFetching} aria-label={t("common.nextPage")} onClick={() => setPagination({ scope, cursors: [...cursors, query.data!.nextCursor] })}><ChevronRight /></Button>
      </div>
      <span>{t("systemLogs.savedAt", { time: formatCompactDateTime(status?.lastSavedAt, i18n.language) })}</span>
    </div>}>
      {query.isError ? <ErrorState message={query.error.message} onRetry={() => void query.refetch()} /> : <Table>
        <TableHeader><TableRow>
          <TableHead className="w-44">{t("systemLogs.time")}</TableHead><TableHead className="w-24">{t("systemLogs.level")}</TableHead><TableHead>{t("systemLogs.event")}</TableHead><TableHead className="w-40">{t("systemLogs.source")}</TableHead><TableHead className="w-32">{t("systemLogs.account")}</TableHead><TableHead className="w-40">{t("systemLogs.requestId")}</TableHead><TableHead className="w-20"><span className="sr-only">{t("systemLogs.detail")}</span></TableHead>
        </TableRow></TableHeader>
        <TableBody>
          {query.isPending ? <TableLoadingRow colSpan={7} /> : !query.data?.items.length ? <TableRow><TableCell colSpan={7}><EmptyState message={t("systemLogs.empty")} /></TableCell></TableRow> : query.data.items.map((entry) => <TableRow key={entry.id}>
            <TableCell className="whitespace-nowrap align-top text-xs text-muted-foreground">{formatCompactDateTime(entry.createdAt, i18n.language)}</TableCell>
            <TableCell className="align-top"><LevelBadge level={entry.level} /></TableCell>
            <TableCell className="min-w-72 max-w-xl whitespace-normal align-top"><p className="text-sm font-medium">{eventLabel(entry)}</p>
              {eventLabel(entry) !== entry.message && <p className="mt-1 break-all font-mono text-xs text-muted-foreground">{entry.message}</p>}
              {logSummary(entry) && <p className="mt-1 line-clamp-2 break-words text-sm text-muted-foreground">{logSummary(entry)}</p>}
            </TableCell>
            <TableCell className="align-top"><p>{providerLabel(entry.provider) || "—"}</p><p className="mt-1 max-w-36 truncate font-mono text-xs text-muted-foreground" title={entry.instanceId}>{entry.instanceId}</p></TableCell>
            <TableCell className="align-top">{entry.accountId ? <button className="font-mono text-sm hover:underline" onClick={() => change("accountId", String(entry.accountId))}>#{entry.accountId}</button> : "—"}</TableCell>
            <TableCell className="align-top">{entry.requestId ? <button className="max-w-36 truncate font-mono text-xs hover:underline" title={entry.requestId} onClick={() => change("requestId", entry.requestId)}>{entry.requestId}</button> : "—"}</TableCell>
            <TableCell className="align-top"><Button variant="ghost" size="sm" onClick={() => setSelected(entry)} aria-label={`${t("systemLogs.detail")} #${entry.id}`}>{t("systemLogs.detail")}</Button></TableCell>
          </TableRow>)}
        </TableBody>
      </Table>}
    </DataTableShell>
    <Dialog open={!!selected} onOpenChange={(open) => { if (!open) setSelected(null); }}>
      <DialogContent className="max-h-[85vh] max-w-3xl overflow-y-auto">
        <DialogHeader><DialogTitle>{t("systemLogs.detail")}{selected && ` #${selected.id}`}</DialogTitle><DialogDescription>{selected && formatCompactDateTime(selected.createdAt, i18n.language)}</DialogDescription></DialogHeader>
        {selected && <div className="min-w-0 space-y-5">
          <div className="flex items-center justify-between gap-3"><div className="flex min-w-0 items-center gap-3"><LevelBadge level={selected.level} /><h2 className="break-all text-base font-medium">{eventLabel(selected)}</h2></div><Button variant="secondary" size="sm" onClick={() => void copy(selected)}><Copy />{t("common.copy")}</Button></div>
          {logReason(selected) && <p className="whitespace-pre-wrap break-words rounded-lg bg-secondary/50 p-4 text-sm">{logReason(selected)}</p>}
          <dl className="grid grid-cols-1 gap-4 text-sm sm:grid-cols-2">{[
            [t("systemLogs.source"), `${providerLabel(selected.provider) || "—"} · ${selected.instanceId || "—"}`],
            [t("systemLogs.accountId"), selected.accountId ? `#${selected.accountId}` : "—"],
            [t("systemLogs.event"), selected.message], [t("systemLogs.requestId"), selected.requestId || "—"],
          ].map(([label, value]) => <div key={label} className="min-w-0"><dt className="mb-1 text-muted-foreground">{label}</dt><dd className="break-all font-mono text-xs leading-5">{value}</dd></div>)}</dl>
          <div><h3 className="mb-2 text-sm font-medium">{t("systemLogs.fields")}</h3><pre className="max-h-72 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-secondary/50 p-4 font-mono text-xs leading-6">{JSON.stringify(selected.fields, null, 2)}</pre></div>
        </div>}
      </DialogContent>
    </Dialog>
  </div>;
}

function LevelBadge({ level }: { level: string }) {
  const { t } = useTranslation();
  return <span className={cn("inline-flex items-center rounded-md px-2 py-1 text-xs font-medium", level === "ERROR" ? "bg-red-500/10 text-red-600 dark:text-red-400" : level === "WARN" ? "bg-amber-500/10 text-amber-700 dark:text-amber-400" : "bg-secondary text-muted-foreground")}>
    {t(`systemLogs.levels.${level}`, { defaultValue: level })}
  </span>;
}
