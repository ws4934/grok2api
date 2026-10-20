import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import type { PaginatedDTO } from "@/shared/api/client";
import type { DataTableFilterOption, DataTableFilterOptionGroup } from "@/shared/components/data-table-filters";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";

// Choices stay limited to the first page of the server search, so a large pool never piles up in the browser.
const LOOKUP_PAGE_SIZE = 50;

// useLookupFilter loads key/account choices only while their submenu is open, re-querying as the admin types.
// Its own value is "any"; callers map that to an empty filter.
export function useLookupFilter<T>(
  name: string,
  label: string,
  placeholder: string,
  load: (input: { page: number; pageSize: number; search: string }) => Promise<PaginatedDTO<T>>,
  toOption: (item: T) => DataTableFilterOptionGroup["options"][number],
): DataTableFilterOption {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  const debouncedSearch = useDebouncedValue(search).trim();
  const query = useQuery({
    queryKey: [name, "lookup-filter", debouncedSearch],
    // A bare number searches by ID.
    queryFn: () => load({ page: 1, pageSize: LOOKUP_PAGE_SIZE, search: /^\d+$/.test(debouncedSearch) ? `#${debouncedSearch}` : debouncedSearch }),
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
