import { type ProviderAccountUI } from "@/features/accounts/account-ui";

// Adobe 账号用 firefly.adobe.com 的登录 Cookie 导入，
// 其余展示（计划、积分窗口）沿用默认 UI。
export const adobeAccountUI: Partial<ProviderAccountUI> = {
  importTexts: {
    quickImport: "accounts.adobe.quickImport",
    file: "accounts.importFile",
    title: "accounts.quickImportProviderTitle",
    description: "accounts.adobe.quickImportDescription",
    tokens: "accounts.adobe.tokens",
    placeholder: "accounts.adobe.tokenPlaceholder",
    filename: "adobe-accounts.txt",
  },
};
