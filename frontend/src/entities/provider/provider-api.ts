import { useQuery } from "@tanstack/react-query";

import { providerListSchema, type ProviderDTO } from "@/entities/provider/provider-schema";
import { apiRequest } from "@/shared/api/client";

export type { ProviderDTO } from "@/entities/provider/provider-schema";

export async function listProviders(): Promise<ProviderDTO[]> {
  const providers = await apiRequest("/api/admin/v1/providers", {}, (value) => providerListSchema.parse(value));
  return providers.sort((left, right) => left.order - right.order);
}

// The catalog only changes when the server is redeployed, so every page shares one long-lived copy.
export function useProviders() {
  return useQuery({ queryKey: ["providers"], queryFn: listProviders, staleTime: 10 * 60_000 });
}
