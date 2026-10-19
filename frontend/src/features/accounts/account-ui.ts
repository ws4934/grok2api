import type { TFunction } from "i18next";
import type { ComponentType } from "react";
import type { UseFormReturn } from "react-hook-form";
import { z } from "zod";

import type { AccountBatchResultDTO, AccountDTO, AccountTaskProgressDTO, AccountUpdateInput } from "@/features/accounts/accounts-api";

// Contract between the generic accounts page and the provider UI modules in ./providers.

// Every bulk account mutation carries this key so the page can block concurrent tasks.
export const accountTaskKey = ["accounts", "task"] as const;

export type AccountFilterId = "type" | "renewal" | "risk" | "agreement" | "association";
export type AccountFilterOption = { value: string; label: string };
export type AccountFilterDef = { id: AccountFilterId; label: string; options: AccountFilterOption[] };

// A module-owned dialog opened from a row or bulk action; the module defines the kinds.
export type AccountTask = { kind: string; targets: string[] | "all"; account?: AccountDTO };

export type AccountUIContext = {
  provider: string;
  // A bulk task is running.
  pending: boolean;
  task: AccountTask | null;
  openTask: (task: AccountTask) => void;
  closeTask: () => void;
  clearSelection: () => void;
  invalidate: () => void;
  showError: (error: unknown) => void;
};

// i18n keys for the import entries (filename is literal; the backend reads text parts by extension).
// formats: extra menu entries for files of other tools; the backend recognizes them by content.
export type AccountImportTexts = { quickImport: string; file: string; title: string; description: string; tokens: string; placeholder: string; filename: string; formats?: readonly string[] };

export type ProviderAccountUI = {
  TypeCell: ComponentType<{ account: AccountDTO }>;
  QuotaCell: ComponentType<{ account: AccountDTO; locale: string; quotaModes: readonly string[] }>;
  importTexts: AccountImportTexts;
  syncAllDescription: string;
  // The type filter sits before status/egress; the other filters follow them.
  typeOptions?: (t: TFunction) => AccountFilterOption[];
  filters?: (t: TFunction) => AccountFilterDef[];
  // Providers whose linked accounts may be deleted together with this provider's accounts.
  linkedProviders?: readonly string[];
  // Egress scopes accounts may bind to; defaults to the provider id. The second scope is a compatible fallback pool.
  egressScopes?: readonly string[];
  // An exhausted window of this mode shows the account as waiting for reset.
  blockingQuotaMode?: string;
  // Quota modes that only route requests (derived from shown windows); not displayed.
  hiddenQuotaModes?: readonly string[];
  // Overrides the provider refresh-quotas task and the per-account quota refresh.
  syncAll?: (onProgress: (value: AccountTaskProgressDTO) => void, signal: AbortSignal) => Promise<AccountBatchResultDTO>;
  refreshQuota?: (id: string) => Promise<unknown>;
  // Quota dialogs offer a local quota reset next to the sync.
  quotaReset?: boolean;
  // Offers "renew all" for every refreshable account of the provider.
  renewAll?: boolean;
  EditFields?: ComponentType<{ account: AccountDTO; form: UseFormReturn<AccountEditForm> }>;
  editInput?: (values: AccountEditForm, account: AccountDTO) => Partial<AccountUpdateInput>;
  RowMenuItems?: ComponentType<{ account: AccountDTO; ctx: AccountUIContext }>;
  BulkActions?: ComponentType<{ targets: string[] | "all"; ctx: AccountUIContext }>;
  Dialogs?: ComponentType<{ ctx: AccountUIContext }>;
  // Extra guidance shown in the redirect (callback URL) login step.
  AuthHelp?: ComponentType;
};

export function isAbortError(error: unknown): boolean {
  return (error instanceof DOMException || error instanceof Error) && error.name === "AbortError";
}

export function resolveAccountUI<T extends object>(provider: string, modules: Readonly<Record<string, Partial<T>>>, fallback: T): T {
  return { ...fallback, ...modules[provider] };
}

export function accountEditSchema(t: TFunction) {
  return z.object({
    name: z.string().min(1, t("errors.required")),
    enabled: z.boolean(),
    priority: z.number().int(),
    maxConcurrent: z.number().int().min(1, t("errors.positive")).max(256),
    minimumRemaining: z.number().min(0),
    cloudflareCookies: z.string().max(16 << 10, t("settings.invalidValue")),
    clearCloudflareCookies: z.boolean(),
    buildSuperEntitled: z.boolean(),
    buildRouteMode: z.enum(["auto", "build", "xai"]),
  });
}

export type AccountEditForm = z.infer<ReturnType<typeof accountEditSchema>>;

export type ConnectMethod = "deviceOAuth" | "redirectOAuth" | "import";

// Connect menu entries in display order, from the catalog credential flags.
export function connectMethods(credential?: Readonly<Record<ConnectMethod, boolean>>): ConnectMethod[] {
  return credential ? (["deviceOAuth", "redirectOAuth", "import"] as const).filter((method) => credential[method]) : [];
}

export function accountEgressScopes(provider: string, ui: Pick<ProviderAccountUI, "egressScopes">): readonly string[] {
  return ui.egressScopes ?? [provider];
}

// Windows in catalog mode order; without catalog modes every window is shown as received.
export function orderQuotaWindows<W extends { mode: string }>(modes: readonly string[], windows: readonly W[]): W[] {
  if (modes.length === 0) return [...windows];
  return modes.flatMap((mode) => windows.filter((window) => window.mode === mode));
}

// Device logins poll at the server interval; a slow_down answer adds five seconds.
export function authPollDelayMs(intervalSeconds: number | undefined, slowDown = false): number {
  return ((intervalSeconds && intervalSeconds > 0 ? intervalSeconds : 5) + (slowDown ? 5 : 0)) * 1000;
}

// Milliseconds until the session expires, or undefined when the timestamp is unusable.
export function authSessionRemainingMs(expiresAt: string, now = Date.now()): number | undefined {
  const at = Date.parse(expiresAt);
  return Number.isNaN(at) ? undefined : Math.max(0, at - now);
}
