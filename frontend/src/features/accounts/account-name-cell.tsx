import { Bot, Handshake, VenusAndMars } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useProviders, type ProviderDTO } from "@/entities/provider/provider-api";
import { providerIcon } from "@/entities/provider/provider-icon";
import { providerColor, providerLabel, providerRank } from "@/entities/provider/provider-presentation";
import type { AccountDTO, LinkedAccountDTO } from "@/features/accounts/accounts-api";
import { accountDisplayName } from "@/features/accounts/account-identity";
import { cn } from "@/shared/lib/cn";
import { formatDateTime } from "@/shared/lib/format";

function identityDetails(name: string, email?: string, userId?: string): string[] {
  const values = [email?.trim() || name.trim(), userId?.trim()];
  const seen = new Set<string>();
  return values.filter((value): value is string => {
    if (!value) return false;
    const normalized = value.toLocaleLowerCase();
    if (seen.has(normalized)) return false;
    seen.add(normalized);
    return true;
  });
}

function accountLinks(account: AccountDTO, providers?: ProviderDTO[]): LinkedAccountDTO[] {
  const links = account.linkedAccounts ?? (account.linkedAccountId && account.linkedProvider
    ? [{ id: account.linkedAccountId, provider: account.linkedProvider, name: account.linkedAccountName ?? "" }]
    : []);
  return [...links].sort((left, right) => providerRank(left.provider, providers) - providerRank(right.provider, providers) || left.id.localeCompare(right.id));
}

export function AccountNameCell({ account }: { account: AccountDTO }) {
  const { t, i18n } = useTranslation();
  const providers = useProviders().data;
  const links = accountLinks(account, providers);
  const displayName = accountDisplayName(account);
  const connections = [
    { id: account.id, provider: account.provider, details: identityDetails(account.name, account.email, account.userId) },
    ...links.filter((linked) => linked.provider !== account.provider).map((linked) => ({ id: linked.id, provider: linked.provider, details: identityDetails(linked.name, linked.email, linked.userId) })),
  ].sort((left, right) => providerRank(left.provider, providers) - providerRank(right.provider, providers));

  return (
    <div className="flex min-h-9 min-w-0 flex-col justify-center gap-0.5">
      <div className="flex min-w-0 items-center gap-1.5">
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="min-w-0 truncate text-xs font-medium">{displayName}</span>
          </TooltipTrigger>
          <TooltipContent>{displayName}</TooltipContent>
        </Tooltip>
      </div>
      <div className="flex min-h-4 w-fit min-w-0 items-center">
        <Tooltip>
          <TooltipTrigger asChild>
            <div
              tabIndex={0}
              aria-label={connections.map((connection) => providerLabel(connection.provider, providers)).join(", ")}
              className="flex min-w-0 cursor-help items-center gap-1.5 overflow-hidden rounded-sm focus-visible:outline-none"
            >
              {connections.map((connection) => {
                const ProviderIcon = providerIcon(connection.provider);
                return <ProviderIcon key={`${connection.provider}:${connection.id}`} className="size-3.5 shrink-0" style={{ color: providerColor(connection.provider, providers) }} />;
              })}
            </div>
          </TooltipTrigger>
          <TooltipContent className="min-w-44 space-y-2 px-2.5 py-2">
            {connections.map((connection) => {
              const label = providerLabel(connection.provider, providers);
              const ProviderIcon = providerIcon(connection.provider);
              return (
                <div key={`${connection.provider}:${connection.id}`} className="space-y-0.5">
                  <div className="flex items-center gap-1.5 text-xs font-medium">
                    <ProviderIcon className="size-3.5" style={{ color: providerColor(connection.provider, providers) }} />
                    <span>{label}</span>
                  </div>
                  {(connection.details.length > 0 ? connection.details : [label]).map((detail, index) => (
                    <p key={detail} className={cn("max-w-64 truncate pl-5 text-xs", index > 0 && "text-primary-foreground/70")}>{detail}</p>
                  ))}
                </div>
              );
            })}
          </TooltipContent>
        </Tooltip>
        {account.termsAcceptedAt || account.nsfwEnabledAt ? (
          <>
            <span className="mx-2 h-3 w-px shrink-0 bg-border" aria-hidden="true" />
            <span className="flex items-center gap-1.5">
              {account.termsAcceptedAt ? (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <span tabIndex={0} aria-label={t("webAccountSettings.acceptTerms")} className="inline-flex cursor-help text-pink-500 focus-visible:outline-none dark:text-pink-400">
                      <Handshake className="size-3.5" />
                    </span>
                  </TooltipTrigger>
                  <TooltipContent>{t("webAccountSettings.acceptTerms")} · {formatDateTime(account.termsAcceptedAt, i18n.language)}</TooltipContent>
                </Tooltip>
              ) : null}
              {account.nsfwEnabledAt ? (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <span tabIndex={0} aria-label={t("accounts.nsfwEnabledMark")} className="inline-flex cursor-help text-yellow-500 focus-visible:outline-none dark:text-yellow-400">
                      <VenusAndMars className="size-3.5" />
                    </span>
                  </TooltipTrigger>
                  <TooltipContent>{t("accounts.nsfwEnabledTooltip", { time: formatDateTime(account.nsfwEnabledAt, i18n.language) })}</TooltipContent>
                </Tooltip>
              ) : null}
            </span>
          </>
        ) : null}
        {account.buildBotFlagged ? (
          <>
            <span className="mx-2 h-3 w-px shrink-0 bg-border" aria-hidden="true" />
            <Tooltip>
              <TooltipTrigger asChild>
                <span
                  tabIndex={0}
                  aria-label={t("accounts.botRisk")}
                  className={
                    account.buildBotFlagSource === 2
                      ? "inline-flex cursor-help text-rose-500 focus-visible:outline-none dark:text-rose-400"
                      : "inline-flex cursor-help text-amber-500 focus-visible:outline-none dark:text-amber-400"
                  }
                >
                  <Bot className="size-3.5" />
                </span>
              </TooltipTrigger>
              <TooltipContent>
                {account.buildBotFlagSource === 2
                  ? t("accounts.botRiskTooltipSource2")
                  : account.buildBotFlagSource === 1
                    ? t("accounts.botRiskTooltipSource1")
                    : t("accounts.botRiskTooltip")}
              </TooltipContent>
            </Tooltip>
          </>
        ) : null}
      </div>
    </div>
  );
}
