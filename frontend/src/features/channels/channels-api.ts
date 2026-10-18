import { z } from "zod";
import { apiRequest } from "@/shared/api/client";

const settingsSchema = z.object({
  version: z.number(),
  enabled: z.boolean(),
  image_conversation_model: z.string(),
  output_token_limit: z.number(),
  vision_tokens: z.number(),
  account_concurrency: z.number(),
  image_workers: z.number(),
  cloudflare_clearance: z.boolean().default(false),
  default_image_model: z.string().default(""),
});

const descriptorSchema = z.object({
  id: z.string(),
  name: z.string(),
  version: z.number(),
  capabilities: z.object({
    chat: z.boolean(),
    responses: z.boolean(),
    image_generation: z.boolean(),
    image_edit: z.boolean(),
    mask: z.boolean(),
    transparent: z.boolean(),
    oauth: z.boolean(),
    refresh: z.boolean(),
  }),
  auth_methods: z.array(z.string()),
  clearance_url: z.string().optional(),
});

const accountSchema = z.object({
  id: z.number(),
  provider: z.string(),
  name: z.string(),
  enabled: z.boolean(),
  max_concurrent: z.number(),
  identity: z.object({
    user_id: z.string(),
    workspace_id: z.string().optional(),
    email: z.string().optional(),
    plan: z.string().optional(),
  }),
  status: z.string(),
  failure: z.string().optional(),
  credential_version: z.number(),
  refreshable: z.boolean(),
  expires_at: z.string().optional(),
  synced_at: z.string().optional(),
  egress_node_id: z.number().optional(),
  models: z.array(z.string()).nullable(),
  quotas: z.array(z.object({ feature: z.string(), remaining: z.number().nullable(), reset_at: z.string().optional() })).nullable(),
});

const priceSchema = z.object({
  id: z.number(),
  model: z.string(),
  operation: z.string(),
  version: z.number(),
  input_per_million_ticks: z.number(),
  output_per_million_ticks: z.number(),
  image_ticks: z.number(),
  quality: z.string().optional(),
  size: z.string().optional(),
});

const modelSchema = z.object({
  ID: z.number(),
  PublicID: z.string(),
  UpstreamModel: z.string(),
  Capability: z.string(),
  Enabled: z.boolean(),
});

export const jobSchema = z.object({
  id: z.string(),
  status: z.string(),
  created_at: z.string(),
  items: z.array(z.object({
    id: z.string(),
    index: z.number(),
    status: z.string(),
    stage: z.string(),
    asset_ids: z.array(z.string()).nullable(),
    width: z.number().optional(),
    height: z.number().optional(),
    mime_type: z.string().optional(),
    error: z.string().optional(),
  })),
});

export type ChannelSettings = z.infer<typeof settingsSchema>;
export type ChannelAccount = z.infer<typeof accountSchema>;
export type ChannelView = { descriptor: z.infer<typeof descriptorSchema>; settings: ChannelSettings };
export type ChannelPrice = z.infer<typeof priceSchema>;
export type ChannelModel = z.infer<typeof modelSchema>;
export type ImageJob = z.infer<typeof jobSchema>;

function request<T>(path: string, schema: z.ZodType<T>, method = "GET", body?: object): Promise<T> {
  return apiRequest(`/api/admin/v1/channels${path}`, { method, body }, (value) => schema.parse(value));
}

const base = (id: string) => `/${encodeURIComponent(id)}`;

export const channelsApi = {
  list: () => request("", z.array(z.object({ descriptor: descriptorSchema, settings: settingsSchema }))),
  settings: (id: string, v: ChannelSettings) => request(base(id), settingsSchema, "PATCH", v),
  accounts: (id: string, offset: number) =>
    request(`${base(id)}/accounts?offset=${offset}&limit=50`, z.object({ items: z.array(accountSchema), total: z.number() })),
  import: (id: string, data: string, replace: boolean) =>
    request(
      `${base(id)}/accounts/import`,
      z.array(z.object({ index: z.number(), status: z.string(), account_id: z.number().optional(), reason: z.string().optional() })),
      "POST",
      { data, replace },
    ),
  export: (id: string, ids: number[]) =>
    request(`${base(id)}/accounts/export`, z.object({ version: z.number(), accounts: z.array(z.unknown()) }), "POST", { ids }),
  update: (id: string, account: number, patch: object) => request(`${base(id)}/accounts/${account}`, accountSchema, "PATCH", patch),
  actions: (id: string, ids: number[], action: string) =>
    request(
      `${base(id)}/accounts/actions`,
      z.array(z.object({ id: z.number(), success: z.boolean(), reason: z.string().optional() })),
      "POST",
      { ids, action },
    ),
  startOAuth: (id: string) =>
    request(`${base(id)}/oauth/sessions`, z.object({ id: z.string(), authorize_url: z.string(), expires_at: z.string() }), "POST"),
  completeOAuth: (id: string, session: string, url: string) =>
    request(`${base(id)}/oauth/sessions/${encodeURIComponent(session)}/complete`, accountSchema, "POST", { callback_url: url }),
  prices: (id: string) => request(`${base(id)}/prices`, z.array(priceSchema)),
  savePrice: (id: string, v: object) => request(`${base(id)}/prices`, priceSchema, "POST", v),
  grants: (id: string) => request(`${base(id)}/client-key-grants`, z.array(z.number())),
  grant: (id: string, key: number, enabled: boolean) =>
    request(`${base(id)}/client-key-grants/${key}`, z.object({ enabled: z.boolean() }), "PUT", { enabled }),
  retryJob: (id: string, job: string, idempotencyKey: string) =>
    request(`${base(id)}/image-jobs/${encodeURIComponent(job)}/retry`, jobSchema, "POST", { idempotency_key: idempotencyKey }),
  clientModels: (id: string, key: string) => request(`${base(id)}/client-models/${encodeURIComponent(key)}`, z.array(modelSchema)),
  models: (id: string) => request(`${base(id)}/models`, z.array(modelSchema)),
  createModel: (id: string, v: object) => request(`${base(id)}/models`, modelSchema, "POST", v),
  setModelEnabled: (id: string, model: number, enabled: boolean) => request(`${base(id)}/models/${model}`, modelSchema, "PATCH", { enabled }),
  deleteModel: (id: string, model: number) => request(`${base(id)}/models/${model}`, z.object({ deleted: z.boolean() }), "DELETE"),
  jobs: (id: string, offset = 0) =>
    request(`${base(id)}/image-jobs?offset=${offset}&limit=50`, z.object({ items: z.array(jobSchema), total: z.number() })),
};
