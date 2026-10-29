import { apiRequest } from "@/shared/api/client";
import { logQuery, systemLogsSchema, type LogFilters } from "./system-logs-model";

export function getSystemLogs(filters: LogFilters, cursor: string, signal: AbortSignal) {
  return apiRequest(`/api/admin/v1/system/logs?${logQuery(filters, cursor)}`, { signal }, (value) => systemLogsSchema.parse(value));
}
