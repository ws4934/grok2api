import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowUpRight, Database, Image as ImageIcon, ListChecks, Play, RefreshCw, Search, Trash2 } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { listModels } from "@/entities/model/model-api";
import { useProviders, type ProviderDTO } from "@/entities/provider/provider-api";
import { providerShortLabel } from "@/entities/provider/provider-presentation";
import { listAccounts } from "@/features/accounts/accounts-api";
import { listClientKeys } from "@/features/client-keys/client-keys-api";
import { deleteMediaAssets, getImageStats, getMediaAsset, listMediaAssets } from "@/features/media/media-api";
import { assetOperations, mediaAssetPath, mediaProviders, type MediaAssetDTO, type MediaKind } from "@/features/media/media-model";
import { ProviderTag, VideoPreview } from "@/features/media/media-ui";
import type { PaginatedDTO } from "@/shared/api/client";
import { ErrorState, LoadingState } from "@/shared/components/data-state";
import { DataTableFilters, type DataTableFilterOption, type DataTableFilterOptionGroup } from "@/shared/components/data-table-filters";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { PageHeader } from "@/shared/components/page-header";
import { Pagination } from "@/shared/components/pagination";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";

const emptyFilters = { provider: "", model: "", operation: "", clientKeyId: "", accountId: "", from: "", to: "" };
type GalleryFilters = typeof emptyFilters;
const mediaCapabilities = new Set(["image", "image_edit", "video"]);
// Lookup lists stay at the server's first 50 matches, like the audit filters.
const LOOKUP_PAGE_SIZE = 50;

