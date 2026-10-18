import type { RoutePriceDTO } from "@/entities/model/model-price";

export type ModelRouteDTO = {
  id: string;
  publicId: string;
  provider: string;
  upstreamModel: string;
  capability: "responses" | "chat" | "image" | "image_edit" | "video" | "tts" | "stt" | "realtime";
  origin: "catalog" | "discovered" | "manual";
  enabled: boolean;
  accountIds: string[];
  bindingMode: boolean;
  supportedAccounts: number;
  syncedAccounts: number;
  totalAccounts: number;
  capabilityKnown: boolean;
  available: boolean;
  lastSyncedAt?: string;
  // Absent when the server has no price book.
  price?: RoutePriceDTO;
};

export type ModelEndpointCapability = "completions" | "responses" | "messages" | "image" | "image_edit" | "video" | "tts" | "stt" | "realtime";

export type ModelRouteGroupDTO = {
  key: string;
  routes: ModelRouteDTO[];
  endpointCapabilities: ModelEndpointCapability[];
};
