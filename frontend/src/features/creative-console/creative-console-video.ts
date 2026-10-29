import type { ModelRouteDTO } from "../../entities/model/types.ts";

type VideoRoute = Pick<ModelRouteDTO, "provider" | "upstreamModel">;

// Public model names can be aliases, and multiple providers may share one name.
// Match the actual targets; the gateway prefers routes that retain every input.
export function videoInputPolicy(routes: readonly VideoRoute[]) {
  const isConsole15 = (route: VideoRoute) => route.provider === "grok_console"
    && route.upstreamModel.replace(/^Console\//, "") === "grok-imagine-video-1.5";
  return {
    maxReferenceImages: Math.max(1, ...routes.map((route) => route.provider === "grok_console" ? (isConsole15(route) ? 14 : 7) : route.provider === "grok_build" ? 8 : route.provider === "grok_web" ? 7 : 1)),
    canCombineFirstFrame: routes.some((route) => isConsole15(route) || route.provider === "grok_web"),
    canReferenceVoices: routes.some(isConsole15),
    canPinFrames: routes.some(isConsole15),
    canGenerateAudio: routes.some((route) => route.provider === "grok_console"),
    referenceDurationLimit: routes.length > 0 && routes.every((route) => route.provider === "grok_web" || route.provider === "grok_console" && !isConsole15(route)) ? 10 : 15,
  };
}

// Web uses asset roles, not the Console/API <IMAGE_n> prompt-token contract.
export function videoReferenceLabel(routes: readonly VideoRoute[], index: number, hasFirstFrame: boolean): string {
  return routes.some((route) => route.provider === "grok_web")
    ? `#${index + 1}`
    : `<IMAGE_${index + (hasFirstFrame ? 1 : 0)}>`;
}

// Files stay in memory until submit; history stores a name to request re-upload,
// never a short-lived file_id that has already been released by the job.
export type VideoFrameInput = { url?: string; file?: File; name?: string };
export type VideoKeyframeInput = VideoFrameInput & { id: string; timestamp: string };
export type VideoFrameIssue = "unsupported" | "image" | "time" | "duplicate" | "limit";

export function videoFrameIssue(lastFrame: VideoFrameInput | undefined, keyframes: readonly VideoKeyframeInput[], duration: number, supported: boolean): VideoFrameIssue | undefined {
  if ((lastFrame || keyframes.length) && !supported) return "unsupported";
  if (keyframes.length > 4) return "limit";
  for (const frame of [...(lastFrame ? [lastFrame] : []), ...keyframes]) {
    if (!frame.file && !parseVideoReferenceURL(frame.url ?? "")) return "image";
  }
  const slots = new Set<number>();
  for (const frame of keyframes) {
    const time = Number(frame.timestamp);
    if (!frame.timestamp.trim() || !Number.isFinite(time) || time <= 0 || time >= duration) return "time";
    const slot = Math.round(time * 3);
    if (slots.has(slot)) return "duplicate";
    slots.add(slot);
  }
  return undefined;
}

export function snapshotVideoFrame(frame: VideoFrameInput): { url?: string; name?: string } {
  const url = frame.url?.trim();
  return { ...(url && /^https?:\/\//i.test(url) ? { url } : {}), ...(frame.file || frame.name ? { name: frame.file?.name ?? frame.name } : {}) };
}

export type VideoReferenceImage = {
  id: string;
  name: string;
  url?: string;
  fileId?: string;
};

export function parseVideoReferenceURL(value: string): string | undefined {
  const url = value.trim();
  try {
    const { protocol } = new URL(url);
    if (protocol === "http:" || protocol === "https:" || (protocol === "data:" && /^data:image\//i.test(url))) return url;
  } catch { /* Invalid URLs stay in the editor for correction. */ }
  return undefined;
}
