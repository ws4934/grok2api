import type { ModelRouteDTO } from "../../entities/model/types.ts";

type VideoRoute = Pick<ModelRouteDTO, "provider" | "upstreamModel">;

// Public model names can be aliases, and multiple providers may share one name.
// Match the actual targets; the gateway prefers routes that retain every input.
export function videoInputPolicy(routes: readonly VideoRoute[]) {
  const isConsole15 = (route: VideoRoute) => route.provider === "grok_console"
    && route.upstreamModel.replace(/^Console\//, "") === "grok-imagine-video-1.5";
  return {
    maxReferenceImages: Math.max(1, ...routes.map((route) => route.provider === "grok_console" ? (isConsole15(route) ? 14 : 7) : route.provider === "grok_build" ? 8 : 1)),
    canCombineFirstFrame: routes.some(isConsole15),
    canReferenceVoices: routes.some(isConsole15),
    referenceDurationLimit: routes.length > 0 && routes.every((route) => route.provider === "grok_console" && !isConsole15(route)) ? 10 : 15,
  };
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
