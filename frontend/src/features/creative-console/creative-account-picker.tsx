import { useQuery } from "@tanstack/react-query";
import { Check, ChevronDown, ChevronLeft, ChevronRight, Search, UserRound } from "lucide-react";
import { useId, useRef, useState, type KeyboardEvent } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Spinner } from "@/components/ui/spinner";
import { useProviders } from "@/entities/provider/provider-api";
import { providerLabel } from "@/entities/provider/provider-presentation";
import { listAccounts, type AccountDTO } from "@/features/accounts/accounts-api";
import { accountDisplayName } from "@/features/accounts/account-identity";
import { canSelectCreativeAccount, creativeAccountSearch, creativeAccountState } from "@/features/creative-console/creative-console-account";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";

export function CreativeAccountPicker({ value, onChange }: { value: AccountDTO | null; onChange: (value: AccountDTO | null) => void }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [page, setPage] = useState(1);
  const listId = useId();
  const listRef = useRef<HTMLDivElement>(null);
  const lookup = creativeAccountSearch(search);
  const query = useDebouncedValue(lookup, 250);
  const searching = lookup !== query;
  const providers = useProviders().data;
  const accounts = useQuery({
    queryKey: ["creative-console", "accounts", query, page],
    queryFn: () => listAccounts({ page, pageSize: 20, search: query, sortBy: "createdAt", sortOrder: "desc" }),
    enabled: open,
    retry: false,
  });

  function select(account: AccountDTO | null) {
    onChange(account);
    setOpen(false);
  }

  function navigate(event: KeyboardEvent<HTMLElement>) {
    if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
    const options = Array.from(listRef.current?.querySelectorAll<HTMLButtonElement>('button[role="option"]:not(:disabled)') ?? []);
    if (!options.length) return;
    event.preventDefault();
    const current = options.indexOf(document.activeElement as HTMLButtonElement);
    const next = event.key === "Home" ? 0 : event.key === "End" ? options.length - 1 : event.key === "ArrowUp" ? (current <= 0 ? options.length - 1 : current - 1) : (current + 1) % options.length;
    options[next].focus();
  }

  return <Popover open={open} onOpenChange={setOpen}>
    <PopoverTrigger asChild>
      <Button type="button" variant="secondary" role="combobox" aria-label={t("creativeConsole.accountSelect")} aria-expanded={open} aria-haspopup="listbox" aria-controls={listId} className={cn("min-w-0 flex-1 justify-between gap-2 bg-secondary/55 font-normal lg:w-60 lg:flex-none", value && "ring-1 ring-amber-500/40")}>
        <UserRound className="size-4 shrink-0" /><span className="min-w-0 flex-1 truncate text-left">{value ? accountDisplayName(value) : t("creativeConsole.accountAuto")}</span><ChevronDown className="size-4 shrink-0 opacity-50" />
      </Button>
    </PopoverTrigger>
    <PopoverContent align="end" className="w-[min(28rem,calc(100vw-2rem))] space-y-2 p-2">
      <div className="relative"><Search className="pointer-events-none absolute left-3 top-2.5 size-4 text-muted-foreground" /><Input aria-label={t("creativeConsole.accountSearch")} aria-controls={listId} placeholder={t("creativeConsole.accountSearch")} value={search} onKeyDown={navigate} onChange={(event) => { setSearch(event.target.value); setPage(1); }} className="pl-9" /></div>
      <div ref={listRef} id={listId} role="listbox" aria-label={t("creativeConsole.accountSelect")} onKeyDown={navigate} className="max-h-80 overflow-y-auto">
        <button type="button" role="option" aria-selected={!value} onClick={() => select(null)} className="flex w-full items-center gap-2 rounded-md px-3 py-2 text-sm hover:bg-secondary focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"><Check className={cn("size-4 shrink-0", value && "invisible")} />{t("creativeConsole.accountAuto")}</button>
        {searching || accounts.isPending ? <div className="flex justify-center py-8"><Spinner /></div> : accounts.isError ? <div className="space-y-2 px-3 py-5 text-center text-sm"><p className="text-destructive">{accounts.error.message}</p><Button type="button" size="sm" variant="secondary" onClick={() => void accounts.refetch()}>{t("common.retry")}</Button></div> : accounts.data.items.length === 0 ? <p className="px-3 py-6 text-center text-sm text-muted-foreground">{t("creativeConsole.accountEmpty")}</p> : accounts.data.items.map((account) => {
          const state = creativeAccountState(account);
          const selectable = canSelectCreativeAccount(account);
          return <button type="button" role="option" aria-selected={value?.id === account.id} key={account.id} disabled={!selectable} onClick={() => select(account)} className="flex w-full items-center gap-2 rounded-md px-3 py-2.5 text-left hover:bg-secondary focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-45">
            <Check className={cn("size-4 shrink-0", value?.id !== account.id && "invisible")} />
            <span className="min-w-0 flex-1"><span className="block truncate text-sm font-medium">{accountDisplayName(account)}</span><span className="block truncate text-xs text-muted-foreground">{providerLabel(account.provider, providers)} · #{account.id}</span></span>
            <Badge variant="secondary" className={cn("shrink-0", (state === "limited" || state === "cooling") && "bg-amber-500/10 text-amber-700 dark:text-amber-400")}>{t(`creativeConsole.accountState.${state}`)}</Badge>
          </button>;
        })}
      </div>
      {!searching && accounts.data && accounts.data.total > 20 ? <div className="flex items-center justify-between border-t px-2 pt-2 text-xs text-muted-foreground"><span>{page} / {Math.ceil(accounts.data.total / 20)}</span><div className="flex gap-1"><Button type="button" variant="ghost" size="icon" className="size-7" aria-label={t("common.previousPage")} disabled={page === 1 || accounts.isFetching} onClick={() => setPage(page - 1)}><ChevronLeft /></Button><Button type="button" variant="ghost" size="icon" className="size-7" aria-label={t("common.nextPage")} disabled={page * 20 >= accounts.data.total || accounts.isFetching} onClick={() => setPage(page + 1)}><ChevronRight /></Button></div></div> : null}
    </PopoverContent>
  </Popover>;
}