export function GalleryPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const providers = useProviders().data;
  const [kind, setKind] = useState<MediaKind>("image");
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [filters, setFilters] = useState<GalleryFilters>(emptyFilters);
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [viewingId, setViewingId] = useState<string | null>(null);
  const debouncedSearch = useDebouncedValue(search);
  const normalizedSearch = debouncedSearch.trim();
  const filtered = Boolean(normalizedSearch) || Object.values(filters).some(Boolean);

  const assetsQuery = useQuery({
    queryKey: ["media", "assets", kind, page, pageSize, normalizedSearch, filters],
    queryFn: () => listMediaAssets({ page, pageSize, kind, search: normalizedSearch, ...filters }),
  });
  const statsQuery = useQuery({
    queryKey: ["media", "images", "stats"],
    queryFn: getImageStats,
    staleTime: 30_000,
  });
  const modelsQuery = useQuery({
    queryKey: ["models", "media-filter"],
    queryFn: () => listModels({ page: 1, pageSize: 100 }),
    staleTime: 60_000,
  });
  const modelOptions = useMemo(() => [...new Set((modelsQuery.data?.items ?? []).filter((model) => mediaCapabilities.has(model.capability)).map((model) => model.publicId))].map((id) => ({ value: id, label: id })), [modelsQuery.data?.items]);
  const keyFilter = useLookupFilter("client-keys", t("media.key"), t("audits.keyFilterPlaceholder"), (search) => listClientKeys({ page: 1, pageSize: LOOKUP_PAGE_SIZE, search }), (key) => ({ value: key.id, label: key.name || key.prefix, description: `#${key.id} · ${key.prefix}` }));
  const accountFilter = useLookupFilter("accounts", t("media.account"), t("audits.accountFilterPlaceholder"), (search) => listAccounts({ page: 1, pageSize: LOOKUP_PAGE_SIZE, search }), (account) => ({ value: account.id, label: account.name || account.email || `#${account.id}`, description: `#${account.id}`, badge: providerShortLabel(account.provider, providers) }));

  const result = assetsQuery.data;
  const refreshing = assetsQuery.isFetching || statsQuery.isFetching;
  const pageIDs = result?.items.map((asset) => asset.id) ?? [];
  const selectedOnPage = pageIDs.filter((id) => selected.has(id));
  const allPageSelected = pageIDs.length > 0 && selectedOnPage.length === pageIDs.length;

  const deleteMutation = useMutation({
    mutationFn: () => deleteMediaAssets([...selected]),
    onSuccess: (deleteResult) => {
      if (result && selectedOnPage.length === result.items.length && page > 1) setPage(page - 1);
      if (viewingId && selected.has(viewingId)) setViewingId(null);
      setSelected(new Set());
      setDeleteOpen(false);
      void queryClient.invalidateQueries({ queryKey: ["media"] });
      toast.success(t("media.images.deleted", { count: deleteResult.deleted }));
    },
    // 409 mediaAssetInUse arrives localized; the selection stays so the admin can adjust it.
    onError: (error) => {
      void queryClient.invalidateQueries({ queryKey: ["media"] });
      toast.error(error instanceof Error ? error.message : t("errors.generic"));
    },
  });

  function setFilter(key: keyof GalleryFilters, value: string): void {
    setFilters((current) => ({ ...current, [key]: value }));
    setPage(1);
  }

  function refreshAll(): void {
    void assetsQuery.refetch();
    void statsQuery.refetch();
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

  function toggleAsset(id: string, checked: boolean): void {
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
        title={t("media.images.title")}
        description={t("media.images.description")}
        actions={(
          <>
            <Tabs value={kind} onValueChange={(value) => { setKind(value === "video" ? "video" : "image"); setPage(1); }}>
              <TabsList aria-label={t("media.kindLabel")}>
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
            <div className="flex w-full min-w-0 flex-wrap items-center gap-3 sm:w-auto">
              <Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} />
              <div className="relative min-w-0 flex-1 sm:w-72 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input
                  className="h-8 pl-9 text-xs"
                  value={search}
                  onChange={(event) => { setSearch(event.target.value); setPage(1); }}
                  placeholder={t("media.images.search")}
                  aria-label={t("media.images.search")}
                />
              </div>
              <DataTableFilters filters={[
                { id: "provider", label: t("media.provider"), value: filters.provider, onChange: (value) => setFilter("provider", value), options: mediaProviders(providers).map((provider) => ({ value: provider.id, label: provider.name })) },
                { id: "model", label: t("media.model"), value: filters.model, onChange: (value) => setFilter("model", value), options: modelOptions },
                { id: "operation", label: t("media.operation"), value: filters.operation, onChange: (value) => setFilter("operation", value), options: assetOperations.map((operation) => ({ value: operation, label: t(`media.assetOperation.${operation}`) })) },
                { id: "key", label: t("media.key"), value: filters.clientKeyId, onChange: (value) => setFilter("clientKeyId", value === "any" ? "" : value), options: [keyFilter] },
                { id: "account", label: t("media.account"), value: filters.accountId, onChange: (value) => setFilter("accountId", value === "any" ? "" : value), options: [accountFilter] },
              ]} />
              <div className="flex items-center gap-1">
                <Input type="date" className="h-8 w-36 text-xs" value={filters.from} max={filters.to || undefined} onChange={(event) => setFilter("from", event.target.value)} aria-label={t("media.images.from")} title={t("media.images.from")} />
                <span className="text-xs text-muted-foreground" aria-hidden="true">–</span>
                <Input type="date" className="h-8 w-36 text-xs" value={filters.to} min={filters.from || undefined} onChange={(event) => setFilter("to", event.target.value)} aria-label={t("media.images.to")} title={t("media.images.to")} />
              </div>
              {filtered && result ? <span className="hidden whitespace-nowrap text-xs tabular-nums text-muted-foreground md:inline">{t("media.images.pageSummary", { count: result.items.length, total: result.total })}</span> : null}
            </div>
            {selected.size > 0 ? (
              <div className="flex h-8 items-center gap-2">
                <span className="text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
                <Button variant="secondary" size="sm" className="text-destructive hover:text-destructive" onClick={() => setDeleteOpen(true)}><Trash2 />{t("common.delete")}</Button>
              </div>
            ) : (
              <GallerySummary
                loading={statsQuery.isPending}
                unavailable={statsQuery.isError}
                totalImages={statsQuery.data?.totalImages ?? 0}
                totalBytes={statsQuery.data?.totalBytes ?? 0}
                locale={i18n.language}
              />
            )}
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
        {assetsQuery.isError ? <ErrorState message={assetsQuery.error.message} onRetry={() => void assetsQuery.refetch()} /> : null}
        {assetsQuery.isPending ? <ImageGridLoading /> : null}
        {!assetsQuery.isPending && result && result.items.length === 0 ? <GalleryEmptyState message={t(filtered ? "media.images.noMatches" : kind === "video" ? "media.images.emptyVideos" : "media.images.empty")} /> : null}

        {!assetsQuery.isPending && result && result.items.length > 0 ? (
          <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-4 2xl:grid-cols-5">
            {result.items.map((asset) => <AssetCard key={asset.id} asset={asset} providers={providers} locale={i18n.language} selectionMode={selected.size > 0} selected={selected.has(asset.id)} onSelectedChange={(checked) => toggleAsset(asset.id, checked)} onOpen={() => setViewingId(asset.id)} />)}
          </div>
        ) : null}
      </DataTableShell>

      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("media.images.deleteTitle", { count: selected.size })}</AlertDialogTitle>
            <AlertDialogDescription>{t("media.images.deleteDescription")}</AlertDialogDescription>
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

      <Dialog open={viewingId !== null} onOpenChange={(open) => !open && setViewingId(null)}>
        <DialogContent className="max-h-[calc(100vh-2rem)] max-w-4xl overflow-y-auto">
          <DialogHeader className="min-w-0 pr-8">
            <DialogTitle>{t("media.images.details")}</DialogTitle>
            <DialogDescription className="min-w-0 truncate font-mono" title={viewingId ?? undefined}>{viewingId}</DialogDescription>
          </DialogHeader>
          {viewingId ? <AssetDetails id={viewingId} providers={providers} locale={i18n.language} /> : null}
        </DialogContent>
      </Dialog>
    </div>
  );
}

