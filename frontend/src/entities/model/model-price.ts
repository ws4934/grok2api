import type { ModelRouteDTO } from "@/entities/model/types";

// Mirrors backend domain/pricing; dependency-free so node --test can load it.

export const priceOperations = ["text", "image", "image_edit", "video", "tts", "stt"] as const;
export type PriceOperation = (typeof priceOperations)[number];

export const priceComponentKeys = [
  "inputTokenUSDPerMillion",
  "cachedInputTokenUSDPerMillion",
  "outputTokenUSDPerMillion",
  "inputImageUSD",
  "outputImageUSD",
  "outputSecondUSD",
  "characterUSDPerMillion",
] as const;
export type PriceComponent = (typeof priceComponentKeys)[number];
// Decimal USD strings: per million tokens/characters, per image or per second.
export type PriceComponents = Partial<Record<PriceComponent, string>>;

// Effective billing price of a route. Official prices carry components only where they do not
// vary by request options (text, TTS); realtime routes are "none" without an operation.
export type RoutePriceDTO = PriceComponents & {
  source: "official" | "custom" | "none";
  id?: string;
  pricingModel?: string;
  operation?: PriceOperation;
};

export type ModelPriceDTO = PriceComponents & {
  id: string;
  provider: string;
  pricingModel: string;
  operation: PriceOperation;
  active: boolean;
  createdAt: string;
  supersededAt?: string;
};

const capabilityOperations: Partial<Record<ModelRouteDTO["capability"], PriceOperation>> = {
  responses: "text", chat: "text", image: "image", image_edit: "image_edit", video: "video", tts: "tts", stt: "stt",
};

// priceOperation is the billing operation of a route capability; realtime has none.
export function priceOperation(capability: ModelRouteDTO["capability"]): PriceOperation | undefined {
  return capabilityOperations[capability];
}

// Display order per operation; the optional component (if any) falls back on the server:
// cached input → input rate, video input image → 0.
const operationComponents: Record<PriceOperation, readonly PriceComponent[]> = {
  text: ["inputTokenUSDPerMillion", "cachedInputTokenUSDPerMillion", "outputTokenUSDPerMillion"],
  image: ["outputImageUSD"],
  image_edit: ["inputImageUSD", "outputImageUSD"],
  video: ["inputImageUSD", "outputSecondUSD"],
  tts: ["characterUSDPerMillion"],
  stt: ["outputSecondUSD"],
};
const optionalComponents: Partial<Record<PriceOperation, PriceComponent>> = { text: "cachedInputTokenUSDPerMillion", video: "inputImageUSD" };
// Components summarised in one line; the rest only appear in details.
const headlineComponents: Record<PriceOperation, readonly PriceComponent[]> = {
  text: ["inputTokenUSDPerMillion", "outputTokenUSDPerMillion"],
  image: ["outputImageUSD"],
  image_edit: ["outputImageUSD"],
  video: ["outputSecondUSD"],
  tts: ["characterUSDPerMillion"],
  stt: ["outputSecondUSD"],
};

export function priceComponents(operation: PriceOperation): Array<{ component: PriceComponent; required: boolean }> {
  return operationComponents[operation].map((component) => ({ component, required: optionalComponents[operation] !== component }));
}

// priceHeadline is the one-line summary of a price: its main rates and their shared unit.
export function priceHeadline(operation: PriceOperation, price: PriceComponents): { values: string[]; unit: PriceUnit } {
  const components = headlineComponents[operation];
  const values = components.flatMap((component) => {
    const value = price[component];
    return value === undefined ? [] : [formatPriceUSD(value)];
  });
  return { values, unit: priceComponentUnit(components[0]) };
}

export type PriceUnit = "million_tokens" | "million_characters" | "image" | "second";

export function priceComponentUnit(component: PriceComponent): PriceUnit {
  if (component === "characterUSDPerMillion") return "million_characters";
  if (component.endsWith("PerMillion")) return "million_tokens";
  return component.endsWith("ImageUSD") ? "image" : "second";
}

