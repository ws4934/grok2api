import type { ApiErrorDetail, ImageParams, ImageResult } from "./creative-console-api";

// Type-only imports keep this module loadable by node --test.

export type VideoAction = "generate" | "edit" | "extend";

export type ImageHistoryRecord = {
  id: string;
  createdAt: number;
  prompt: string;
  model: string;
  basicProtocol: boolean;
  params: ImageParams;
  imageURLs?: string[];
  status: "generating" | "completed" | "failed" | "interrupted";
  images: ImageResult[];
  error?: ApiErrorDetail;
  // jobIds are durable image jobs (async providers); their results can be fetched after a reload.
  jobIds?: string[];
};

export type VideoHistoryRecord = {
  id: string;
  createdAt: number;
  action: VideoAction;
  prompt: string;
  model: string;
  params: {
    duration?: number; aspectRatio?: string; resolution?: string;
    lastFrame?: { url?: string; name?: string };
    keyframes?: Array<{ url?: string; name?: string; timestampSeconds: number }>;
    generateAudio?: boolean;
  };
  status: "submitting" | "generating" | "completed" | "failed" | "interrupted";
  requestId?: string;
  video?: { url: string; duration?: number };
  error?: ApiErrorDetail;
};

type KeyValueStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

const mediaHistoryMaxEntries = 100;
const mediaHistoryMaxBytes = 1024 * 1024;
const imageStatuses = ["generating", "completed", "failed", "interrupted"] as const;
const videoStatuses = ["submitting", "generating", "completed", "failed", "interrupted"] as const;
const videoActions = ["generate", "edit", "extend"] as const;

// Image N of a record is submitted with this X-Request-ID, which its audit records carry; jobIds[N] is that image's job.
export function imageJobRequestId(recordId: string, index: number): string {
  return index === 0 ? recordId : `${recordId}-${index}`;
}

export function historyStorageKey(kind: "chat" | "image" | "video", scope: string): string {
  return `grok2api:creative-console:${kind}-history:${encodeURIComponent(scope)}`;
}

export function loadStoredList<T>(key: string, parse: (value: unknown) => T[], storage = browserStorage()): T[] {
  if (!storage) return [];
  try {
    const parsed: unknown = JSON.parse(storage.getItem(key) ?? "[]");
    return Array.isArray(parsed) ? parsed.flatMap(parse) : [];
  } catch {
    return [];
  }
}

// Items are ordered newest first; trailing (oldest) items are dropped until the list fits the caps and the quota.
export function persistStoredList<T>(key: string, items: T[], maxEntries: number, maxBytes: number, storage = browserStorage()): T[] {
  const retained = items.slice(0, maxEntries);
  if (!storage) return retained;
  while (retained.length > 0) {
    try {
      const serialized = JSON.stringify(retained);
      if (serialized.length * 2 > maxBytes) {
        retained.pop();
        continue;
      }
      storage.setItem(key, serialized);
      return retained;
    } catch {
      retained.pop();
    }
  }
  try {
    storage.removeItem(key);
  } catch {
    // Storage may be unavailable; the in-memory history remains usable.
  }
  return retained;
}

export function loadImageHistory(scope: string, storage = browserStorage()): ImageHistoryRecord[] {
  return newestFirst(loadStoredList(historyStorageKey("image", scope), parseImageRecord, storage));
}

// Only metadata and URLs are stored: inline data:/blob: images would exhaust the quota.
export function persistImageHistory(scope: string, records: ImageHistoryRecord[], storage = browserStorage()): void {
  const storable = records.map((record) => ({ ...record, images: record.images.filter((image) => !isInlineURL(image.url)) }));
  persistStoredList(historyStorageKey("image", scope), storable, mediaHistoryMaxEntries, mediaHistoryMaxBytes, storage);
}

export function loadVideoHistory(scope: string, storage = browserStorage()): VideoHistoryRecord[] {
  return newestFirst(loadStoredList(historyStorageKey("video", scope), parseVideoRecord, storage));
}

export function persistVideoHistory(scope: string, records: VideoHistoryRecord[], storage = browserStorage()): void {
  const storable = records.map((record) => ({ ...record, params: parseVideoParams(record.params) }));
  persistStoredList(historyStorageKey("video", scope), storable, mediaHistoryMaxEntries, mediaHistoryMaxBytes, storage);
}

