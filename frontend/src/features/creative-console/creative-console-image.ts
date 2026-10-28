import type { ModelRouteDTO } from "../../entities/model/types.ts";
import { isGrokProvider } from "../../entities/provider/provider-presentation.ts";
import type { ImageParams } from "./creative-console-api.ts";

type ImageRoute = Pick<ModelRouteDTO, "provider" | "upstreamModel">;

const consoleRatios = ["1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "2:1", "1:2", "auto"];
const webRatios = [...consoleRatios, "19.5:9", "9:19.5", "20:9", "9:20"];
const web20Ratios = ["1:1", "16:9", "9:16", "4:3", "3:2", "2:3", "21:9", "5:2", "auto"];

export type ImageGenerationPolicy = {
  basicProtocol: boolean;
  aspectRatios: readonly string[];
  resolutions: readonly string[];
  qualities: readonly string[];
};

// A public ID can be renamed or shared by several providers. Only offer Grok
// controls every eligible target implements; the request cannot pin one route.
export function imageGenerationPolicy(routes: readonly ImageRoute[]): ImageGenerationPolicy {
  const basicProtocol = routes.length > 0 && routes.every((route) => !isGrokProvider(route.provider));
  if (basicProtocol || routes.length === 0) return { basicProtocol, aspectRatios: [], resolutions: [], qualities: [] };
  const ratios = routes.map((route) => {
    if (route.provider === "grok_console") return consoleRatios;
    if (route.provider === "grok_web" && route.upstreamModel === "grok-imagine-image-2.0") return web20Ratios;
    if (route.provider === "grok_web" && route.upstreamModel !== "grok-imagine-image") return webRatios;
    return [];
  });
  const consoleOnly = routes.every((route) => route.provider === "grok_console");
  return {
    basicProtocol,
    aspectRatios: ratios[0].filter((ratio) => ratios.every((values) => values.includes(ratio))),
    resolutions: consoleOnly ? ["1k", "2k"] : [],
    qualities: consoleOnly && routes.every((route) => route.upstreamModel === "grok-imagine-image-2.0") ? ["auto", "low", "medium"] : [],
  };
}

export function imageReferencePolicy(editRoutes: readonly ImageRoute[]) {
  return {
    limit: editRoutes.length === 0 ? 0 : Math.min(...editRoutes.map((route) => route.provider === "grok_console" ? 3
      : route.provider === "grok_web" && route.upstreamModel === "grok-imagine-image-2.0" ? 5 : route.provider === "muse" ? 4 : 8)),
  };
}

export function parseImageReferenceURLs(value: string) {
  const urls = value.split(/\r?\n/).map((url) => url.trim()).filter(Boolean);
  return { urls, invalid: urls.some((value) => {
    try { const url = new URL(value); return url.protocol !== "https:" || Boolean(url.username || url.password); }
    catch { return true; }
  }) };
}

// Use the same effective values for controls, outbound requests and new history
// records. Hidden Web options must not survive via an earlier Console selection.
export function imageGenerationParams(policy: ImageGenerationPolicy, params: ImageParams): ImageParams {
  if (policy.basicProtocol) return { count: params.count, size: params.size || "auto", quality: params.quality || "auto" };
  const choose = (values: readonly string[], value: string | undefined) => value && values.includes(value) ? value : values[0];
  const aspectRatio = choose(policy.aspectRatios, params.aspectRatio);
  const resolution = choose(policy.resolutions, params.resolution);
  const quality = choose(policy.qualities, params.quality);
  return {
    count: params.count,
    ...(aspectRatio ? { aspectRatio } : {}),
    ...(resolution ? { resolution } : {}),
    ...(quality ? { quality } : {}),
  };
}
