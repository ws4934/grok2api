import { z } from "zod";

export const systemLogSchema = z.object({
  id: z.number().int().positive(), createdAt: z.string(), level: z.string(), message: z.string(),
  instanceId: z.string(), provider: z.string(), accountId: z.number().int().nonnegative(), requestId: z.string(),
  fields: z.record(z.string(), z.unknown()),
});
export const systemLogsSchema = z.object({
  items: z.array(systemLogSchema), nextCursor: z.string(),
  status: z.object({
    queueDepth: z.number().nonnegative(), dropped: z.number().nonnegative(), writeFailures: z.number().nonnegative(),
    lastError: z.string(), lastFailureAt: z.string().nullable(), lastSavedAt: z.string().nullable(),
    retentionDays: z.number().positive(), maxEntries: z.number().positive(),
  }),
});
export type SystemLog = z.infer<typeof systemLogSchema>;
export type SystemLogs = z.infer<typeof systemLogsSchema>;
export type LogFilters = { search: string; level: string; provider: string; accountId: string; requestId: string };

export function logQuery(filters: LogFilters, cursor: string): string {
  const params = new URLSearchParams({ limit: "50" });
  for (const [key, raw] of Object.entries({ ...filters, cursor })) {
    const value = raw.trim(); if (value) params.set(key, value);
  }
  return params.toString();
}

export function logReason(entry: SystemLog): string {
  return typeof entry.fields.error === "string" ? entry.fields.error : "";
}

export function logSummary(entry: SystemLog): string {
  const reason = logReason(entry);
  if (reason) return reason;
  const { method, path, status, duration_ms: duration } = entry.fields;
  if (entry.message === "http_request" && typeof method === "string" && typeof path === "string") {
    return `${method} ${path}${typeof status === "number" ? ` · HTTP ${status}` : ""}${typeof duration === "number" ? ` · ${duration} ms` : ""}`;
  }
  return "";
}
