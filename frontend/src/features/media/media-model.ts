import { z } from "zod";

// Dependency-free (zod only) so node --test can load it.

export const mediaJobStatuses = ["queued", "in_progress", "completed", "failed", "canceled", "uncertain"] as const;
export type MediaJobStatus = (typeof mediaJobStatuses)[number];
export type MediaKind = "image" | "video";
export const assetOperations = ["image", "image_edit", "video"] as const;

export const mediaJobSchema = z.object({
  id: z.string(),
  kind: z.enum(["image", "video"]),
  provider: z.string(),
  model: z.string(),
  operation: z.string(),
  prompt: z.string(),
  status: z.enum(mediaJobStatuses),
  progress: z.number(),
  seconds: z.number(),
  size: z.string(),
  quality: z.string(),
  assetIds: z.array(z.string()),
  accountName: z.string(),
  clientKeyName: z.string(),
  createdAt: z.string(),
  completedAt: z.string().nullable(),
  errorCode: z.string(),
  errorMessage: z.string(),
  cancellable: z.boolean(),
  retryable: z.boolean(),
  usagePending: z.boolean(),
});
export type MediaJobDTO = z.infer<typeof mediaJobSchema>;

// resumed: the same job fetches its result again; otherwise the retry is a new job.
export const retriedMediaJobSchema = mediaJobSchema.extend({ resumed: z.boolean() });
export type RetriedMediaJobDTO = z.infer<typeof retriedMediaJobSchema>;

export const mediaJobStatsSchema = z.object({
  totalJobs: z.number(),
  queued: z.number(),
  inProgress: z.number(),
  completed: z.number(),
  failed: z.number(),
  canceled: z.number(),
  uncertain: z.number(),
});
export type MediaJobStatsDTO = z.infer<typeof mediaJobStatsSchema>;

// Origin fields are empty for uploads.
export const mediaAssetSchema = z.object({
  id: z.string(),
  kind: z.string(),
  mimeType: z.string(),
  sizeBytes: z.number(),
  sha256: z.string(),
  createdAt: z.string(),
  provider: z.string(),
  model: z.string(),
  upstreamModel: z.string(),
  clientKeyId: z.number(),
  clientKeyName: z.string(),
  accountId: z.number(),
  accountName: z.string(),
  operation: z.string(),
  jobId: z.string(),
  requestId: z.string(),
  prompt: z.string(),
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

// The server decides cancel/retry; deleting a job must also preserve its pending settlement.
// A retryable uncertain job is resubmitted as a new job; a retryable failed job fetches the result the upstream already drew.
export function mediaJobActions(job: Pick<MediaJobDTO, "status" | "cancellable" | "retryable" | "assetIds" | "usagePending">) {
  const terminal = isTerminalJob(job.status);
  return {
    cancel: job.cancellable && !terminal,
    retry: job.retryable && job.status === "uncertain",
    resume: job.retryable && job.status === "failed",
    delete: terminal && !job.usagePending,
    results: job.assetIds.length > 0,
  };
}

// Keep terminal jobs current until their durable settlement finishes.
export function mediaJobsRefetchInterval(jobs: readonly Pick<MediaJobDTO, "status" | "usagePending">[] = []): number | false {
  return jobs.some((job) => !isTerminalJob(job.status) || job.usagePending) ? 5_000 : false;
}

export function mediaProviders<T extends { media: { imageGeneration: boolean; imageEdit: boolean; videoGeneration: boolean } }>(providers: readonly T[] = []): T[] {
  return providers.filter(({ media }) => media.imageGeneration || media.imageEdit || media.videoGeneration);
}

// Admin pages share the API origin; relative paths avoid depending on the configured public base URL.
export function mediaAssetPath(kind: string, id: string): string {
  return `/v1/media/${kind === "video" ? "videos" : "images"}/${encodeURIComponent(id)}`;
}
