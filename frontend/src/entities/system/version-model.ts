import { z } from "zod";

export const versionInfoSchema = z.object({
  currentVersion: z.string(),
  latestVersion: z.string(),
  updateAvailable: z.boolean(),
  status: z.enum(["unchecked", "up_to_date", "update_available", "check_failed"]),
  checkedAt: z.string().nullable(),
  releaseUrl: z.string(),
  releaseNotes: z.string(),
  error: z.string(),
  canUpdate: z.boolean(),
  // Chinese reason from the server; empty when canUpdate.
  updateBlockedReason: z.string(),
  canRestart: z.boolean(),
  rollbackAvailable: z.boolean(),
});

export type VersionInfoDTO = z.infer<typeof versionInfoSchema>;

// Result of update/apply and update/rollback; restarting means the server restarts itself shortly.
export const updateResultSchema = z.object({ version: z.string(), restarting: z.boolean() });

export type UpdateResultDTO = z.infer<typeof updateResultSchema>;

export type Clock = { now: () => number; sleep: (ms: number) => Promise<void> };

export const systemClock: Clock = { now: () => Date.now(), sleep: (ms) => new Promise((resolve) => setTimeout(resolve, ms)) };

// waitForVersion polls until the restarted server reports target; errors are expected while it is down.
export async function waitForVersion(target: string, fetchVersion: () => Promise<string>, clock: Clock = systemClock, intervalMs = 2_000, timeoutMs = 120_000): Promise<boolean> {
  const deadline = clock.now() + timeoutMs;
  while (clock.now() < deadline) {
    await clock.sleep(intervalMs);
    try {
      if ((await fetchVersion()) === target) return true;
    } catch {
      // Network errors and 5xx while the service restarts.
    }
  }
  return false;
}