// useLookupFilter loads key/account choices only while their submenu is open, re-querying as the admin types.
function useLookupFilter<T>(name: string, label: string, placeholder: string, load: (search: string) => Promise<PaginatedDTO<T>>, toOption: (item: T) => DataTableFilterOptionGroup["options"][number]): DataTableFilterOption {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  const debouncedSearch = useDebouncedValue(search).trim();
  const query = useQuery({
    queryKey: [name, "media-filter", debouncedSearch],
    // A bare number searches by ID, as in the audit filters.
    queryFn: () => load(/^\d+$/.test(debouncedSearch) ? `#${debouncedSearch}` : debouncedSearch),
    enabled: open,
    staleTime: 60_000,
  });
  const items = query.data?.items ?? [];
  return {
    value: "any", label, onGroupsOpenChange: setOpen,
    groupSearch: { value: search, placeholder, onChange: setSearch },
    groups: [{
      id: name, label, hideLabel: true, maxHeightClassName: "max-h-56 overflow-y-auto py-0.5",
      emptyLabel: query.isError ? t("audits.filterOptionsLoadFailed") : query.isFetching ? t("common.loading") : t("audits.filterOptionsEmpty"),
      options: items.map(toOption),
      loading: query.isFetching, hasMore: query.isError, actionLabel: t("common.retry"), onAction: () => { void query.refetch(); },
      noteLabel: !query.isError && (query.data?.total ?? 0) > items.length ? t("audits.filterOptionsTruncated") : undefined,
    }],
  };
}