// A sync request still "generating" when the page loads can no longer deliver its result here;
// records with durable job IDs keep polling.
export function parseImageRecord(value: unknown): ImageHistoryRecord[] {
  if (!isLocalRecord(value) || typeof value.id !== "string" || typeof value.prompt !== "string" || typeof value.model !== "string") return [];
  const createdAt = finiteTimestamp(value.createdAt);
  const params: Record<string, unknown> = isLocalRecord(value.params) ? value.params : {};
  const count = params.count;
  const jobIds = Array.isArray(value.jobIds) ? value.jobIds.filter((id): id is string => typeof id === "string" && id.length > 0) : [];
  if (createdAt === null || !oneOf(value.status, imageStatuses) || typeof count !== "number" || !Number.isInteger(count) || count < 1) return [];
  return [{
    id: value.id,
    createdAt,
    prompt: value.prompt,
    model: value.model,
    basicProtocol: value.basicProtocol === true,
    params: { count, size: optionalString(params.size), quality: optionalString(params.quality), aspectRatio: optionalString(params.aspectRatio), resolution: optionalString(params.resolution) },
    ...(Array.isArray(value.imageURLs) ? { imageURLs: value.imageURLs.filter((url): url is string => typeof url === "string" && /^https:\/\//i.test(url)).slice(0, 8) } : {}),
    status: value.status === "generating" && jobIds.length === 0 ? "interrupted" : value.status,
    images: Array.isArray(value.images) ? value.images.flatMap(parseStoredImage) : [],
    error: parseErrorDetail(value.error),
    ...(jobIds.length > 0 ? { jobIds } : {}),
  }];
}

// Jobs that already have a request ID keep polling after a reload; unconfirmed submissions are interrupted.
export function parseVideoRecord(value: unknown): VideoHistoryRecord[] {
  if (!isLocalRecord(value) || typeof value.id !== "string" || typeof value.prompt !== "string" || typeof value.model !== "string") return [];
  const createdAt = finiteTimestamp(value.createdAt);
  if (createdAt === null || !oneOf(value.action, videoActions) || !oneOf(value.status, videoStatuses)) return [];
  const params: Record<string, unknown> = isLocalRecord(value.params) ? value.params : {};
  const requestId = optionalString(value.requestId);
  const video = isLocalRecord(value.video) && typeof value.video.url === "string" && value.video.url && !isInlineURL(value.video.url)
    ? { url: value.video.url, duration: finiteNumber(value.video.duration) }
    : undefined;
  const status = value.status === "submitting" || (value.status === "generating" && !requestId) ? "interrupted" : value.status;
  return [{
    id: value.id,
    createdAt,
    action: value.action,
    prompt: value.prompt,
    model: value.model,
    params: parseVideoParams(params),
    status,
    requestId,
    video,
    error: parseErrorDetail(value.error),
  }];
}

function parseVideoParams(params: Record<string, unknown>): VideoHistoryRecord["params"] {
  const lastFrame = storedVideoFrame(params.lastFrame);
  const keyframes = Array.isArray(params.keyframes) ? params.keyframes.flatMap((frame) => {
    if (!isLocalRecord(frame)) return [];
    const timestampSeconds = finiteNumber(frame.timestampSeconds);
    return timestampSeconds !== undefined ? [{ ...storedVideoFrame(frame), timestampSeconds }] : [];
  }).slice(0, 4) : undefined;
  return {
    duration: finiteNumber(params.duration), aspectRatio: optionalString(params.aspectRatio), resolution: optionalString(params.resolution),
    ...(lastFrame ? { lastFrame } : {}), ...(keyframes?.length ? { keyframes } : {}),
    ...(typeof params.generateAudio === "boolean" ? { generateAudio: params.generateAudio } : {}),
  };
}

function storedVideoFrame(value: unknown): { url?: string; name?: string } | undefined {
  if (!isLocalRecord(value)) return undefined;
  const url = optionalString(value.url)?.trim();
  const name = optionalString(value.name);
  // Retain missing-image placeholders for re-upload, not binary data, File objects or expired IDs.
  return { ...(url && /^https?:\/\//i.test(url) ? { url } : {}), ...(name ? { name } : {}) };
}

export function finiteTimestamp(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) && value > 0 ? value : null;
}

export function isLocalRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parseStoredImage(value: unknown): ImageResult[] {
  if (!isLocalRecord(value) || typeof value.url !== "string" || !value.url || isInlineURL(value.url)) return [];
  return [{ url: value.url, revisedPrompt: optionalString(value.revisedPrompt) }];
}

function parseErrorDetail(value: unknown): ApiErrorDetail | undefined {
  if (!isLocalRecord(value) || typeof value.message !== "string") return undefined;
  return { message: value.message, code: optionalString(value.code), requestId: optionalString(value.requestId) };
}

function newestFirst<T extends { createdAt: number }>(records: T[]): T[] {
  return records.sort((left, right) => right.createdAt - left.createdAt).slice(0, mediaHistoryMaxEntries);
}

function isInlineURL(url: string): boolean {
  return url.startsWith("data:") || url.startsWith("blob:");
}

function optionalString(value: unknown): string | undefined {
  return typeof value === "string" && value ? value : undefined;
}

function finiteNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function oneOf<T extends string>(value: unknown, options: readonly T[]): value is T {
  return typeof value === "string" && (options as readonly string[]).includes(value);
}

function browserStorage(): KeyValueStorage | undefined {
  try {
    return globalThis.localStorage;
  } catch {
    return undefined;
  }
}
