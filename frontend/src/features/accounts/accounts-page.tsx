import { zodResolver } from "@hookform/resolvers/zod";
import { useInfiniteQuery, useIsMutating, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ClipboardPaste, Download, ExternalLink, FileUp, MoreHorizontal, Pencil, Plus, RefreshCw, RotateCw, Search, TimerOff, Trash2, TriangleAlert } from "lucide-react";
import { useCallback, useEffect, useRef, useState, type CSSProperties, type ReactNode } from "react";
import { useForm, useWatch } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useProviders, type ProviderDTO } from "@/entities/provider/provider-api";
import { providerIcon } from "@/entities/provider/provider-icon";
import { orderedProviderIds, providerColor, providerLabel } from "@/entities/provider/provider-presentation";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";
import {
  cleanupAccounts,
  clearAccountCooldown,
  deleteAccount,
  deleteAccounts,
  previewAccountDeletion,
  previewCleanup,
  exportAccountBatch,
  exportSelectedAccounts,
  getAccountSummary,
  importProviderAccounts,
  listAccounts,
  refreshAccountsQuota,
  refreshAccountQuota,
  refreshAccountToken,
  refreshAccountsTokens,
  refreshAllAccountTokens,
  refreshProviderAccountQuotas,
  resetAccountsQuota,
  resetAllAccountQuota,
  updateAccount,
  updateAccountsEnabled,
  updateAccountsMaxConcurrent,
  type AccountDTO,
  type AccountCleanupStatus,
  type AccountProvider,
  type CleanupPreviewDTO,
  type AccountUpdateInput,
  type AccountTaskProgressDTO,
} from "@/features/accounts/accounts-api";
import { AccountAuthDialog } from "@/features/accounts/account-auth-dialog";
import { AccountNameCell } from "@/features/accounts/account-name-cell";
import { accountEditSchema, accountEgressScopes, accountTaskKey, connectMethods, isAbortError, type AccountEditForm, type AccountFilterId, type AccountTask, type AccountUIContext } from "@/features/accounts/account-ui";
import { accountUI } from "@/features/accounts/providers";
import { assignEgressAccounts, listAllEgressNodes, listEgressNodes, listEgressSources, unassignEgressAccounts } from "@/features/settings/settings-api";

type QuotaTask = "sync" | "reset";
type EgressConfigurationTask = "bind" | "unbind";

const egressFilterNodePageSize = 100;
const egressFilterSourcePageSize = 100;

type AccountSelection = {
  provider: AccountProvider;
  ids: Set<string>;
};

