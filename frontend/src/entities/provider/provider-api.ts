import { useQuery } from "@tanstack/react-query";
import { z } from "zod";

import { apiRequest } from "@/shared/api/client";

const providerSchema = z.object({
  id: z.string().min(1),
  name: z.string(),
  shortName: z.string(),
  order: z.number(),
  capabilities: z.array(z.enum(["responses", "chat", "image", "image_edit", "video", "tts", "stt", "realtime"])),
  conversation: z.object({ responses: z.boolean(), chatCompletions: z.boolean(), messages: z.boolean() }),
  media: z.object({ imageGeneration: z.boolean(), imageEdit: z.boolean(), videoGeneration: z.boolean(), tts: z.boolean(), stt: z.boolean(), realtime: z.boolean() }),
  quota: z.enum(["billing", "remote_window", "local_window"]),
  usage: z.enum(["upstream", "estimated"]),
  // false: the provider ignores client-key tier restrictions.
  tierScoped: z.boolean(),
  egressScopes: z.array(z.string()),
  credential: z.object({ authType: z.enum(["oauth", "sso"]), import: z.boolean(), refresh: z.boolean(), deviceOAuth: z.boolean(), redirectOAuth: z.boolean() }),
});
const providerListSchema = z.array(providerSchema);

export type ProviderDTO = z.infer<typeof providerSchema>;

export async function listProviders(): Promise<ProviderDTO[]> {
  const providers = await apiRequest("/api/admin/v1/providers", {}, (value) => providerListSchema.parse(value));
  return providers.sort((left, right) => left.order - right.order);
}

// The catalog only changes when the server is redeployed, so every page shares one long-lived copy.
export function useProviders() {
  return useQuery({ queryKey: ["providers"], queryFn: listProviders, staleTime: 10 * 60_000 });
}
