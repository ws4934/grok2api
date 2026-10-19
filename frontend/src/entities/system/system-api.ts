import { updateResultSchema, versionInfoSchema, type UpdateResultDTO, type VersionInfoDTO } from "@/entities/system/version-model";
import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, isString } from "@/shared/api/decoder";

export type SystemInfoDTO = {
  publicApiBaseURL: string;
};

const decodeSystemInfo = createObjectDecoder<SystemInfoDTO>("system info", { publicApiBaseURL: isString });

export function getSystemInfo(): Promise<SystemInfoDTO> {
  return apiRequest("/api/admin/v1/system", {}, decodeSystemInfo);
}

export function getVersionInfo(): Promise<VersionInfoDTO> {
  return apiRequest("/api/admin/v1/system/version", {}, (value) => versionInfoSchema.parse(value));
}

export function checkForUpdates(): Promise<VersionInfoDTO> {
  return apiRequest("/api/admin/v1/system/update/check", { method: "POST" }, (value) => versionInfoSchema.parse(value));
}

export function applyUpdate(): Promise<UpdateResultDTO> {
  return apiRequest("/api/admin/v1/system/update/apply", { method: "POST" }, (value) => updateResultSchema.parse(value));
}

export function rollbackUpdate(): Promise<UpdateResultDTO> {
  return apiRequest("/api/admin/v1/system/update/rollback", { method: "POST" }, (value) => updateResultSchema.parse(value));
}