function AssetCard({ asset, providers, locale, selectionMode, selected, onSelectedChange, onOpen }: { asset: MediaAssetDTO; providers?: ProviderDTO[]; locale: string; selectionMode: boolean; selected: boolean; onSelectedChange: (checked: boolean) => void; onOpen: () => void }) {
  const { t } = useTranslation();
  const path = mediaAssetPath(asset.kind, asset.id);
  return (
    <article className="group relative min-w-0 [content-visibility:auto] [contain-intrinsic-size:0_320px]">
      <Checkbox
        checked={selected}
        onCheckedChange={(checked) => onSelectedChange(checked === true)}
        aria-label={t("common.selectItem", { name: asset.id })}
        className={cn("absolute left-2 top-2 z-10 bg-background/90 shadow-sm backdrop-blur-sm transition-opacity md:opacity-0 md:group-hover:opacity-100 md:group-focus-within:opacity-100", selected && "md:opacity-100")}
      />
      <button
        type="button"
        aria-label={selectionMode ? t("common.selectItem", { name: asset.id }) : t("media.images.openDetails", { id: asset.id })}
        className="relative block aspect-square w-full overflow-hidden rounded-lg bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40"
        onClick={() => selectionMode ? onSelectedChange(!selected) : onOpen()}
      >
        {asset.kind === "video" ? (
          <>
            {/* The media fragment makes the browser fetch metadata and paint an early frame as the poster. */}
            <video src={`${path}#t=0.1`} preload="metadata" muted playsInline className="pointer-events-none size-full object-cover" />
            <span className="absolute bottom-2 right-2 flex size-6 items-center justify-center rounded-full bg-background/85 shadow-sm backdrop-blur-sm"><Play className="size-3" /></span>
          </>
        ) : (
          <img src={path} alt={asset.prompt || asset.id} loading="lazy" decoding="async" className="size-full object-cover transition-transform duration-300 ease-out group-hover:scale-[1.025]" />
        )}
        {selected ? <span className="pointer-events-none absolute inset-0 rounded-lg ring-1 ring-inset ring-primary/70" aria-hidden="true" /> : null}
      </button>
      <div className="space-y-1 px-0.5 pt-2.5 text-xs">
        <div className="flex min-w-0 items-center justify-between gap-2">
          <span className={cn("min-w-0 flex-1 truncate font-medium", !asset.prompt && "font-mono")} title={asset.prompt || asset.id}>{asset.prompt || asset.id}</span>
          {asset.jobId ? <JobLink asset={asset} className="shrink-0 text-muted-foreground hover:text-foreground"><ListChecks className="size-3.5" /></JobLink> : null}
        </div>
        <div className="flex min-w-0 items-center justify-between gap-2 text-[11px] text-muted-foreground">
          <span className="flex min-w-0 items-center gap-1">
            <span className="shrink-0"><ProviderTag id={asset.provider} providers={providers} short /></span>
            {asset.model ? <span className="truncate" title={asset.model}>· {asset.model}</span> : null}
          </span>
          {asset.operation ? <span className="shrink-0">{t(`media.assetOperation.${asset.operation}`, { defaultValue: asset.operation })}</span> : null}
        </div>
        <div className="flex min-w-0 items-center justify-between gap-2 text-[11px] text-muted-foreground">
          <span className="min-w-0 truncate" title={[asset.clientKeyName, asset.accountName].filter(Boolean).join(" · ")}>{[asset.clientKeyName, asset.accountName].filter(Boolean).join(" · ") || "-"}</span>
          <span className="shrink-0 whitespace-nowrap">{formatDateTime(asset.createdAt, locale)}</span>
        </div>
      </div>
    </article>
  );
}

function JobLink({ asset, className, children }: { asset: MediaAssetDTO; className?: string; children: ReactNode }) {
  const { t } = useTranslation();
  return (
    <Link to={`/media-jobs?${new URLSearchParams({ kind: asset.kind, search: asset.jobId })}`} className={className} title={t("media.images.viewJob")} aria-label={t("media.images.viewJob")}>
      {children}
    </Link>
  );
}

