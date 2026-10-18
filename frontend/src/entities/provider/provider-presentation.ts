// Structural subsets of ProviderDTO keep this module dependency-free (and node-testable).
type ProviderRef = { id: string };
type ProviderIdentity = ProviderRef & { name: string; shortName: string };

// Built-in Grok identities double as the fallback catalog, so Grok labels and order never
// flash raw ids while the catalog loads or after it fails.
const grokProviders: readonly ProviderIdentity[] = [
  { id: "grok_build", name: "Grok Build", shortName: "Build" },
  { id: "grok_web", name: "Grok Web", shortName: "Web" },
  { id: "grok_console", name: "Grok Console", shortName: "Console" },
];

// Entries of the --quota-product-N palette in index.css. Grok colours are fixed; other catalog
// providers take the spare entries in catalog order; ids outside the catalog stay neutral.
const grokColors = new Map([["grok_build", 1], ["grok_web", 2], ["grok_console", 4]]);
const sparePalette = [3, 5, 6];
const neutralPalette = 0;

/**
 * isGrokProvider is true for the built-in xAI providers. Use it only for genuinely Grok-only
 * behaviour (the xAI request dialect: aspect_ratio/resolution images, web_search/x_search tools,
 * Console video edits); anything the catalog describes must be read from the catalog.
 */
export function isGrokProvider(id: string): boolean {
  return grokProviders.some((provider) => provider.id === id);
}

function findProvider(id: string, providers?: readonly ProviderIdentity[]): ProviderIdentity | undefined {
  return providers?.find((provider) => provider.id === id) ?? grokProviders.find((provider) => provider.id === id);
}

export function providerLabel(id: string, providers?: readonly ProviderIdentity[]): string {
  return findProvider(id, providers)?.name || id;
}

export function providerShortLabel(id: string, providers?: readonly ProviderIdentity[]): string {
  return findProvider(id, providers)?.shortName || id;
}

// providerColor returns a CSS colour for inline styles (dots, stripes, icons).
export function providerColor(id: string, providers?: readonly ProviderRef[]): string {
  const grok = grokColors.get(id);
  if (grok !== undefined) return paletteColor(grok);
  const index = (providers ?? []).filter((provider) => !grokColors.has(provider.id)).findIndex((provider) => provider.id === id);
  return paletteColor(index < 0 ? neutralPalette : sparePalette[index % sparePalette.length]);
}

function paletteColor(index: number): string {
  return `var(--quota-product-${index})`;
}

function orderedCatalog(providers?: readonly ProviderRef[]): readonly ProviderRef[] {
  return providers?.length ? providers : grokProviders;
}

// providerRank sorts ids by catalog order; ids outside the catalog sort last.
export function providerRank(id: string, providers?: readonly ProviderRef[]): number {
  const catalog = orderedCatalog(providers);
  const index = catalog.findIndex((provider) => provider.id === id);
  return index < 0 ? catalog.length : index;
}

// orderedProviderIds lists catalog providers in order, then any other ids (e.g. from usage data) in first-seen order.
export function orderedProviderIds(providers?: readonly ProviderRef[], extraIds: Iterable<string> = []): string[] {
  return [...new Set([...orderedCatalog(providers).map((provider) => provider.id), ...extraIds])];
}

// Egress scopes owned by non-Grok providers; Grok scopes keep their fixed, translated entries in egress settings.
export function extensionEgressScopes(providers?: readonly (ProviderRef & { egressScopes: string[] })[]): string[] {
  return [...new Set((providers ?? []).filter((provider) => !isGrokProvider(provider.id)).flatMap((provider) => provider.egressScopes))];
}
