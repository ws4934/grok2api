// listAllPaginatedItems reads every page of a paginated admin list (at most 50 pages of 100).
export async function listAllPaginatedItems<T>(loadPage: (page: number, pageSize: number) => Promise<{ items: T[]; total: number }>): Promise<T[]> {
  const items: T[] = [];
  for (let page = 1; page <= 50; page += 1) {
    const result = await loadPage(page, 100);
    items.push(...result.items);
    if (result.items.length === 0 || items.length >= result.total) break;
  }
  return items;
}