export function AccountsPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const providers = useProviders().data;
  const providerIds = orderedProviderIds(providers);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const quickImportFileInputRef = useRef<HTMLInputElement>(null);
  const quotaSyncAbortRef = useRef<AbortController | null>(null);
  const renewalAbortRef = useRef<AbortController | null>(null);
  const importAbortRef = useRef<AbortController | null>(null);
  const importToastRef = useRef<string | number | null>(null);
  const [selectedProvider, setSelectedProvider] = useState("");
  const provider = providerIds.includes(selectedProvider) ? selectedProvider : providerIds[0];
  const catalogEntry = providers?.find((entry) => entry.id === provider);
  const ui = accountUI(provider);
  const { TypeCell, QuotaCell, EditFields, RowMenuItems, BulkActions, Dialogs, AuthHelp } = ui;
  const providerName = providerLabel(provider, providers);
  const refreshable = Boolean(catalogEntry?.credential.refresh);
  const linkedTargets = ui.linkedProviders ?? [];
  const egressScopes = accountEgressScopes(provider, ui);
  const [primaryEgressScope, secondaryEgressScope] = egressScopes;
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [filters, setFilters] = useState<Partial<Record<AccountFilterId, string>>>({});
  const [egressFilter, setEgressFilter] = useState("");
  const [egressFilterSelectedLabel, setEgressFilterSelectedLabel] = useState("");
  const [egressFilterOptionsOpen, setEgressFilterOptionsOpen] = useState(false);
  const [egressFilterOptionsSearch, setEgressFilterOptionsSearch] = useState("");
  const [sort, setSort] = useState<TableSort>({ field: "createdAt", order: "desc" });
  const [selection, setSelection] = useState<AccountSelection>(() => ({ provider: "", ids: new Set() }));
  const [task, setTask] = useState<AccountTask | null>(null);
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  const [batchConcurrencyOpen, setBatchConcurrencyOpen] = useState(false);
  const [batchMaxConcurrent, setBatchMaxConcurrent] = useState("1");
  const [batchQuotaTaskOpen, setBatchQuotaTaskOpen] = useState(false);
  const [batchQuotaTask, setBatchQuotaTask] = useState<QuotaTask>("sync");
  const [egressConfigurationOpen, setEgressConfigurationOpen] = useState(false);
  const [egressConfigurationTask, setEgressConfigurationTask] = useState<EgressConfigurationTask>("bind");
  const [egressNodeID, setEgressNodeID] = useState("");
  const [cleanupOpen, setCleanupOpen] = useState(false);
  const [cleanupStatuses, setCleanupStatuses] = useState<Set<AccountCleanupStatus>>(() => new Set());
  // Cleanup preview + optional linked deletion (independent from the delete dialogs' state).
  const [cleanupLinkedTargets, setCleanupLinkedTargets] = useState<AccountProvider[]>([]);
  // Keyed preview: a stale result stays mounted while the next one loads, so the
  // dialog never reflows between "has counts" and "no counts".
  const [cleanupPreview, setCleanupPreview] = useState<{ key: string; data: CleanupPreviewDTO } | null>(null);
  const [cleanupPreviewError, setCleanupPreviewError] = useState(false);
  const [exportOpen, setExportOpen] = useState(false);
  const [exportLimit, setExportLimit] = useState("1000");
  const [exportCursor, setExportCursor] = useState("0");
  const [exportSnapshotMaxId, setExportSnapshotMaxId] = useState("0");
  const [exportBatchNumber, setExportBatchNumber] = useState(1);
  const [exportCompletedCount, setExportCompletedCount] = useState(0);
  const [syncAllOpen, setSyncAllOpen] = useState(false);
  const [allQuotaTask, setAllQuotaTask] = useState<QuotaTask>("sync");
  const [quotaSyncProgress, setQuotaSyncProgress] = useState<AccountTaskProgressDTO | null>(null);
  const [renewAllOpen, setRenewAllOpen] = useState(false);
  const [renewalProgress, setRenewalProgress] = useState<AccountTaskProgressDTO | null>(null);
  const [editing, setEditing] = useState<AccountDTO | null>(null);
  const [deleting, setDeleting] = useState<AccountDTO | null>(null);
  const [linkedDeleteTargets, setLinkedDeleteTargets] = useState<AccountProvider[]>([]);
  const [linkedDeleteCounts, setLinkedDeleteCounts] = useState<Partial<Record<AccountProvider, number>>>({});
  // Preview failures must not be painted as +0 — block confirm until a successful recount.
  const [linkedDeletePreviewError, setLinkedDeletePreviewError] = useState(false);
  const [authOpen, setAuthOpen] = useState(false);
  const [quickImportOpen, setQuickImportOpen] = useState(false);
  const [quickImportTokens, setQuickImportTokens] = useState("");
  const debouncedSearch = useDebouncedValue(search);
  const debouncedEgressFilterOptionsSearch = useDebouncedValue(egressFilterOptionsSearch);
  const bulkTaskPending = useIsMutating({ mutationKey: accountTaskKey }) > 0;

  useEffect(() => () => {
    quotaSyncAbortRef.current?.abort();
    renewalAbortRef.current?.abort();
    importAbortRef.current?.abort();
    if (importToastRef.current !== null) toast.dismiss(importToastRef.current);
  }, []);

  const form = useForm<AccountEditForm>({
    resolver: zodResolver(accountEditSchema(t)),
    defaultValues: {
      name: "", enabled: true, priority: 1, maxConcurrent: 8, minimumRemaining: 0,
      cloudflareCookies: "", clearCloudflareCookies: false, buildSuperEntitled: false, buildRouteMode: "auto",
    },
  });
  const accountEnabled = useWatch({ control: form.control, name: "enabled" });
  const selected = selection.provider === provider ? selection.ids : new Set<string>();
  const selectedIdsKey = Array.from(selected).sort().join(",");

  const accountsQuery = useQuery({
    queryKey: ["accounts", provider, page, pageSize, debouncedSearch, statusFilter, egressFilter, filters, sort.field, sort.order],
    queryFn: () => listAccounts({
      provider, page, pageSize, search: debouncedSearch, status: statusFilter, egress: egressFilter, ...filters,
      sortBy: sort.field, sortOrder: sort.order,
    }),
    // Keep the previous rows while the next page loads, but not across providers: their columns differ.
    placeholderData: (previous, previousQuery) => previousQuery?.queryKey[1] === provider ? previous : undefined,
  });

  const summaryQuery = useQuery({
    queryKey: ["accounts", "summary"],
    queryFn: getAccountSummary,
  });
  // The binding dialog still needs every compatible node, but only while open.
  const egressNodesQuery = useQuery({
    queryKey: ["egress-nodes", "account-binding"],
    queryFn: () => listAllEgressNodes(),
    enabled: egressConfigurationOpen && egressConfigurationTask === "bind",
    staleTime: 60_000,
  });
  // Filter choices are loaded only when the third-level menu opens. Nodes and
  // subscription sources use bounded pages so large pools do not flood the page.
  // A secondary scope (Console accounts may use Grok Web exits) pages independently
  // so unrelated nodes can never consume the primary result pages.
  const egressFilterNodesQuery = useInfiniteQuery({
    queryKey: ["egress-nodes", "account-filter", primaryEgressScope, debouncedEgressFilterOptionsSearch],
    queryFn: ({ pageParam }) => listEgressNodes({ page: pageParam, pageSize: egressFilterNodePageSize, search: debouncedEgressFilterOptionsSearch, scope: primaryEgressScope }),
    initialPageParam: 1,
    getNextPageParam: (lastPage) => lastPage.page * lastPage.pageSize < lastPage.total ? lastPage.page + 1 : undefined,
    enabled: egressFilterOptionsOpen,
    staleTime: 60_000,
  });
  const egressFilterSecondaryNodesQuery = useInfiniteQuery({
    queryKey: ["egress-nodes", "account-filter", secondaryEgressScope ?? "", debouncedEgressFilterOptionsSearch],
    queryFn: ({ pageParam }) => listEgressNodes({ page: pageParam, pageSize: egressFilterNodePageSize, search: debouncedEgressFilterOptionsSearch, scope: secondaryEgressScope }),
    initialPageParam: 1,
    getNextPageParam: (lastPage) => lastPage.page * lastPage.pageSize < lastPage.total ? lastPage.page + 1 : undefined,
    enabled: egressFilterOptionsOpen && Boolean(secondaryEgressScope),
    staleTime: 60_000,
  });
  const egressFilterSourcesQuery = useInfiniteQuery({
    queryKey: ["egress-sources", "account-filter", primaryEgressScope, debouncedEgressFilterOptionsSearch],
    queryFn: ({ pageParam }) => listEgressSources({ page: pageParam, pageSize: egressFilterSourcePageSize, search: debouncedEgressFilterOptionsSearch, scope: primaryEgressScope }),
    initialPageParam: 1,
    getNextPageParam: (lastPage) => lastPage.page * lastPage.pageSize < lastPage.total ? lastPage.page + 1 : undefined,
    enabled: egressFilterOptionsOpen,
    staleTime: 60_000,
  });
  const egressFilterSecondarySourcesQuery = useInfiniteQuery({
    queryKey: ["egress-sources", "account-filter", secondaryEgressScope ?? "", debouncedEgressFilterOptionsSearch],
    queryFn: ({ pageParam }) => listEgressSources({ page: pageParam, pageSize: egressFilterSourcePageSize, search: debouncedEgressFilterOptionsSearch, scope: secondaryEgressScope }),
    initialPageParam: 1,
    getNextPageParam: (lastPage) => lastPage.page * lastPage.pageSize < lastPage.total ? lastPage.page + 1 : undefined,
    enabled: egressFilterOptionsOpen && Boolean(secondaryEgressScope),
    staleTime: 60_000,
  });

  const invalidateAccountData = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: ["accounts"] });
  }, [queryClient]);

  const updateMutation = useMutation({
    mutationFn: (values: AccountEditForm) => {
      if (!editing) throw new Error(t("errors.generic"));
      const input: AccountUpdateInput = {
        name: values.name,
        priority: values.priority,
        maxConcurrent: values.maxConcurrent,
        minimumRemaining: values.minimumRemaining,
        ...accountUI(editing.provider).editInput?.(values, editing),
      };
      if (values.enabled !== editing.enabled) input.enabled = values.enabled;
      return updateAccount(editing.id, input);
    },
    onSuccess: (account) => {
      invalidateAccountData();
      // Provider edits such as the Build entitlement change model availability.
      void queryClient.invalidateQueries({ queryKey: ["models"] });
      setEditing(null);
      if (account.modelSyncFailed) toast.warning(t("accounts.updatedWithModelSyncFailure"));
      else if (account.enabledDoesNotClearCooldown) toast.warning(t("accounts.enabledDoesNotClearCooldown"));
      else toast.success(t("accounts.updated"));
    },
    onError: showError,
  });

  useEffect(() => {
    if (!deleting && !batchDeleteOpen) return;
    const ids = deleting ? [deleting.id] : (selectedIdsKey ? selectedIdsKey.split(",") : []);
    if (linkedDeleteTargets.length === 0 || ids.length === 0) return;
    let cancelled = false;
    // Keep dialog height stable: never mount/unmount loading rows; only update counts in place.
    // Clear error/counts only inside the async path (not sync in effect body) to satisfy react-hooks/set-state-in-effect.
    const timer = window.setTimeout(() => {
      void previewAccountDeletion(ids, provider, linkedDeleteTargets)
        .then((preview) => {
          if (cancelled) return;
          // Always materialize a count for every selected target (including 0),
          // otherwise a missing key would leave the spinner forever.
          const next: Partial<Record<AccountProvider, number>> = {};
          for (const target of linkedDeleteTargets) {
            next[target] = preview.linkedByProvider?.[target] ?? 0;
          }
          setLinkedDeletePreviewError(false);
          setLinkedDeleteCounts(next);
        })
        .catch(() => {
          if (cancelled) return;
          // Do NOT write fake +0 — that understates destructive scope. Block confirm until retry succeeds.
          setLinkedDeleteCounts({});
          setLinkedDeletePreviewError(true);
          toast.error(t("accounts.linkedDeletePreviewFailed"));
        });
    }, 300);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [batchDeleteOpen, deleting, linkedDeleteTargets, provider, selectedIdsKey, t]);

  // Count is display-only. Until preview returns, show a tiny spinner — never flash +0 then +N.
  // On preview error keep targets checked but show failure (not +0) and block confirm.
  const linkedDeleteSlot = (target: AccountProvider) => {
    const checked = linkedDeleteTargets.includes(target);
    const failed = checked && linkedDeletePreviewError && !(target in linkedDeleteCounts);
    const pending = checked && !linkedDeletePreviewError && !(target in linkedDeleteCounts);
    const label = !checked || pending ? "" : failed ? t("accounts.linkedDeleteExtraFailed") : t("accounts.linkedDeleteExtra", { count: linkedDeleteCounts[target] ?? 0 });
    return { pending, failed, label };
  };

  // When any linked target is checked, require a successful preview before confirm.
  const linkedPreviewBlocking =
    linkedDeleteTargets.length > 0 &&
    (linkedDeletePreviewError || linkedDeleteTargets.some((target) => !(target in linkedDeleteCounts)));

  const resetLinkedDeleteState = () => {
    setLinkedDeleteTargets([]);
    setLinkedDeleteCounts({});
    setLinkedDeletePreviewError(false);
  };

  const toggleLinkedDeleteTarget = (target: AccountProvider, checked: boolean) => {
    setLinkedDeleteTargets((current) => {
      const next = checked ? (current.includes(target) ? current : [...current, target]) : current.filter((item) => item !== target);
      if (next.length === 0) {
        setLinkedDeleteCounts({});
        setLinkedDeletePreviewError(false);
      } else if (!checked) {
        setLinkedDeleteCounts((counts) => {
          const copy = { ...counts };
          delete copy[target];
          return copy;
        });
      } else {
        // New selection: drop previous error and recount.
        setLinkedDeletePreviewError(false);
      }
      return next;
    });
  };

  const selectAllLinkedTargets = () => {
    const allSelected = linkedTargets.every((item) => linkedDeleteTargets.includes(item));
    if (allSelected) {
      setLinkedDeleteTargets([]);
      setLinkedDeleteCounts({});
      return;
    }
    // Clear counts so newly selected targets show spinner until preview returns.
    setLinkedDeletePreviewError(false);
    setLinkedDeleteCounts({});
    setLinkedDeleteTargets([...linkedTargets]);
  };

  const deleteMutation = useMutation({
    // Snapshot id/targets in mutate() args so AlertDialog close/reset cannot clear linkedDeleteTargets mid-flight.
    mutationFn: (input: { id: string; provider: AccountProvider; linkedDeleteTargets: AccountProvider[] }) =>
      deleteAccount(input.id, input.linkedDeleteTargets.length ? { provider: input.provider, linkedDeleteTargets: input.linkedDeleteTargets } : undefined),
    onSuccess: () => {
      invalidateAccountData();
      setDeleting(null);
      resetLinkedDeleteState();
      toast.success(t("accounts.deleted"));
    },
    onError: showError,
  });

  // Batch paths skip groups that still have active media jobs; surface that instead of a bare success.
  const notifyDeleteResult = (result: { deleted?: number; skipped?: number } | { deleted: boolean }) => {
    const skipped = typeof result === "object" && "skipped" in result ? result.skipped ?? 0 : 0;
    if (skipped > 0) {
      const deleted = typeof result === "object" && typeof result.deleted === "number" ? result.deleted : 0;
      toast.warning(t("accounts.deletedWithSkipped", { deleted, skipped }));
      return;
    }
    toast.success(t("accounts.deleted"));
  };

  const tokenMutation = useMutation({
    mutationFn: refreshAccountToken,
    onSuccess: () => {
      invalidateAccountData();
      toast.success(t("accounts.authRefreshed"));
    },
    onError: showError,
  });

  const clearCooldownMutation = useMutation({
    mutationFn: clearAccountCooldown,
    onSuccess: () => {
      invalidateAccountData();
      toast.success(t("accounts.cooldownCleared"));
    },
    onError: showError,
  });

  const quotaMutation = useMutation({
    mutationFn: (id: string) => (ui.refreshQuota ?? refreshAccountQuota)(id),
    onSuccess: () => {
      invalidateAccountData();
      toast.success(t("accounts.billingRefreshed"));
    },
    onError: showError,
  });

  const allTokenMutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: () => {
      const controller = new AbortController();
      renewalAbortRef.current = controller;
      setRenewalProgress(null);
      return refreshAllAccountTokens(setRenewalProgress, controller.signal);
    },
    onSuccess: (result) => {
      setRenewAllOpen(false);
      toast.success(t("accounts.allTokensRefreshed", result));
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => { renewalAbortRef.current = null; setRenewalProgress(null); invalidateAccountData(); },
  });

  const quotaSyncMutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: () => {
      const controller = new AbortController();
      quotaSyncAbortRef.current = controller;
      setQuotaSyncProgress(null);
      if (ui.syncAll) return ui.syncAll(setQuotaSyncProgress, controller.signal);
      return refreshProviderAccountQuotas(provider, setQuotaSyncProgress, controller.signal);
    },
    onSuccess: (result) => {
      setSyncAllOpen(false);
      toast.success(t("accounts.allBillingRefreshed", result));
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => { quotaSyncAbortRef.current = null; setQuotaSyncProgress(null); invalidateAccountData(); },
  });

  const allQuotaResetMutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: resetAllAccountQuota,
    onSuccess: (result) => {
      setSyncAllOpen(false);
      toast.success(t("accountQuotaReset.completed", result));
    },
    onError: showError,
    onSettled: invalidateAccountData,
  });

  const importMutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: (files: File[]) => {
      const controller = new AbortController();
      importAbortRef.current = controller;
      const toastID = toast.loading(t("common.importingProgress", { completed: 0, total: "…" }));
      importToastRef.current = toastID;
      const onProgress = (progress: AccountTaskProgressDTO) => {
        toast.loading(t(progress.phase === "syncing" ? "common.syncingProgress" : "common.importingProgress", progress), { id: toastID });
      };
      return importProviderAccounts(provider, files, onProgress, controller.signal);
    },
    onSuccess: (result) => {
      if (importToastRef.current !== null) toast.dismiss(importToastRef.current);
      importToastRef.current = null;
      importAbortRef.current = null;
      setQuickImportOpen(false);
      setQuickImportTokens("");
      if (result.failed > 0) {
        toast.warning(t("accounts.importedWithFailures", result));
        return;
      }
      if (result.syncFailed > 0) {
        toast.warning(t("accounts.importedWithSyncFailures", result));
        return;
      }
      toast.success(t("accounts.imported", result));
    },
    onError: (error) => {
      if (importToastRef.current !== null) toast.dismiss(importToastRef.current);
      importToastRef.current = null;
      importAbortRef.current = null;
      if (!isAbortError(error)) showError(error);
    },
    onSettled: () => {
      importAbortRef.current = null;
      invalidateAccountData();
    },
  });

  const exportMutation = useMutation({
    mutationFn: async (input: { kind: "selected"; ids: string[] } | { kind: "batch"; limit: number; afterId: string; snapshotMaxId: string; batchNumber: number }) => {
      if (input.kind === "selected") {
        return { kind: input.kind, blob: await exportSelectedAccounts(provider, input.ids) } as const;
      }
      return { kind: input.kind, batchNumber: input.batchNumber, batch: await exportAccountBatch(provider, input.limit, input.afterId, input.snapshotMaxId) } as const;
    },
    onSuccess: (result) => {
      if (result.kind === "selected") {
        downloadAccountExport(result.blob, provider, "selected");
        setExportOpen(false);
        toast.success(t("accounts.exported"));
        return;
      }
      downloadAccountExport(result.batch.blob, provider, `batch-${String(result.batchNumber).padStart(4, "0")}`);
      const completed = exportCompletedCount + result.batch.count;
      if (result.batch.hasMore) {
        setExportCursor(result.batch.nextId);
        setExportSnapshotMaxId(result.batch.snapshotMaxId);
        setExportBatchNumber(result.batchNumber + 1);
        setExportCompletedCount(completed);
        toast.success(t("accountExport.batchCompleted", { count: result.batch.count }));
        return;
      }
      setExportOpen(false);
      toast.success(t("accountExport.completed", { count: completed }));
    },
    onError: showError,
  });

  const batchUpdateMutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: (enabled: boolean) => updateAccountsEnabled([...selected], enabled, provider),
    onSuccess: () => {
      clearSelection();
      invalidateAccountData();
      toast.success(t("accounts.batchUpdated"));
    },
    onError: showError,
  });

  const batchConcurrencyMutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: (maxConcurrent: number) => updateAccountsMaxConcurrent([...selected], maxConcurrent, provider),
    onSuccess: () => {
      setBatchConcurrencyOpen(false);
      clearSelection();
      invalidateAccountData();
      toast.success(t("accounts.batchConcurrencyUpdated"));
    },
    onError: showError,
  });

  const batchBillingMutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: () => refreshAccountsQuota([...selected], provider),
    onSuccess: (result) => {
      clearSelection();
      setBatchQuotaTaskOpen(false);
      invalidateAccountData();
      toast.success(t("accounts.batchBillingRefreshed", result));
    },
    onError: showError,
  });

  const batchQuotaResetMutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: () => resetAccountsQuota([...selected], provider),
    onSuccess: (result) => {
      clearSelection();
      setBatchQuotaTaskOpen(false);
      invalidateAccountData();
      toast.success(t("accountQuotaReset.completed", result));
    },
    onError: showError,
  });

  const batchTokenMutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: () => refreshAccountsTokens([...selected], provider),
    onSuccess: (result) => {
      clearSelection();
      invalidateAccountData();
      toast.success(t("accounts.allTokensRefreshed", result));
    },
    onError: showError,
  });

  const batchDeleteMutation = useMutation({
    mutationKey: accountTaskKey,
    // Snapshot selection/targets at click time; dialog unmount/reset must not empty targets.
    mutationFn: (input: { ids: string[]; provider: AccountProvider; linkedDeleteTargets: AccountProvider[] }) =>
      deleteAccounts(input.ids, input.provider, input.linkedDeleteTargets),
    onSuccess: (result) => {
      clearSelection();
      setBatchDeleteOpen(false);
      resetLinkedDeleteState();
      invalidateAccountData();
      notifyDeleteResult(result);
    },
    onError: showError,
  });

  const bindEgressMutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: () => {
      if (!egressNodeID) throw new Error(t("accounts.bindEgressEmpty"));
      return assignEgressAccounts(egressNodeID, provider, [...selected]);
    },
    onSuccess: () => {
      clearSelection();
      setEgressConfigurationOpen(false);
      invalidateAccountData();
      void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
      toast.success(t("accounts.egressBound"));
    },
    onError: showError,
  });
  const unbindEgressMutation = useMutation({
    mutationKey: accountTaskKey,
    mutationFn: () => unassignEgressAccounts(provider, [...selected]),
    onSuccess: () => {
      clearSelection();
      setEgressConfigurationOpen(false);
      invalidateAccountData();
      void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
      toast.success(t("accounts.egressUnbound"));
    },
    onError: showError,
  });

  const resetCleanupState = () => {
    setCleanupStatuses(new Set());
    setCleanupLinkedTargets([]);
    setCleanupPreview(null);
    setCleanupPreviewError(false);
  };

  // Toggles never drop the previous preview: freshness is derived from the key below.
  const toggleCleanupTarget = (target: AccountProvider, checked: boolean) => {
    setCleanupLinkedTargets((current) => checked ? (current.includes(target) ? current : [...current, target]) : current.filter((item) => item !== target));
    setCleanupPreviewError(false);
  };

  const selectAllCleanupTargets = () => {
    const allSelected = linkedTargets.every((item) => cleanupLinkedTargets.includes(item));
    setCleanupLinkedTargets(allSelected ? [] : [...linkedTargets]);
    setCleanupPreviewError(false);
  };

  const cleanupMutation = useMutation({
    mutationKey: accountTaskKey,
    // Snapshot statuses/targets at click; dialog close/reset must not mutate an in-flight request.
    mutationFn: (input: { statuses: AccountCleanupStatus[]; targets: AccountProvider[] }) =>
      cleanupAccounts(provider, input.statuses, input.targets),
    onSuccess: (result) => {
      setCleanupOpen(false);
      resetCleanupState();
      invalidateAccountData();
      const linked = result.linkedDeleted ?? 0;
      const skipped = result.skipped ?? 0;
      if (linked > 0 || skipped > 0) {
        toast.success(t("accounts.cleanupCompletedDetailed", { deleted: result.deleted, linked, skipped }));
      } else {
        toast.success(t("accounts.cleanupCompleted", { deleted: result.deleted }));
      }
    },
    onError: showError,
  });

  // Debounced cleanup preview: counts refresh whenever statuses/targets change.
  // setState only inside timeout/promise callbacks (react-hooks/set-state-in-effect).
  const cleanupStatusesKey = [...cleanupStatuses].sort().join(",");
  const cleanupTargetsKey = [...cleanupLinkedTargets].sort().join(",");
  const cleanupPreviewKey = `${provider}|${cleanupStatusesKey}|${cleanupTargetsKey}`;
  // Fresh = the loaded preview matches the current selection; otherwise show spinners
  // in the fixed-size count slots and keep the confirm button disabled.
  const cleanupPreviewFresh = !cleanupPreviewError && cleanupPreview?.key === cleanupPreviewKey;
  const cleanupPreviewTotals = cleanupPreviewFresh ? cleanupPreview?.data ?? null : null;
  useEffect(() => {
    if (!cleanupOpen || cleanupStatusesKey === "") return;
    let cancelled = false;
    const previewKey = `${provider}|${cleanupStatusesKey}|${cleanupTargetsKey}`;
    const statuses = cleanupStatusesKey.split(",") as AccountCleanupStatus[];
    // Defense in depth: never send a target that is invalid for the current pool.
    const allowed = accountUI(provider).linkedProviders ?? [];
    const targets = (cleanupTargetsKey ? cleanupTargetsKey.split(",") : []).filter((target) => allowed.includes(target));
    const timer = window.setTimeout(() => {
      void previewCleanup(provider, statuses, targets)
        .then((preview) => {
          if (cancelled) return;
          setCleanupPreviewError(false);
          setCleanupPreview({ key: previewKey, data: preview });
        })
        .catch(() => {
          if (cancelled) return;
          // No fake zeros: destructive scope must never be understated.
          setCleanupPreview(null);
          setCleanupPreviewError(true);
          toast.error(t("accounts.cleanupPreviewFailed"));
        });
    }, 300);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [cleanupOpen, cleanupStatusesKey, cleanupTargetsKey, provider, t]);

  function changeProvider(value: AccountProvider) {
    setSelectedProvider(value);
    setPage(1);
    setSelection({ provider: value, ids: new Set() });
    setFilters({});
    setStatusFilter("");
    // A node or subscription narrowing belongs to the previous pool's scope;
    // keep the plain bound filter and drop the target.
    setEgressFilter((current) => (current.includes(":") ? "bound" : current));
    setEgressFilterSelectedLabel("");
    setEgressFilterOptionsOpen(false);
    setEgressFilterOptionsSearch("");
    setQuickImportOpen(false);
    setQuickImportTokens("");
    setTask(null);
    // Cleanup dialog state is provider-scoped: linked targets from another pool
    // would be rejected by the API (self-target 400) once the dialog reopens.
    setCleanupOpen(false);
    resetCleanupState();
  }

  function setFilter(id: AccountFilterId, value: string): void {
    setFilters((current) => ({ ...current, [id]: value }));
    setPage(1);
  }

  function submitQuickImport(): void {
    const value = quickImportTokens.trim();
    if (!value) return;
    importMutation.mutate([new File([value], ui.importTexts.filename, { type: "text/plain" })]);
  }

  async function loadQuickImportFile(file: File | undefined): Promise<void> {
    if (!file) return;
    if (file.size > 30 * 1024 * 1024) {
      toast.error(t("apiErrors.accountImportFileTooLarge"));
      return;
    }
    try {
      setQuickImportTokens(await file.text());
    } catch {
      toast.error(t("errors.generic"));
    }
  }

  function beginEdit(account: AccountDTO): void {
    setEditing(account);
    form.reset({
      name: account.name,
      enabled: account.enabled,
      priority: account.priority,
      maxConcurrent: account.maxConcurrent,
      minimumRemaining: account.minimumRemaining,
      cloudflareCookies: "",
      clearCloudflareCookies: false,
      buildSuperEntitled: account.buildSuperEntitled,
      buildRouteMode: account.buildRouteMode,
    });
  }

  function showError(error: unknown): void {
    toast.error(error instanceof Error ? error.message : t("errors.generic"));
  }

  const result = accountsQuery.data;
  const pageIDs = result?.items.map((account) => account.id) ?? [];
  const selectedOnPage = pageIDs.filter((id) => selected.has(id));
  const allPageSelected = pageIDs.length > 0 && selectedOnPage.length === pageIDs.length;

  function clearSelection(): void {
    setSelection((current) => ({ provider: current.provider, ids: new Set() }));
  }

  function resetExportProgress(): void {
    setExportCursor("0");
    setExportSnapshotMaxId("0");
    setExportBatchNumber(1);
    setExportCompletedCount(0);
  }

  function openProviderExport(): void {
    clearSelection();
    resetExportProgress();
    setExportOpen(true);
  }

  function openSelectedExport(): void {
    resetExportProgress();
    setExportOpen(true);
  }

  function togglePage(checked: boolean): void {
    setSelection((current) => {
      const next = new Set(current.provider === provider ? current.ids : []);
      for (const id of pageIDs) {
        if (checked) next.add(id);
        else next.delete(id);
      }
      return { provider, ids: next };
    });
  }

  function toggleAccount(id: string, checked: boolean): void {
    setSelection((current) => {
      const next = new Set(current.provider === provider ? current.ids : []);
      if (checked) next.add(id);
      else next.delete(id);
      return { provider, ids: next };
    });
  }

  function changeSort(field: string, initialOrder: SortOrder): void {
    setSort((current) => nextTableSort(current, field, initialOrder));
    setPage(1);
  }

  const ctx: AccountUIContext = {
    provider,
    pending: bulkTaskPending,
    task,
    openTask: setTask,
    closeTask: () => setTask(null),
    clearSelection,
    invalidate: invalidateAccountData,
    showError,
  };

  const summary = summaryQuery.data;
  const recoveringAccounts = summary?.recovering ?? 0;
  const cooldownAccounts = summary?.recovery.cooldown ?? 0;
  const waitingResetAccounts = summary?.recovery.waitingReset ?? 0;
  const probingAccounts = summary?.recovery.probing ?? 0;
  const disabledAccounts = summary?.issues.disabled ?? 0;
  const invalidAccounts = summary?.issues.reauthRequired ?? 0;
  const riskAccounts = summary?.risk ?? 0;
  const abnormalAccounts = recoveringAccounts + disabledAccounts + invalidAccounts;
  const summaryLoading = summaryQuery.isPending;
  const summaryUnavailable = summaryQuery.isError;
  const abnormalBreakdown = [
    { label: t("accounts.statusCooldown"), count: cooldownAccounts, tone: "bg-amber-500/10 text-amber-700 dark:text-amber-300" },
    { label: t("accounts.waitingReset"), count: waitingResetAccounts, tone: "bg-amber-500/10 text-amber-700 dark:text-amber-300" },
    { label: t("accounts.probing"), count: probingAccounts, tone: "bg-sky-500/10 text-sky-700 dark:text-sky-300" },
    { label: t("accounts.riskFilter"), count: riskAccounts, tone: "bg-orange-500/10 text-orange-700 dark:text-orange-300" },
    { label: t("accounts.statusDisabled"), count: disabledAccounts, tone: "bg-muted text-muted-foreground" },
    { label: t("accounts.statusReauthRequired"), count: invalidAccounts, tone: "bg-red-500/10 text-red-700 dark:text-red-300" },
  ];
  const abnormalDetail = abnormalBreakdown.map((item) => `${item.label} ${formatNumber(item.count, i18n.language, 0)}`).join(" · ");
  const abnormalDetailItems = summaryUnavailable
    ? [{ label: "-", value: "", tone: "bg-muted text-muted-foreground" }]
    : abnormalBreakdown
      .filter((item) => item.count > 0)
      .map((item) => ({ ...item, value: formatNumber(item.count, i18n.language, 0) }));
  if (!summaryUnavailable && abnormalDetailItems.length === 0) {
    abnormalDetailItems.push({ label: t("accounts.statusActive"), value: "", tone: "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300", count: 0 });
  }
  const hasProviderAccounts = (summary?.providers[provider]?.total ?? 0) > 0 || (result?.total ?? 0) > 0;
  const bindableEgressNodes = (egressNodesQuery.data?.items ?? []).filter((node) => node.enabled && node.proxyConfigured && egressScopes.includes(node.scope));
  const egressFilterSearchTerm = egressFilterOptionsSearch.trim().toLocaleLowerCase();
  const secondaryEgressEnabled = Boolean(secondaryEgressScope);
  const secondaryNodePages = secondaryEgressEnabled ? (egressFilterSecondaryNodesQuery.data?.pages ?? []) : [];
  const scopedEgressNodes = [...(egressFilterNodesQuery.data?.pages ?? []), ...secondaryNodePages]
    .flatMap((nodePage) => nodePage.items)
    .filter((node) => egressScopes.includes(node.scope))
    .filter((node) => !egressFilterSearchTerm || node.name.toLocaleLowerCase().includes(egressFilterSearchTerm));
  const secondarySourcePages = secondaryEgressEnabled ? (egressFilterSecondarySourcesQuery.data?.pages ?? []) : [];
  const scopedEgressSources = [...(egressFilterSourcesQuery.data?.pages ?? []), ...secondarySourcePages]
    .flatMap((sourcePage) => sourcePage.items)
    .filter((source) => egressScopes.includes(source.scope))
    .filter((source) => !egressFilterSearchTerm || source.name.toLocaleLowerCase().includes(egressFilterSearchTerm));
  const egressFilterNodesFailed = egressFilterNodesQuery.isError || (secondaryEgressEnabled && egressFilterSecondaryNodesQuery.isError);
  const egressFilterNodesFetching = egressFilterNodesQuery.isFetching || (secondaryEgressEnabled && egressFilterSecondaryNodesQuery.isFetching);
  const egressFilterNodesHaveMore = egressFilterNodesFailed || egressFilterNodesQuery.hasNextPage || (secondaryEgressEnabled && egressFilterSecondaryNodesQuery.hasNextPage);
  const loadMoreEgressFilterNodes = () => {
    if (egressFilterNodesQuery.isError) void egressFilterNodesQuery.refetch();
    if (secondaryEgressEnabled && egressFilterSecondaryNodesQuery.isError) void egressFilterSecondaryNodesQuery.refetch();
    if (egressFilterNodesFailed) return;
    if (egressFilterNodesQuery.hasNextPage) void egressFilterNodesQuery.fetchNextPage();
    if (secondaryEgressEnabled && egressFilterSecondaryNodesQuery.hasNextPage) void egressFilterSecondaryNodesQuery.fetchNextPage();
  };
  const egressFilterSourcesFailed = egressFilterSourcesQuery.isError || (secondaryEgressEnabled && egressFilterSecondarySourcesQuery.isError);
  const egressFilterSourcesFetching = egressFilterSourcesQuery.isFetching || (secondaryEgressEnabled && egressFilterSecondarySourcesQuery.isFetching);
  const egressFilterSourcesHaveMore = egressFilterSourcesFailed || egressFilterSourcesQuery.hasNextPage || (secondaryEgressEnabled && egressFilterSecondarySourcesQuery.hasNextPage);
  const loadMoreEgressFilterSources = () => {
    if (egressFilterSourcesQuery.isError) void egressFilterSourcesQuery.refetch();
    if (secondaryEgressEnabled && egressFilterSecondarySourcesQuery.isError) void egressFilterSecondarySourcesQuery.refetch();
    if (egressFilterSourcesFailed) return;
    if (egressFilterSourcesQuery.hasNextPage) void egressFilterSourcesQuery.fetchNextPage();
    if (secondaryEgressEnabled && egressFilterSecondarySourcesQuery.hasNextPage) void egressFilterSecondarySourcesQuery.fetchNextPage();
  };
  const egressBoundGroups = [
    {
      id: "nodes", label: t("accounts.egressNodeGroup"),
      emptyLabel: egressFilterNodesFailed ? t("accounts.egressFilterOptionsLoadFailed") : egressFilterNodesFetching ? t("common.loading") : t("accounts.egressNodeGroupEmpty"),
      options: scopedEgressNodes.map((node) => ({ value: `node:${node.id}`, label: node.name })),
      loading: egressFilterNodesFetching, hasMore: egressFilterNodesHaveMore,
      actionLabel: egressFilterNodesFailed ? t("common.retry") : egressFilterNodesFetching ? t("common.loading") : t("accounts.egressFilterOptionsLoadMore"),
      onAction: loadMoreEgressFilterNodes,
    },
    {
      id: "sources", label: t("accounts.egressSourceGroup"),
      emptyLabel: egressFilterSourcesFailed ? t("accounts.egressFilterOptionsLoadFailed") : egressFilterSourcesFetching ? t("common.loading") : t("accounts.egressSourceGroupEmpty"),
      options: scopedEgressSources.map((source) => ({ value: `source:${source.id}`, label: source.name })),
      loading: egressFilterSourcesFetching, hasMore: egressFilterSourcesHaveMore,
      actionLabel: egressFilterSourcesFailed ? t("common.retry") : egressFilterSourcesFetching ? t("common.loading") : t("accounts.egressFilterSourcesLoadMore"),
      onAction: loadMoreEgressFilterSources,
    },
  ];
  const methods = connectMethods(catalogEntry?.credential);
  const typeOptions = ui.typeOptions?.(t);
  const extraFilters = ui.filters?.(t) ?? [];
  const columnCount = refreshable ? 8 : 7;

  const linkedDeleteBlock = linkedTargets.length > 0 ? (
    <div className="space-y-3 border-t pt-3">
      <LinkedTargetPicker targets={linkedTargets} checked={linkedDeleteTargets} providers={providers} slot={linkedDeleteSlot} onToggle={toggleLinkedDeleteTarget} onToggleAll={selectAllLinkedTargets} />
      <p className="min-h-4 text-xs text-muted-foreground">
        {linkedDeletePreviewError ? t("accounts.linkedDeletePreviewFailed") : t("accounts.linkedDeleteHint")}
      </p>
    </div>
  ) : null;

  return (
    <div className="space-y-5">
      <header className="flex min-h-8 items-center">
        <h1 className="text-xl font-medium">{t("accounts.title")}</h1>
        <p className="sr-only">{t("console.accountsDescription")}</p>
      </header>
      <section className="grid gap-2 sm:grid-cols-2 xl:grid-cols-[repeat(var(--account-panels),minmax(0,1fr))]" style={{ "--account-panels": providerIds.length + 1 } as CSSProperties}>
        {providerIds.map((id) => {
          const ProviderIcon = providerIcon(id);
          const counts = summary?.providers[id] ?? { total: 0, available: 0 };
          return (
            <AccountMetricPanel
              key={id}
              icon={<ProviderIcon style={{ color: providerColor(id, providers) }} />}
              loading={summaryLoading}
              label={t("accounts.providerAccountCount", { provider: providerLabel(id, providers) })}
              value={summaryUnavailable ? "-" : formatNumber(counts.total, i18n.language, 0)}
              detail={t("accounts.routableAccountCount", { count: formatNumber(counts.available, i18n.language, 0) })}
            />
          );
        })}
        <AccountMetricPanel
          tone={abnormalAccounts > 0 ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground"}
          icon={<TriangleAlert />}
          loading={summaryLoading}
          label={t("accounts.abnormalAccountCount")}
          value={summaryUnavailable ? "-" : formatNumber(abnormalAccounts, i18n.language, 0)}
          detail={abnormalDetail}
          detailItems={abnormalDetailItems}
        />
      </section>
      <div className="space-y-5">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <Tabs value={provider} onValueChange={changeProvider}>
            <TabsList>
              {providerIds.map((id) => {
                const ProviderIcon = providerIcon(id);
                return (
                  <TabsTrigger key={id} value={id} className="gap-1.5">
                    <ProviderIcon className="size-3.5" style={{ color: providerColor(id, providers) }} />
                    <span>{providerLabel(id, providers)}</span>
                  </TabsTrigger>
                );
              })}
            </TabsList>
          </Tabs>
          <DropdownMenu>
            <DropdownMenuTrigger asChild><Button size="sm"><Plus />{t("accounts.connectAccount")}</Button></DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              {methods.includes("deviceOAuth") ? <DropdownMenuItem onClick={() => setAuthOpen(true)}><ExternalLink />{t("accounts.deviceLogin")}</DropdownMenuItem> : null}
              {methods.includes("redirectOAuth") ? <DropdownMenuItem onClick={() => setAuthOpen(true)}><ExternalLink />{t("accounts.redirectLogin")}</DropdownMenuItem> : null}
              {methods.includes("import") ? (
                <>
                  <DropdownMenuItem disabled={bulkTaskPending} onClick={() => setQuickImportOpen(true)}><ClipboardPaste />{t(ui.importTexts.quickImport)}</DropdownMenuItem>
                  <DropdownMenuItem disabled={bulkTaskPending} onClick={() => fileInputRef.current?.click()}><FileUp />{t(ui.importTexts.file)}</DropdownMenuItem>
                  {ui.importTexts.formats?.map((label) => <DropdownMenuItem key={label} disabled={bulkTaskPending} onClick={() => fileInputRef.current?.click()}><FileUp />{t(label)}</DropdownMenuItem>)}
                </>
              ) : null}
              {hasProviderAccounts ? (
                <>
                  {methods.length > 0 ? <DropdownMenuSeparator /> : null}
                  <DropdownMenuItem onClick={openProviderExport}><Download />{t("accounts.exportAuth")}</DropdownMenuItem>
                </>
              ) : null}
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
        <input
          ref={fileInputRef}
          type="file"
          multiple
          accept="application/json,text/plain,.json,.jsonl,.txt"
          className="hidden"
          onChange={(event) => {
            const files = Array.from(event.target.files ?? []);
            if (files.length > 0) {
              importMutation.mutate(files);
            }
            event.target.value = "";
          }}
        />

        <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); }} placeholder={t("accounts.search")} aria-label={t("accounts.search")} />
              </div>
              <DataTableFilters filters={[
                ...(typeOptions ? [{ id: "type", label: t("accountType.label"), value: filters.type ?? "", onChange: (value: string) => setFilter("type", value), options: typeOptions }] : []),
                { id: "status", label: t("accounts.status"), value: statusFilter, onChange: (value) => { setStatusFilter(value); setPage(1); }, options: [
                  { value: "active", label: t("accounts.statusActive") },
                  { value: "disabled", label: t("accounts.statusDisabled") },
                  { value: "reauthRequired", label: t("accounts.statusReauthRequired") },
                  { value: "cooldown", label: t("accounts.statusCooldown") },
                  { value: "waitingReset", label: t("accounts.waitingReset") },
                  { value: "probing", label: t("accounts.probing") },
                ] },
                { id: "egress", label: t("accounts.egressFilter"), value: egressFilter, selectedLabel: egressFilterSelectedLabel || undefined, onChange: (value) => {
                  setEgressFilter(value);
                  setEgressFilterSelectedLabel(value.includes(":")
                    ? egressBoundGroups.flatMap((group) => group.options).find((option) => option.value === value)?.label ?? ""
                    : "");
                  setPage(1);
                }, options: [
                  {
                    value: "bound", label: t("accounts.egressBound"), groups: egressBoundGroups,
                    onGroupsOpenChange: setEgressFilterOptionsOpen,
                    groupSearch: { value: egressFilterOptionsSearch, placeholder: t("accounts.egressFilterOptionsSearch"), onChange: (value) => {
                      setEgressFilterOptionsSearch(value);
                    } },
                  },
                  { value: "unbound", label: t("accounts.egressUnbound") },
                ] },
                ...extraFilters.map((filter) => ({ id: filter.id, label: filter.label, value: filters[filter.id] ?? "", onChange: (value: string) => setFilter(filter.id, value), options: filter.options })),
              ]} />
            </div>
            {selected.size > 0 ? (
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="mr-1 text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={openSelectedExport}><Download />{t("accounts.exportAuth")}</Button>
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => batchUpdateMutation.mutate(true)}>{t("common.enable")}</Button>
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => batchUpdateMutation.mutate(false)}>{t("common.disable")}</Button>
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => {
                  setBatchMaxConcurrent("1");
                  setBatchConcurrencyOpen(true);
                }}>{t("accounts.batchSetConcurrency")}</Button>
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => {
                  setEgressNodeID("");
                  setEgressConfigurationTask("bind");
                  setEgressConfigurationOpen(true);
                }}>{t("accounts.egressConfiguration")}</Button>
                {BulkActions ? <BulkActions targets={[...selected]} ctx={ctx} /> : null}
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => {
                  if (ui.quotaReset) {
                    setBatchQuotaTask("sync");
                    setBatchQuotaTaskOpen(true);
                    return;
                  }
                  batchBillingMutation.mutate();
                }}>{t("accountCredential.quotaSyncAction")}</Button>
                {refreshable ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => batchTokenMutation.mutate()}>{t("accountCredential.refreshAction")}</Button> : null}
                <Button variant="secondary" size="sm" className="bg-destructive/10 text-destructive hover:bg-destructive/15 hover:text-destructive" disabled={bulkTaskPending} onClick={() => { resetLinkedDeleteState(); setBatchDeleteOpen(true); }}>{t("common.delete")}</Button>
              </div>
            ) : hasProviderAccounts ? (
              <div className="flex flex-wrap items-center justify-end gap-1.5">
                {BulkActions ? <BulkActions targets="all" ctx={ctx} /> : null}
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => { setAllQuotaTask("sync"); setSyncAllOpen(true); }}>{t("accountCredential.quotaSyncAction")}</Button>
                {ui.renewAll ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => setRenewAllOpen(true)}>{t("accountCredential.refreshAction")}</Button> : null}
                <Button variant="secondary" size="sm" className="bg-destructive/10 text-destructive hover:bg-destructive/15 hover:text-destructive" disabled={bulkTaskPending} onClick={() => { resetCleanupState(); setCleanupOpen(true); }}><Trash2 />{t("accounts.cleanupAction")}</Button>
              </div>
            ) : null}
          </>
        )}
        footer={result && result.total > 0 ? <Pagination page={result.page} pageSize={result.pageSize} total={result.total} onPageChange={setPage} onPageSizeChange={(value) => { setPageSize(value); setPage(1); }} /> : undefined}
      >
        {accountsQuery.isError ? <ErrorState message={accountsQuery.error.message} onRetry={() => void accountsQuery.refetch()} /> : null}
        {result && result.items.length === 0 ? <EmptyState /> : null}
        {accountsQuery.isPending || (result && result.items.length > 0) ? (
          <Table viewportRows={20} rowHeight={56} className="table-fixed border-collapse min-w-[780px] xl:min-w-[960px] 2xl:min-w-[1080px]">
            <colgroup>
              <col style={{ width: "3%" }} />
              <col style={{ width: "18%" }} />
              <col style={{ width: "7%" }} />
              <col style={{ width: "7%" }} />
              <col style={{ width: refreshable ? "27%" : "43%" }} />
              {refreshable ? <col style={{ width: "16%" }} /> : null}
              <col style={{ width: "18%" }} />
              <col style={{ width: "4%" }} />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="px-2"><Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} /></TableHead>
                <SortableTableHead field="name" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("accounts.account")}</SortableTableHead>
                <SortableTableHead field="type" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort} className="whitespace-nowrap">{t("accountType.label")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort} className="whitespace-nowrap">{t("accounts.status")}</SortableTableHead>
                <TableHead className={cn("whitespace-nowrap", !refreshable && "px-6")}>{t("accounts.quota")}</TableHead>
                {refreshable ? <TableHead className="whitespace-nowrap pl-4">{t("accountCredential.label")}</TableHead> : null}
                <SortableTableHead field="createdAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort} className="whitespace-nowrap">{t("accounts.createdAt")}</SortableTableHead>
                <TableActionHead />
              </TableRow>
            </TableHeader>
            {accountsQuery.isPending ? (
              <TableBody><TableLoadingRow colSpan={columnCount} /></TableBody>
            ) : (
              <VirtualTableBody
                items={result?.items ?? []}
                colSpan={columnCount}
                rowHeight={56}
                renderRow={(account) => (
                  <TableRow className="group h-14 [&>td]:py-1.5" key={account.id} data-state={selected.has(account.id) ? "selected" : undefined}>
                    <TableCell className="px-2"><Checkbox checked={selected.has(account.id)} onCheckedChange={(checked) => toggleAccount(account.id, checked === true)} aria-label={t("common.selectItem", { name: account.name })} /></TableCell>
                    <TableCell className="min-w-0"><AccountNameCell account={account} /></TableCell>
                    <TableCell className="text-center whitespace-nowrap"><TypeCell account={account} /></TableCell>
                    <TableCell className="text-center whitespace-nowrap"><AccountStatus account={account} blockingQuotaMode={ui.blockingQuotaMode} /></TableCell>
                    <TableCell className={refreshable ? undefined : "px-6"}><QuotaCell account={account} locale={i18n.language} quotaModes={catalogEntry?.quotaModes ?? []} /></TableCell>
                    {refreshable ? <TableCell className="whitespace-nowrap pl-4 text-xs">
                      {account.credentialKind ? (
                        <Tooltip>
                          <TooltipTrigger asChild><span tabIndex={0} className="block cursor-help text-muted-foreground">{t(`accountCredential.kinds.${account.credentialKind}`, { defaultValue: account.credentialKind })}</span></TooltipTrigger>
                          <TooltipContent>{t(`accountCredential.kindHints.${account.credentialKind}`, { defaultValue: account.credentialKind })}</TooltipContent>
                        </Tooltip>
                      ) : null}
                      {account.refreshable ? (
                        <Tooltip>
                          <TooltipTrigger asChild><span tabIndex={0} className="cursor-help font-medium text-emerald-700 dark:text-emerald-300">{t("accountCredential.autoRefresh")}</span></TooltipTrigger>
                          <TooltipContent>{account.expiresAt ? t("accountCredential.expiresAt", { time: formatDateTime(account.expiresAt, i18n.language) }) : t("accountCredential.expiryUnknown")}</TooltipContent>
                        </Tooltip>
                      ) : <span className="font-medium text-amber-700 dark:text-amber-300">{t("accountCredential.noAutoRefresh")}</span>}
                    </TableCell> : null}
                    <TableCell className="whitespace-nowrap text-xs text-muted-foreground">{formatDateTime(account.createdAt, i18n.language)}</TableCell>
                    <TableActionCell>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild><Button variant="ghost" size="icon" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem onClick={() => beginEdit(account)}><Pencil />{t("common.edit")}</DropdownMenuItem>
                          {RowMenuItems ? <RowMenuItems account={account} ctx={ctx} /> : null}
                          {refreshable ? <DropdownMenuItem onClick={() => tokenMutation.mutate(account.id)}><RotateCw />{t("accounts.refreshToken")}</DropdownMenuItem> : null}
                          {account.cooldownUntil && new Date(account.cooldownUntil) > new Date() ? (
                            <DropdownMenuItem onClick={() => clearCooldownMutation.mutate(account.id)} disabled={clearCooldownMutation.isPending}>
                              <TimerOff />{t("accounts.clearCooldown")}
                            </DropdownMenuItem>
                          ) : null}
                          <DropdownMenuItem onClick={() => quotaMutation.mutate(account.id)}><RefreshCw />{t("accounts.refreshModeQuota")}</DropdownMenuItem>
                          <DropdownMenuSeparator />
                          <DropdownMenuItem className="text-destructive focus:text-destructive" onClick={() => { resetLinkedDeleteState(); setDeleting(account); }}><Trash2 />{t("common.delete")}</DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableActionCell>
                  </TableRow>
                )}
              />
            )}
          </Table>
        ) : null}
        </DataTableShell>
      </div>

      {Dialogs ? <Dialogs ctx={ctx} /> : null}

      {authOpen ? <AccountAuthDialog provider={provider} providerName={providerName} help={AuthHelp ? <AuthHelp /> : undefined} onClose={() => setAuthOpen(false)} /> : null}

      <AlertDialog open={syncAllOpen} onOpenChange={(open) => {
        // 额度同步可以中途取消（与续期一致）；重置一旦发出就等它完成。
        if (allQuotaResetMutation.isPending) return;
        if (!open) quotaSyncAbortRef.current?.abort();
        setSyncAllOpen(open);
      }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(ui.quotaReset ? "accountQuotaTask.allTitle" : "accounts.syncAllTitle")}</AlertDialogTitle>
            <AlertDialogDescription>{t(ui.syncAllDescription, { provider: providerName })}</AlertDialogDescription>
          </AlertDialogHeader>
          {ui.quotaReset ? (
            <div className="space-y-3">
              <Tabs value={allQuotaTask} onValueChange={(value) => setAllQuotaTask(value as QuotaTask)}>
                <TabsList className="grid h-10 w-full grid-cols-2 p-1">
                  <TabsTrigger value="sync" className="h-8 font-normal" disabled={quotaSyncMutation.isPending || allQuotaResetMutation.isPending}>{t("accounts.refreshBilling")}</TabsTrigger>
                  <TabsTrigger value="reset" className="h-8 font-normal" disabled={quotaSyncMutation.isPending || allQuotaResetMutation.isPending}>{t("accountQuotaReset.action")}</TabsTrigger>
                </TabsList>
              </Tabs>
              <p className="min-h-10 text-xs leading-5 text-muted-foreground">{t(allQuotaTask === "sync" ? "accounts.syncAllDescription" : "accountQuotaTask.resetAllDescription")}</p>
            </div>
          ) : null}
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction disabled={quotaSyncMutation.isPending || allQuotaResetMutation.isPending} onClick={(event) => {
              event.preventDefault();
              if (ui.quotaReset && allQuotaTask === "reset") allQuotaResetMutation.mutate();
              else quotaSyncMutation.mutate();
            }}>
              {quotaSyncMutation.isPending ? <><Spinner />{quotaSyncProgress ? <span className="tabular-nums">{quotaSyncProgress.completed} / {quotaSyncProgress.total}</span> : t("common.loading")}</> : allQuotaResetMutation.isPending ? <Spinner /> : t(ui.quotaReset ? "accountQuotaTask.execute" : "accounts.syncAll")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={renewAllOpen} onOpenChange={(open) => { if (!open) renewalAbortRef.current?.abort(); setRenewAllOpen(open); }}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.renewAllTitle")}</AlertDialogTitle><AlertDialogDescription>{t("accounts.renewAllDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction disabled={allTokenMutation.isPending} onClick={(event) => { event.preventDefault(); allTokenMutation.mutate(); }}>{allTokenMutation.isPending ? <><Spinner />{renewalProgress ? <span className="tabular-nums">{renewalProgress.completed} / {renewalProgress.total}</span> : t("common.loading")}</> : t("accounts.renewAll")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={exportOpen} onOpenChange={(open) => { if (!open && !exportMutation.isPending) setExportOpen(false); }}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.exportTitle", { provider: providerName })}</AlertDialogTitle><AlertDialogDescription>{t("accounts.exportDescription")}</AlertDialogDescription></AlertDialogHeader>
          {selected.size > 0 ? <p className="text-sm text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</p> : <div className="grid gap-2">
            <Label htmlFor="account-export-limit">{t("accounts.exportCount")}</Label>
            <Input id="account-export-limit" type="number" min={1} max={10000} value={exportLimit} disabled={exportSnapshotMaxId !== "0"} onChange={(event) => setExportLimit(event.target.value)} />
            <p className="text-xs text-muted-foreground">{t("accountExport.countDescription")}</p>
            {exportCompletedCount > 0 ? <p className="text-sm text-muted-foreground">{t("accountExport.batchProgress", { count: exportCompletedCount, batch: exportBatchNumber })}</p> : null}
          </div>}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={exportMutation.isPending}>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              disabled={exportMutation.isPending || (selected.size === 0 && (!Number.isInteger(Number(exportLimit)) || Number(exportLimit) < 1 || Number(exportLimit) > 10000))}
              onClick={(event) => {
                event.preventDefault();
                if (selected.size > 0) {
                  exportMutation.mutate({ kind: "selected", ids: [...selected] });
                  return;
                }
                exportMutation.mutate({ kind: "batch", limit: Number(exportLimit), afterId: exportCursor, snapshotMaxId: exportSnapshotMaxId, batchNumber: exportBatchNumber });
              }}
            >
              {exportMutation.isPending ? <Spinner /> : null}
              {selected.size === 0 && exportCompletedCount > 0 ? t("accountExport.nextBatch") : t("accounts.exportAuth")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog open={quickImportOpen} onOpenChange={(open) => { setQuickImportOpen(open); if (!open) { setQuickImportTokens(""); if (quickImportFileInputRef.current) quickImportFileInputRef.current.value = ""; } }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t(ui.importTexts.title, { provider: providerName })}</DialogTitle>
            <DialogDescription>{t(ui.importTexts.description, { provider: providerName })}</DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <div className="flex items-center justify-between gap-3">
              <Label htmlFor="quick-account-tokens">{t(ui.importTexts.tokens)}</Label>
              <Button type="button" variant="secondary" size="sm" disabled={importMutation.isPending} onClick={() => quickImportFileInputRef.current?.click()}><FileUp />{t("accounts.uploadTXT")}</Button>
              <input
                ref={quickImportFileInputRef}
                type="file"
                accept=".txt,.json,.jsonl"
                className="hidden"
                onChange={(event) => {
                  void loadQuickImportFile(event.target.files?.[0]);
                  event.target.value = "";
                }}
              />
            </div>
            <Textarea
              id="quick-account-tokens"
              className="min-h-56 font-mono"
              autoComplete="off"
              spellCheck={false}
              value={quickImportTokens}
              onChange={(event) => setQuickImportTokens(event.target.value)}
              placeholder={t(ui.importTexts.placeholder)}
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="secondary" size="sm" onClick={() => { setQuickImportOpen(false); setQuickImportTokens(""); }}>{t("common.cancel")}</Button>
            <Button type="button" size="sm" disabled={!quickImportTokens.trim() || importMutation.isPending} onClick={submitQuickImport}>{importMutation.isPending ? <Spinner /> : null}{t("accounts.importAction")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={Boolean(editing)} onOpenChange={(open) => !open && setEditing(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("common.edit")} {editing?.name}</DialogTitle>
            <DialogDescription>{editing?.email ?? editing?.userId}</DialogDescription>
          </DialogHeader>
          <form className="space-y-4" onSubmit={form.handleSubmit((values) => updateMutation.mutate(values))}>
            <div className="space-y-2"><Label htmlFor="account-name">{t("accounts.name")}</Label><Input id="account-name" {...form.register("name")} />{form.formState.errors.name ? <p className="text-xs text-destructive">{form.formState.errors.name.message}</p> : null}</div>
            <div className="flex items-center justify-between border-b py-2"><Label htmlFor="account-enabled">{accountEnabled ? t("common.enabled") : t("common.disabled")}</Label><Switch id="account-enabled" checked={accountEnabled} onCheckedChange={(checked) => form.setValue("enabled", checked)} /></div>
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-2"><Label htmlFor="account-priority">{t("accounts.priority")}</Label><Input id="account-priority" type="number" {...form.register("priority", { valueAsNumber: true })} /></div>
              <div className="space-y-2"><Label htmlFor="account-concurrency">{t("accounts.maxConcurrent")}</Label><Input id="account-concurrency" type="number" min="1" max="256" {...form.register("maxConcurrent", { valueAsNumber: true })} /></div>
            </div>
            <div className="space-y-2"><Label htmlFor="account-minimum">{t("accounts.minimumRemaining")}</Label><Input id="account-minimum" type="number" min="0" step="0.01" {...form.register("minimumRemaining", { valueAsNumber: true })} /></div>
            {editing && EditFields ? <EditFields account={editing} form={form} /> : null}
            <DialogFooter><Button type="button" variant="secondary" size="sm" onClick={() => setEditing(null)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={updateMutation.isPending}>{updateMutation.isPending ? <Spinner /> : null}{t("common.save")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <AlertDialog open={Boolean(deleting)} onOpenChange={(open) => {
        if (!open) {
          // Do not clear linked targets while a delete request is in flight.
          if (deleteMutation.isPending) return;
          setDeleting(null);
          resetLinkedDeleteState();
        }
      }}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.deleteTitle")}</AlertDialogTitle><AlertDialogDescription>{t("accounts.deleteDescription")}</AlertDialogDescription></AlertDialogHeader>
          {linkedDeleteBlock}
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={deleteMutation.isPending || !deleting || linkedPreviewBlocking} onClick={(event) => {
              event.preventDefault();
              if (!deleting || linkedPreviewBlocking) return;
              deleteMutation.mutate({ id: deleting.id, provider, linkedDeleteTargets: [...linkedDeleteTargets] });
            }}>{t("accounts.deleteConfirm")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog open={batchConcurrencyOpen} onOpenChange={(open) => {
        if (!open && batchConcurrencyMutation.isPending) return;
        setBatchConcurrencyOpen(open);
      }}>
        <DialogContent className="sm:max-w-[460px]">
          <DialogHeader>
            <DialogTitle>{t("accounts.batchConcurrencyTitle", { count: selected.size })}</DialogTitle>
            <DialogDescription>{t("accounts.batchConcurrencyDescription")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Label htmlFor="batch-account-concurrency">{t("accounts.maxConcurrent")}</Label>
            <Input
              id="batch-account-concurrency"
              type="number"
              min="1"
              max="256"
              value={batchMaxConcurrent}
              disabled={batchConcurrencyMutation.isPending}
              onChange={(event) => setBatchMaxConcurrent(event.target.value)}
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="secondary" size="sm" disabled={batchConcurrencyMutation.isPending} onClick={() => setBatchConcurrencyOpen(false)}>{t("common.cancel")}</Button>
            <Button
              type="button"
              size="sm"
              disabled={batchConcurrencyMutation.isPending || !Number.isInteger(Number(batchMaxConcurrent)) || Number(batchMaxConcurrent) < 1 || Number(batchMaxConcurrent) > 256}
              onClick={() => batchConcurrencyMutation.mutate(Number(batchMaxConcurrent))}
            >
              {batchConcurrencyMutation.isPending ? <Spinner /> : null}
              {t("common.save")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog open={batchDeleteOpen} onOpenChange={(open) => {
        if (!open && batchDeleteMutation.isPending) return;
        setBatchDeleteOpen(open);
        if (!open) resetLinkedDeleteState();
      }}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.batchDeleteTitle", { count: selected.size })}</AlertDialogTitle><AlertDialogDescription>{t("accounts.deleteDescription")}</AlertDialogDescription></AlertDialogHeader>
          {linkedDeleteBlock}
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={batchDeleteMutation.isPending || selected.size === 0 || linkedPreviewBlocking} onClick={(event) => {
              event.preventDefault();
              if (linkedPreviewBlocking) return;
              batchDeleteMutation.mutate({ ids: [...selected], provider, linkedDeleteTargets: [...linkedDeleteTargets] });
            }}>{t("accounts.deleteConfirm")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={batchQuotaTaskOpen} onOpenChange={(open) => {
        if (batchBillingMutation.isPending || batchQuotaResetMutation.isPending) return;
        setBatchQuotaTaskOpen(open);
      }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("accountQuotaTask.title", { count: selected.size })}</AlertDialogTitle>
            <AlertDialogDescription>{t("accountQuotaTask.description")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="space-y-3">
            <Tabs value={batchQuotaTask} onValueChange={(value) => setBatchQuotaTask(value as QuotaTask)}>
              <TabsList className="grid h-10 w-full grid-cols-2 p-1">
                <TabsTrigger value="sync" className="h-8 font-normal" disabled={batchBillingMutation.isPending || batchQuotaResetMutation.isPending}>{t("accounts.refreshBilling")}</TabsTrigger>
                <TabsTrigger value="reset" className="h-8 font-normal" disabled={batchBillingMutation.isPending || batchQuotaResetMutation.isPending}>{t("accountQuotaReset.action")}</TabsTrigger>
              </TabsList>
            </Tabs>
            <p className="min-h-10 text-xs leading-5 text-muted-foreground">{t(batchQuotaTask === "sync" ? "accountQuotaTask.syncDescription" : "accountQuotaReset.description")}</p>
          </div>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction disabled={batchBillingMutation.isPending || batchQuotaResetMutation.isPending} onClick={(event) => {
              event.preventDefault();
              if (batchQuotaTask === "reset") batchQuotaResetMutation.mutate();
              else batchBillingMutation.mutate();
            }}>
              {batchBillingMutation.isPending || batchQuotaResetMutation.isPending ? <Spinner /> : null}
              {t("accountQuotaTask.execute")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog open={egressConfigurationOpen} onOpenChange={(open) => {
        if (bindEgressMutation.isPending || unbindEgressMutation.isPending) return;
        setEgressConfigurationOpen(open);
        if (!open) {
          setEgressConfigurationTask("bind");
          setEgressNodeID("");
        }
      }}>
        <DialogContent className="sm:max-w-[460px]">
          <DialogHeader>
            <DialogTitle>{t("accounts.egressConfigurationTitle", { count: selected.size })}</DialogTitle>
            <DialogDescription>{t("accounts.egressConfigurationDescription")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <Tabs value={egressConfigurationTask} onValueChange={(value) => setEgressConfigurationTask(value as EgressConfigurationTask)}>
              <TabsList className="grid h-10 w-full grid-cols-2 p-1">
                <TabsTrigger value="bind" className="h-8 font-normal" disabled={bindEgressMutation.isPending || unbindEgressMutation.isPending}>{t("accounts.bindEgress")}</TabsTrigger>
                <TabsTrigger value="unbind" className="h-8 font-normal" disabled={bindEgressMutation.isPending || unbindEgressMutation.isPending}>{t("accounts.unbindEgress")}</TabsTrigger>
              </TabsList>
            </Tabs>
            {egressConfigurationTask === "bind" ? (
              <div className="min-h-20">
                {egressNodesQuery.isPending ? <div className="flex min-h-20 items-center justify-center"><Spinner /></div> : null}
                {egressNodesQuery.isError ? <p className="text-sm text-destructive">{egressNodesQuery.error.message}</p> : null}
                {!egressNodesQuery.isPending && !egressNodesQuery.isError ? (
                  bindableEgressNodes.length > 0 ? (
                    <div className="space-y-2">
                      <Label htmlFor="account-egress-node">{t("accounts.bindEgressNode")}</Label>
                      <Select value={egressNodeID} onValueChange={setEgressNodeID} disabled={bindEgressMutation.isPending}>
                        <SelectTrigger id="account-egress-node"><SelectValue placeholder={t("accounts.bindEgressEmpty")} /></SelectTrigger>
                        <SelectContent>
                          {bindableEgressNodes.map((node) => (
                            <SelectItem key={node.id} value={node.id}>
                              {node.name} ({node.assignedAccountCount}{node.accountCapacity > 0 ? ` / ${node.accountCapacity}` : ` / ${t("settings.egress.unlimited")}`})
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                    </div>
                  ) : <p className="text-xs leading-5 text-muted-foreground">{t("accounts.bindEgressNoNodes")}</p>
                ) : null}
              </div>
            ) : <p className="min-h-20 text-xs leading-5 text-muted-foreground">{t("accounts.unbindEgressDescription")}</p>}
          </div>
          <DialogFooter>
            <Button type="button" variant="secondary" size="sm" disabled={bindEgressMutation.isPending || unbindEgressMutation.isPending} onClick={() => setEgressConfigurationOpen(false)}>{t("common.cancel")}</Button>
            <Button
              type="button"
              size="sm"
              disabled={bindEgressMutation.isPending || unbindEgressMutation.isPending || (egressConfigurationTask === "bind" && (!egressNodeID || egressNodesQuery.isPending || egressNodesQuery.isError))}
              onClick={() => {
                if (egressConfigurationTask === "bind") bindEgressMutation.mutate();
                else unbindEgressMutation.mutate();
              }}
            >
              {bindEgressMutation.isPending || unbindEgressMutation.isPending ? <Spinner /> : null}
              {t("accountQuotaTask.execute")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={cleanupOpen} onOpenChange={(open) => { if (!cleanupMutation.isPending) { setCleanupOpen(open); if (!open) resetCleanupState(); } }}>
        <DialogContent className="max-w-[440px]">
          <DialogHeader>
            <DialogTitle>{t("accounts.cleanupTitle", { provider: providerName })}</DialogTitle>
            <DialogDescription>{t("accounts.cleanupDescription")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-1.5">
            {([
              ["cooldown", t("accounts.statusCooldown")],
              ["disabled", t("accounts.statusDisabled")],
              ["reauthRequired", t("accounts.statusReauthRequired")],
            ] as const).map(([status, label]) => {
              const checked = cleanupStatuses.has(status);
              const pending = checked && !cleanupPreviewError && !cleanupPreviewFresh;
              return (
                <label key={status} className="flex cursor-pointer items-center gap-3 rounded-md bg-muted/40 px-3 py-2.5 text-xs">
                  <Checkbox
                    checked={checked}
                    disabled={cleanupMutation.isPending}
                    onCheckedChange={(value) => {
                      setCleanupStatuses((current) => {
                        const next = new Set(current);
                        if (value === true) next.add(status); else next.delete(status);
                        return next;
                      });
                      setCleanupPreviewError(false);
                    }}
                  />
                  <span>{label}</span>
                  {/* Fixed count slot: spinner while previewing, then the matched root count. */}
                  <span
                    className={cn(
                      "ml-auto inline-flex h-4 min-w-[2.5rem] items-center justify-end tabular-nums text-xs",
                      cleanupPreviewError ? "text-destructive" : "text-muted-foreground",
                      !checked && "invisible",
                    )}
                    aria-hidden={!checked}
                    aria-busy={pending}
                  >
                    {!checked ? null : cleanupPreviewError ? "!" : pending ? <Spinner className="size-3.5" /> : cleanupPreviewTotals?.rootsByStatus?.[status] ?? 0}
                  </span>
                </label>
              );
            })}
          </div>

          {/* Smooth-expand linked deletion block, shown once any status is selected. */}
          <div
            className={cn(
              "grid transition-all duration-300 ease-in-out",
              cleanupStatuses.size > 0 ? "grid-rows-[1fr] opacity-100" : "grid-rows-[0fr] opacity-0",
            )}
            aria-hidden={cleanupStatuses.size === 0}
          >
            <div className="overflow-hidden">
              <div className="space-y-3 border-t pt-3">
                {linkedTargets.length > 0 ? (
                  <>
                    <LinkedTargetPicker
                      targets={linkedTargets}
                      checked={cleanupLinkedTargets}
                      disabled={cleanupMutation.isPending}
                      providers={providers}
                      slot={(target) => {
                        const checked = cleanupLinkedTargets.includes(target);
                        const pending = checked && !cleanupPreviewError && !cleanupPreviewFresh;
                        const label = !checked ? "" : cleanupPreviewError ? t("accounts.linkedDeleteExtraFailed") : t("accounts.linkedDeleteExtra", { count: cleanupPreviewTotals?.linkedByProvider?.[target] ?? 0 });
                        return { pending, failed: cleanupPreviewError, label };
                      }}
                      onToggle={toggleCleanupTarget}
                      onToggleAll={selectAllCleanupTargets}
                    />
                    {/* Stacked messages: the container keeps the tallest variant's height,
                        so switching hint/warning/error never resizes the dialog. */}
                    <div className="grid text-xs">
                      {([
                        ["error", cleanupPreviewError, t("accounts.cleanupPreviewFailed"), "text-destructive"],
                        ["warning", !cleanupPreviewError && cleanupLinkedTargets.length > 0, t("accounts.cleanupLinkedWarning"), "text-destructive"],
                        ["hint", !cleanupPreviewError && cleanupLinkedTargets.length === 0, t("accounts.linkedDeleteHint"), "text-muted-foreground"],
                      ] as const).map(([key, visible, text, tone]) => (
                        <p key={key} aria-hidden={!visible} className={cn("col-start-1 row-start-1", tone, !visible && "invisible")}>{text}</p>
                      ))}
                    </div>
                  </>
                ) : null}
                {/* Always rendered so the total line never unmounts between refreshes. */}
                <p className="flex min-h-4 items-center gap-1.5 text-xs text-muted-foreground" aria-busy={!cleanupPreviewFresh && !cleanupPreviewError}>
                  {cleanupPreviewError ? t("accounts.cleanupPreviewFailed") : !cleanupPreviewFresh ? <Spinner className="size-3.5" /> : t("accounts.cleanupPreviewTotal", { total: cleanupPreviewTotals?.total ?? 0 })}
                </p>
              </div>
            </div>
          </div>

          <DialogFooter>
            <Button type="button" variant="secondary" size="sm" disabled={cleanupMutation.isPending} onClick={() => setCleanupOpen(false)}>{t("common.cancel")}</Button>
            <Button
              type="button"
              variant="destructive"
              size="sm"
              disabled={cleanupMutation.isPending || cleanupStatuses.size === 0 || cleanupPreviewError || !cleanupPreviewFresh}
              onClick={() => cleanupMutation.mutate({ statuses: [...cleanupStatuses], targets: [...cleanupLinkedTargets] })}
            >
              {cleanupMutation.isPending ? <Spinner /> : null}
              {t("accounts.cleanupStart")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function downloadAccountExport(blob: Blob, provider: AccountProvider, suffix: string): void {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = `grok2api-${provider.replaceAll("_", "-")}-accounts-${suffix}-${new Date().toISOString().slice(0, 10)}.json`;
  anchor.click();
  window.setTimeout(() => URL.revokeObjectURL(url), 0);
}

// Linked-provider checkboxes with a fixed-width count slot: spinner while previewing, then +N — never a fake +0.
function LinkedTargetPicker({ targets, checked, disabled, providers, slot, onToggle, onToggleAll }: {
  targets: readonly string[];
  checked: readonly string[];
  disabled?: boolean;
  providers?: ProviderDTO[];
  slot: (target: string) => { pending: boolean; failed: boolean; label: string };
  onToggle: (target: string, checked: boolean) => void;
  onToggleAll: () => void;
}) {
  const { t } = useTranslation();
  return (
    <>
      <div className="flex items-center justify-between gap-3">
        <p className="text-sm font-medium">{t("accounts.linkedDeleteTitle")}</p>
        <Button type="button" variant="ghost" size="sm" className="h-7 px-2 text-xs" disabled={disabled} onClick={onToggleAll}>
          {targets.every((item) => checked.includes(item)) ? t("accounts.linkedDeleteClearAll") : t("accounts.linkedDeleteSelectAll")}
        </Button>
      </div>
      <div className="flex flex-wrap gap-x-6 gap-y-2">
        {targets.map((target) => {
          const isChecked = checked.includes(target);
          const { pending, failed, label } = slot(target);
          const TargetIcon = providerIcon(target);
          return (
            <label key={target} className="flex min-h-6 items-center gap-2 text-sm">
              <Checkbox checked={isChecked} disabled={disabled} onCheckedChange={(value) => onToggle(target, value === true)} />
              <TargetIcon className="size-3.5 shrink-0" style={{ color: providerColor(target, providers) }} aria-hidden />
              <span className="inline-flex min-w-0 items-center gap-1.5">
                <span>{providerLabel(target, providers)}</span>
                <span
                  className={cn("inline-flex h-4 min-w-[2.75rem] items-center justify-start tabular-nums text-xs", failed ? "text-destructive" : "text-muted-foreground", !isChecked && "invisible")}
                  aria-hidden={!isChecked}
                  aria-busy={pending}
                >
                  {pending ? <Spinner className="size-3.5" /> : label}
                </span>
              </span>
            </label>
          );
        })}
      </div>
    </>
  );
}

function AccountMetricPanel({ icon, label, value, detail, detailItems, loading, tone }: {
  icon: ReactNode;
  label: string;
  value: string;
  detail: string;
  detailItems?: Array<{ label: string; value: string; tone?: string }>;
  loading: boolean;
  tone?: string;
}) {
  return (
    <div className="min-h-28 rounded-lg bg-card p-4" aria-busy={loading}>
      <div className="flex min-h-5 items-center justify-between gap-3">
        <span className="text-xs text-muted-foreground">{label}</span>
        <span className={cn("flex size-5 items-center justify-center [&_svg]:size-4", tone)}>{icon}</span>
      </div>
      <div className="mt-3 flex min-h-8 items-center text-2xl font-medium tracking-tight tabular-nums">{loading ? <Spinner /> : value}</div>
      {detailItems ? (
        <div className={cn("-ml-1.5 mt-1.5 flex min-h-5 flex-wrap gap-1 text-[11px] leading-4", loading && "invisible")} title={detail}>
          {detailItems.map((item) => (
            <span key={item.label} className={cn("inline-flex shrink-0 items-baseline gap-1 whitespace-nowrap rounded-md px-1.5 py-0.5", item.tone ?? "bg-muted text-muted-foreground")}>
              <span>{item.label}</span>
              {item.value ? <span className="font-medium tabular-nums">{item.value}</span> : null}
            </span>
          ))}
        </div>
      ) : (
        <p className={cn("mt-1.5 min-h-4 truncate text-[11px] text-muted-foreground", loading && "invisible")} title={detail}>{detail}</p>
      )}
    </div>
  );
}

function AccountStatus({ account, blockingQuotaMode }: { account: AccountDTO; blockingQuotaMode?: string }) {
  const { t, i18n } = useTranslation();
  if (!account.enabled) {
    return <Badge variant="outline" className="text-muted-foreground">{t("accounts.statusDisabled")}</Badge>;
  }
  if (account.authStatus === "reauthRequired") {
    const refreshErrorDetails = formatAdditionalRefreshErrorDetails(account);
    const hasRefreshError = Boolean(account.lastRefreshErrorStatus || account.lastRefreshErrorCode || account.lastRefreshErrorMessage || refreshErrorDetails);
    if (!hasRefreshError) return <Badge variant="destructive">{t("accounts.statusReauthRequired")}</Badge>;
    return (
      <StatusTooltip content={(
        <div className="grid w-72 max-w-[calc(100vw-2rem)] grid-cols-[4.5rem_minmax(0,1fr)] gap-x-3 gap-y-1 text-xs font-normal leading-5">
          {account.lastRefreshErrorStatus ? <><span className="text-primary-foreground/60">{t("accounts.refreshErrorStatus")}</span><span>{account.lastRefreshErrorStatus}</span></> : null}
          {account.lastRefreshErrorCode ? <><span className="text-primary-foreground/60">{t("accounts.refreshErrorCode")}</span><span className="break-all">{account.lastRefreshErrorCode}</span></> : null}
          {account.lastRefreshErrorMessage ? <><span className="text-primary-foreground/60">{t("accounts.refreshErrorMessage")}</span><span className="break-words">{account.lastRefreshErrorMessage}</span></> : null}
          {refreshErrorDetails ? <><span className="text-primary-foreground/60">{t("accounts.refreshErrorResponse")}</span><span className="max-h-40 overflow-auto whitespace-pre-wrap break-all">{refreshErrorDetails}</span></> : null}
        </div>
      )}>
        <Badge variant="destructive">{t("accounts.statusReauthRequired")}</Badge>
      </StatusTooltip>
    );
  }
  const blockingWindow = blockingQuotaMode
    ? account.quotaWindows?.find((window) => window.mode === blockingQuotaMode && window.remaining <= 0)
    : undefined;
  if (blockingWindow) {
    const detail = blockingWindow.resetAt
      ? t("accounts.quotaResetAt", { time: formatDateTime(blockingWindow.resetAt, i18n.language) })
      : t("accounts.quotaResetUnknown");
    return (
      <StatusTooltip content={detail}>
        <Badge variant="secondary" className="bg-amber-500/10 text-amber-700 dark:text-amber-300">{t("accounts.waitingReset")}</Badge>
      </StatusTooltip>
    );
  }
  if (account.quota.status === "waitingReset") {
    const detail = account.quota.nextProbeAt
      ? t(account.quota.type === "paid" ? "accounts.paidWaitingResetUntil" : "accounts.waitingResetUntil", { time: formatDateTime(account.quota.nextProbeAt, i18n.language) })
      : t("accounts.quotaResetUnknown");
    return (
      <StatusTooltip content={detail}>
        <Badge variant="secondary" className="bg-amber-500/10 text-amber-700 dark:text-amber-300">{t("accounts.waitingReset")}</Badge>
      </StatusTooltip>
    );
  }
  if (account.quota.status === "probing") {
    return (
      <StatusTooltip content={t(account.quota.type === "paid" ? "accounts.paidProbingQuota" : "accounts.probingQuota")}>
        <Badge variant="secondary" className="bg-sky-500/10 text-sky-700 dark:text-sky-300">{t("accounts.probing")}</Badge>
      </StatusTooltip>
    );
  }
  if (account.cooldownUntil && new Date(account.cooldownUntil) > new Date()) {
    return <Badge variant="secondary" className="bg-amber-500/10 text-amber-700 dark:text-amber-300">{t("accounts.statusCooldown")}</Badge>;
  }
  return <Badge variant="secondary" className="bg-emerald-500/10 text-emerald-700 dark:text-emerald-300">{t("accounts.statusActive")}</Badge>;
}

function formatAdditionalRefreshErrorDetails(account: AccountDTO): string | undefined {
  const response = account.lastRefreshErrorResponse?.trim();
  if (!response) return undefined;
  try {
    const parsed: unknown = JSON.parse(response);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return response;
    const details = { ...(parsed as Record<string, unknown>) };
    const messages = new Set((account.lastRefreshErrorMessage ?? "").split(" · ").map((value) => value.trim()).filter(Boolean));
    if (typeof details.error === "string" && details.error === account.lastRefreshErrorCode) delete details.error;
    for (const key of ["error_description", "message", "detail", "description", "title"]) {
      if (typeof details[key] === "string" && messages.has(details[key])) delete details[key];
    }
    if (details.error && typeof details.error === "object" && !Array.isArray(details.error)) {
      const nested = { ...(details.error as Record<string, unknown>) };
      if (typeof nested.code === "string" && nested.code === account.lastRefreshErrorCode) delete nested.code;
      for (const key of ["error_description", "message", "detail", "description"]) {
        if (typeof nested[key] === "string" && messages.has(nested[key])) delete nested[key];
      }
      if (Object.keys(nested).length === 0) delete details.error;
      else details.error = nested;
    }
    if (Object.keys(details).length === 0) return undefined;
    return JSON.stringify(details, null, 2);
  } catch {
    return response;
  }
}

function StatusTooltip({ children, content }: { children: ReactNode; content: ReactNode }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span tabIndex={0} className="inline-flex cursor-help">{children}</span>
      </TooltipTrigger>
      <TooltipContent className="w-max max-w-sm">{content}</TooltipContent>
    </Tooltip>
  );
}
