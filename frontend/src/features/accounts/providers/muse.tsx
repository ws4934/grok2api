import { type ProviderAccountUI } from "@/features/accounts/account-ui";

// Muse accounts are imported from muse.ai login cookies (hatch_sess is required).
// The plan and the weekly usage window use the default UI; one weekly limit covers
// every Muse model, so an exhausted window means the account waits for its reset.
export const museAccountUI: Partial<ProviderAccountUI> = {
  importTexts: {
    quickImport: "accounts.muse.quickImport",
    file: "accounts.importFile",
    title: "accounts.quickImportProviderTitle",
    description: "accounts.muse.quickImportDescription",
    tokens: "accounts.muse.tokens",
    placeholder: "accounts.muse.tokenPlaceholder",
    filename: "muse-accounts.txt",
  },
  blockingQuotaMode: "weekly_usage",
};