function AssetDetails({ id, providers, locale }: { id: string; providers?: ProviderDTO[]; locale: string }) {
  const { t } = useTranslation();
  const query = useQuery({ queryKey: ["media", "asset", id], queryFn: () => getMediaAsset(id) });
  if (query.isPending) return <LoadingState />;
  if (query.isError) return <ErrorState message={query.error.message} onRetry={() => void query.refetch()} />;
  const asset = query.data;
  const path = mediaAssetPath(asset.kind, asset.id);
  const fields: Array<[string, ReactNode]> = [
    [t("media.provider"), <ProviderTag key="provider" id={asset.provider} providers={providers} />],
    [t("media.model"), asset.model || "-"],
    [t("media.images.upstreamModel"), asset.upstreamModel || "-"],
    [t("media.operation"), asset.operation ? t(`media.assetOperation.${asset.operation}`, { defaultValue: asset.operation }) : "-"],
    [t("media.key"), asset.clientKeyId ? `${asset.clientKeyName || "-"} #${asset.clientKeyId}` : asset.clientKeyName || "-"],
    [t("media.account"), asset.accountId ? `${asset.accountName || "-"} #${asset.accountId}` : asset.accountName || "-"],
    [t("media.images.job"), asset.jobId ? <JobLink key="job" asset={asset} className="font-mono text-primary hover:underline">{asset.jobId}</JobLink> : "-"],
    [t("media.images.request"), <span key="request" className="font-mono">{asset.requestId || "-"}</span>],
    [t("media.createdAt"), formatDateTime(asset.createdAt, locale)],
    [t("media.images.type"), `${asset.mimeType} · ${formatBytes(asset.sizeBytes, locale)}`],
    ["SHA-256", <span key="sha" className="font-mono" title={asset.sha256}>{asset.sha256 || "-"}</span>],
  ];
  return (
    <div className="min-w-0 space-y-4 text-xs">
      {asset.kind === "video" ? <VideoPreview assetId={asset.id} /> : (
        <a href={path} target="_blank" rel="noreferrer" className="flex justify-center overflow-hidden rounded-lg bg-muted">
          <img src={path} alt={asset.prompt || asset.id} className="max-h-[60vh] w-auto max-w-full object-contain" />
        </a>
      )}
      {!asset.provider && !asset.jobId && !asset.requestId ? <p className="text-muted-foreground">{t("media.images.noOrigin")}</p> : null}
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
        <p className="max-h-32 overflow-y-auto whitespace-pre-wrap break-words rounded-md bg-muted/50 px-3 py-2">{asset.prompt || "-"}</p>
      </div>
      <div className="flex justify-end">
        <Button asChild variant="secondary" size="sm">
          <a href={path} target="_blank" rel="noreferrer"><ArrowUpRight />{t("media.images.openOriginal")}</a>
        </Button>
      </div>
    </div>
  );
}

function GallerySummary({ loading, unavailable, totalImages, totalBytes, locale }: { loading: boolean; unavailable: boolean; totalImages: number; totalBytes: number; locale: string }) {
  const { t } = useTranslation();
  return (
    <div className="flex h-8 items-center gap-4 whitespace-nowrap text-xs" aria-busy={loading}>
      <span className="inline-flex items-center gap-1.5">
        <ImageIcon className="size-3.5 text-emerald-600 dark:text-emerald-400" />
        <span className="text-muted-foreground">{t("media.images.totalImages")}</span>
        <strong className="font-medium tabular-nums">{loading ? <Spinner className="size-3" /> : unavailable ? "-" : formatNumber(totalImages, locale, 0)}</strong>
      </span>
      <span className="h-3 w-px bg-border" aria-hidden="true" />
      <span className="inline-flex items-center gap-1.5">
        <Database className="size-3.5 text-sky-600 dark:text-sky-400" />
        <span className="text-muted-foreground">{t("media.images.totalBytes")}</span>
        <strong className="font-medium tabular-nums">{loading ? <Spinner className="size-3" /> : unavailable ? "-" : formatBytes(totalBytes, locale)}</strong>
      </span>
    </div>
  );
}

function GalleryEmptyState({ message }: { message: string }) {
  return (
    <div className="flex min-h-72 flex-col items-center justify-center gap-3 text-center text-muted-foreground">
      <span className="flex size-10 items-center justify-center rounded-full bg-muted/70">
        <ImageIcon className="size-5 stroke-1.5" />
      </span>
      <p className="text-sm">{message}</p>
    </div>
  );
}

function ImageGridLoading() {
  return (
    <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-4 2xl:grid-cols-5" aria-hidden="true">
      {Array.from({ length: 10 }, (_, index) => (
        <div key={index} className="min-w-0">
          <div className="aspect-square animate-pulse rounded-lg bg-muted" />
          <div className="space-y-2 px-0.5 pt-2.5">
            <div className="h-3 w-3/4 animate-pulse rounded bg-muted" />
            <div className="h-2.5 w-1/2 animate-pulse rounded bg-muted" />
          </div>
        </div>
      ))}
    </div>
  );
}

function formatBytes(value: number, locale: string): string {
  if (!Number.isFinite(value) || value <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let size = value;
  let unitIndex = 0;
  while (size >= 1024 && unitIndex < units.length - 1) {
    size /= 1024;
    unitIndex += 1;
  }
  return `${new Intl.NumberFormat(locale, { maximumFractionDigits: unitIndex === 0 || size >= 10 ? 0 : 1 }).format(size)} ${units[unitIndex]}`;
}
