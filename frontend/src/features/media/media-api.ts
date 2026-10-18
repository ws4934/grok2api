import { galleryQuery, mediaAssetSchema, mediaJobSchema, mediaJobsQuery, mediaJobStatsSchema, pageSchema, retriedMediaJobSchema, type GalleryFilter, type MediaAssetDTO, type MediaJobDTO, type MediaJobsFilter, type MediaJobStatsDTO, type RetriedMediaJobDTO } from "@/features/media/media-model";
import { apiRequest, type PaginatedDTO } from "@/shared/api/client";
import {
  createObjectDecoder,
  createValidatedDecoder,
  decodeCountResult,
  hasShape,
  isNumber,
  isString,
} from "@/shared/api/decoder";

export type ImageStatsDTO = { totalImages: number; totalBytes: number };

const decodeImageStats = createObjectDecoder<ImageStatsDTO>("image stats", {
  totalImages: isNumber,
  totalBytes: isNumber,
});
const assetPage = pageSchema(mediaAssetSchema);
const jobPage = pageSchema(mediaJobSchema);

export function listMediaAssets(filter: GalleryFilter): Promise<PaginatedDTO<MediaAssetDTO>> {
  return apiRequest(`/api/admin/v1/media/images?${galleryQuery(filter)}`, {}, (value) => assetPage.parse(value));
}

export function getMediaAsset(id: string): Promise<MediaAssetDTO> {
  return apiRequest(`/api/admin/v1/media/images/${encodeURIComponent(id)}`, {}, (value) => mediaAssetSchema.parse(value));
}

export function getImageStats(): Promise<ImageStatsDTO> {
  return apiRequest("/api/admin/v1/media/images/stats", {}, decodeImageStats);
}

// Deletes images or videos; 409 mediaAssetInUse when one belongs to an active job.
export function deleteMediaAssets(ids: string[]): Promise<{ deleted: number }> {
  return apiRequest("/api/admin/v1/media/images", { method: "DELETE", body: { ids } }, decodeCountResult<{ deleted: number }>("deleted"));
}

export function listMediaJobs(filter: MediaJobsFilter): Promise<PaginatedDTO<MediaJobDTO>> {
  return apiRequest(`/api/admin/v1/media/jobs?${mediaJobsQuery(filter)}`, {}, (value) => jobPage.parse(value));
}

export function getMediaJobStats(kind: string): Promise<MediaJobStatsDTO> {
  const query = kind ? `?${new URLSearchParams({ kind })}` : "";
  return apiRequest(`/api/admin/v1/media/jobs/stats${query}`, {}, (value) => mediaJobStatsSchema.parse(value));
}

// Only ended jobs; image results stay in the gallery.
export function deleteMediaJobs(ids: string[]): Promise<{ deleted: number }> {
  return apiRequest("/api/admin/v1/media/jobs", { method: "DELETE", body: { ids } }, decodeCountResult<{ deleted: number }>("deleted"));
}

export function cancelMediaJob(id: string): Promise<MediaJobDTO> {
  return apiRequest(`/api/admin/v1/media/jobs/${encodeURIComponent(id)}/cancel`, { method: "POST" }, (value) => mediaJobSchema.parse(value));
}

// The server's default idempotency key (retry:<id>) makes repeated clicks resolve to one new job;
// a resumable failed job comes back itself with resumed: true.
export function retryMediaJob(id: string): Promise<RetriedMediaJobDTO> {
  return apiRequest(`/api/admin/v1/media/jobs/${encodeURIComponent(id)}/retry`, { method: "POST" }, (value) => retriedMediaJobSchema.parse(value));
}

// 临时输入不会进入图库，也不会生成公开 URL；任务只持久化短 file_id。
export type MediaInputDTO = {
  fileId: string;
  kind: string;
  mimeType: string;
  sizeBytes: number;
  expiresAt: string;
};

const decodeMediaInput = createValidatedDecoder<MediaInputDTO>("media input", hasShape({
  fileId: isString,
  kind: isString,
  mimeType: isString,
  sizeBytes: isNumber,
  expiresAt: isString,
}));

export function importVideoInputFromURL(url: string): Promise<MediaInputDTO> {
  return apiRequest("/api/admin/v1/media/inputs/import", { method: "POST", body: { url } }, decodeMediaInput);
}

// 以 multipart/form-data 上传本地图片或视频到有 TTL 的临时输入区。
// 注意：不要手动设置 Content-Type，浏览器会自动带上 multipart 边界。
export function uploadMediaInput(file: File): Promise<MediaInputDTO> {
  const body = new FormData();
  body.append("file", file, file.name);
  return apiRequest("/api/admin/v1/media/inputs/upload", { method: "POST", body }, decodeMediaInput);
}