// 1 USD = 1e10 ticks, stored per token/character or per image/second, so a per-million rate
// has 4 decimals of precision and a per-unit rate 10. Upper limits keep cost × quantity in int64.
export function priceComponentScale(component: PriceComponent): { digits: number; limit: number } {
  return component.endsWith("PerMillion") ? { digits: 4, limit: 10_000 } : { digits: 10, limit: 1_000 };
}

export type PriceInputError = "required" | "invalid" | "precision" | "limit";

function priceInputError(component: PriceComponent, value: string, required: boolean): PriceInputError | undefined {
  if (!value) return required ? "required" : undefined;
  const match = /^(\d+)(?:\.(\d+))?$/.exec(value);
  if (!match) return "invalid";
  const fraction = (match[2] ?? "").replace(/0+$/, "");
  const { digits, limit } = priceComponentScale(component);
  if (fraction.length > digits) return "precision";
  const integer = Number(match[1]);
  return integer > limit || (integer === limit && fraction) ? "limit" : undefined;
}

// checkPriceInput validates form values the way the server does and returns the request
// components: only the operation's non-empty values, trimmed.
export function checkPriceInput(operation: PriceOperation, values: PriceComponents): { errors: Partial<Record<PriceComponent, PriceInputError>>; components: PriceComponents } {
  const errors: Partial<Record<PriceComponent, PriceInputError>> = {};
  const components: PriceComponents = {};
  for (const { component, required } of priceComponents(operation)) {
    const value = (values[component] ?? "").trim();
    const error = priceInputError(component, value, required);
    if (error) errors[component] = error;
    else if (value) components[component] = value;
  }
  return { errors, components };
}

// formatPriceUSD renders an exact decimal USD string with thousands separators.
export function formatPriceUSD(value: string): string {
  const [integer, fraction] = value.split(".");
  const grouped = integer.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return `$${fraction === undefined ? grouped : `${grouped}.${fraction}`}`;
}

const priceSources: readonly string[] = ["official", "custom", "none"];

// decodeRoutePrice keeps what it recognises and drops the rest, so a malformed or newer
// price never fails the model list.
export function decodeRoutePrice(value: unknown): RoutePriceDTO | undefined {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return undefined;
  const record = value as Record<string, unknown>;
  if (typeof record.source !== "string" || !priceSources.includes(record.source)) return undefined;
  const price: RoutePriceDTO = { source: record.source as RoutePriceDTO["source"] };
  if (typeof record.id === "string" || typeof record.id === "number") price.id = String(record.id);
  if (typeof record.pricingModel === "string" && record.pricingModel) price.pricingModel = record.pricingModel;
  if (priceOperations.includes(record.operation as PriceOperation)) price.operation = record.operation as PriceOperation;
  for (const component of priceComponentKeys) {
    const rate = record[component];
    if (typeof rate === "string" && /^\d+(\.\d+)?$/.test(rate)) price[component] = rate;
  }
  return price;
}

export type RoutePriceEntry<T> = { route: T; operation: PriceOperation; pricingModel: string; price: RoutePriceDTO };

// distinctRoutePrices lists a route group's priceable routes once per billed price
// (chat and responses on one upstream model share the text price).
export function distinctRoutePrices<T extends Pick<ModelRouteDTO, "capability" | "price">>(routes: readonly T[]): RoutePriceEntry<T>[] {
  const seen = new Set<string>();
  const result: RoutePriceEntry<T>[] = [];
  for (const route of routes) {
    const operation = priceOperation(route.capability);
    const pricingModel = route.price?.pricingModel;
    if (!operation || !route.price || !pricingModel) continue;
    const key = `${operation}/${pricingModel}`;
    if (seen.has(key)) continue;
    seen.add(key);
    result.push({ route, operation, pricingModel, price: route.price });
  }
  return result;
}
