import { z } from "zod";

const providerSchema = z.object({
  id: z.string().min(1),
  name: z.string(),
  shortName: z.string(),
  order: z.number(),
  capabilities: z.array(z.enum(["responses", "chat", "image", "image_edit", "video", "tts", "stt", "realtime"])),
  conversation: z.object({ responses: z.boolean(), chatCompletions: z.boolean(), messages: z.boolean() }),
  // asyncImages: images run as durable jobs (Responses image_generation with background).
  media: z.object({ imageGeneration: z.boolean(), imageEdit: z.boolean(), videoGeneration: z.boolean(), tts: z.boolean(), stt: z.boolean(), realtime: z.boolean(), asyncImages: z.boolean() }),
  quota: z.enum(["billing", "remote_window", "local_window"]),
  usage: z.enum(["upstream", "estimated"]),
  // false: the provider ignores client-key tier restrictions.
  tierScoped: z.boolean(),
  egressScopes: z.array(z.string()),
  credential: z.object({ authType: z.string(), import: z.boolean(), refresh: z.boolean(), deviceOAuth: z.boolean(), redirectOAuth: z.boolean() }),
  quotaModes: z.array(z.string()),
  // Effective per-account concurrency for new accounts.
  defaultMaxConcurrent: z.number(),
});

export const providerListSchema = z.array(providerSchema);

export type ProviderDTO = z.infer<typeof providerSchema>;
