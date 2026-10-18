import { useTranslation } from "react-i18next";

import type { ProviderAccountUI } from "@/features/accounts/account-ui";

// Plan badge and reason/image_gen/file_upload quota bars come from the default UI (plan field and catalog quota modes).

function AuthHelp() {
  const { t } = useTranslation();
  return (
    <p className="text-xs leading-5 text-muted-foreground">
      {t("accounts.chatgpt.authHelp")}
      <code className="mt-1 block break-all font-mono text-[11px] text-foreground">https://platform.openai.com/auth/callback?code=…&amp;state=…</code>
    </p>
  );
}

export const chatgptAccountUI: Partial<ProviderAccountUI> = {
  importTexts: {
    quickImport: "accounts.chatgpt.quickImport", file: "accounts.importFile", title: "accounts.quickImportProviderTitle", description: "accounts.chatgpt.quickImportDescription",
    tokens: "accounts.chatgpt.tokens", placeholder: "accounts.chatgpt.tokenPlaceholder", filename: "chatgpt-accounts.txt",
  },
  AuthHelp,
};
