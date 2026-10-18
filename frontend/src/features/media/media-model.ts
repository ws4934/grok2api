import { z } from "zod";

// Dependency-free (zod only) so node --test can load it.

export const mediaJobStatuses = ["queued", "in_progress", "completed", "failed", "canceled", "uncertain"] as const;
export type MediaJobStatus = (typeof mediaJobStatuses)[number];
export type MediaKind = "image" | "video";
export const assetOperations = ["image", "image_edit", "video"] as const;

// Fields other than identity and status default so an older or partial payload still decodes.
export const mediaJobSchema = z.object({
  id: z.string(),
  kind: z.enum(["image", "video"]),
  provider: z.string().default(""),
  model: z.string().default(""),
  operation: z.string().default(""),
  prompt: z.string().default(""),
  status: z.enum(mediaJobStatuses),
  progress: z.number().default(0),
  seconds: z.number().default(0),
  size: z.string().default(""),
  quality: z.string().default(""),
  itemCount: z.number().default(0),
  assetIds: z.array(z.string()).default([]),
  accountName: z.string().default(""),
  clientKeyName: z.string().default(""),
  createdAt: z.string(),
  completedAt: z.string().nullable().default(null),
  errorCode: z.string().default(""),
  errorMessage: z.string().default(""),
  cancellable: z.boolean().default(false),
  retryable: z.boolean().default(false),
});
export type MediaJobDTO = z.infer<typeof mediaJobSchema>;

export const mediaJobStatsSchema = z.object({
  totalJobs: z.number().default(0),
  queued: z.number().default(0),
  inProgress: z.number().default(0),
  completed: z.number().default(0),
  failed: z.number().default(0),
  canceled: z.number().default(0),
  uncertain: z.number().default(0),
});
export type MediaJobStatsDTO = z.infer<typeof mediaJobStatsSchema>;

// Origin fields are empty for uploads and assets saved before provenance existed.
export const mediaAssetSchema = z.object({
  id: z.string(),
  kind: z.string(),
  mimeType: z.string(),
  sizeBytes: z.number(),
  sha256: z.string().default(""),
  createdAt: z.string(),
  provider: z.string().default(""),
  model: z.string().default(""),
  upstreamModel: z.string().default(""),
  clientKeyId: z.number().default(0),
  clientKeyName: z.string().default(""),
  accountId: z.number().default(0),
  accountName: z.string().default(""),
  operation: z.string().default(""),
  jobId: z.string().default(""),
  requestId: z.string().default(""),
  prompt: z.string().default(""),
});
export type MediaAssetDTO = z.infer<typeof mediaAssetSchema>;

export function pageSchema<T extends z.ZodType>(item: T) {
  return z.object({ items: z.array(item), page: z.number(), pageSize: z.number(), total: z.number() });
}

export type MediaJobsFilter = {
  page: number;
  pageSize: number;
  kind?: MediaKind | "";
  status?: MediaJobStatus | "";
  provider?: string;
  search?: string;
  sortBy?: string;
  sortOrder?: "asc" | "desc";
};

export function mediaJobsQuery(filter: MediaJobsFilter): string {
  const query = new URLSearchParams({ page: String(filter.page), pageSize: String(filter.pageSize) });
  if (filter.kind) query.set("kind", filter.kind);
  if (filter.status) query.set("status", filter.status);
  if (filter.provider) query.set("provider", filter.provider);
  if (filter.search) query.set("search", filter.search);
  if (filter.sortBy && filter.sortOrder) {
    query.set("sortBy", filter.sortBy);
    query.set("sortOrder", filter.sortOrder);
  }
  return query.toString();
}

export type GalleryFilter = {
  page: number;
  pageSize: number;
  kind: MediaKind;
  search?: string;
  provider?: string;
  model?: string;
  operation?: string;
  clientKeyId?: string;
  accountId?: string;
  jobId?: string;
  // Local calendar days (YYYY-MM-DD) from <input type="date">; both ends inclusive.
  from?: string;
  to?: string;
};

export function galleryQuery(filter: GalleryFilter): string {
  const query = new URLSearchParams({ page: String(filter.page), pageSize: String(filter.pageSize), kind: filter.kind });
  const optional: Array<[string, string | undefined]> = [
    ["search", filter.search], ["provider", filter.provider], ["model", filter.model], ["operation", filter.operation],
    ["clientKeyId", filter.clientKeyId], ["accountId", filter.accountId], ["jobId", filter.jobId],
    // The server takes RFC3339 with an inclusive start and an exclusive end, so the end day becomes the next midnight.
    ["from", localDayStart(filter.from, 0)], ["to", localDayStart(filter.to, 1)],
  ];
  for (const [key, value] of optional) if (value) query.set(key, value);
  return query.toString();
}

// localDayStart returns the RFC3339 instant of local midnight `offsetDays` after `day`, or undefined for a non-date.
export function localDayStart(day: string | undefined, offsetDays: number): string | undefined {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day ?? "");
  if (!match) return undefined;
  const date = new Date(Number(match[1]), Number(match[2]) - 1, Number(match[3]) + offsetDays);
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString();
}

export function isTerminalJob(status: MediaJobStatus): boolean {
  return status !== "queued" && status !== "in_progress";
}

// The server decides cancel/retry (it knows whether work already reached the upstream); only ended jobs can be deleted.
export function mediaJobActions(job: Pick<MediaJobDTO, "status" | "cancellable" | "retryable" | "assetIds">) {
  const terminal = isTerminalJob(job.status);
  return {
    cancel: job.cancellable && !terminal,
    retry: job.retryable && job.status === "uncertain",
    delete: terminal,
    results: job.assetIds.length > 0,
  };
}

export function mediaProviders<T extends { media: { imageGeneration: boolean; imageEdit: boolean; videoGeneration: boolean } }>(providers: readonly T[] = []): T[] {
  return providers.filter(({ media }) => media.imageGeneration || media.imageEdit || media.videoGeneration);
}

// Admin pages share the API origin; relative paths avoid depending on the configured public base URL.
export function mediaAssetPath(kind: string, id: string): string {
  return `/v1/media/${kind === "video" ? "videos" : "images"}/${encodeURIComponent(id)}`;
}
