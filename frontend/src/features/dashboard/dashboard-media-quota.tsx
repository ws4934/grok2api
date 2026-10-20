import { useTranslation } from "react-i18next";

import { Spinner } from "@/components/ui/spinner";
import { useProviders } from "@/entities/provider/provider-api";
import { providerColor, providerLabel, providerRank } from "@/entities/provider/provider-presentation";
import type { DashboardDTO } from "@/features/dashboard/dashboard-api";
import { DashboardPanel } from "@/features/dashboard/dashboard-panel";
import { formatNumber } from "@/shared/lib/format";

type DashboardMediaQuotaProps = {
  dashboard?: DashboardDTO;
  locale: string;
  loading: boolean;
};

const KIND_ORDER = ["image", "video", "credits"];

// Remaining image/video quota per provider, summed over usable accounts; hidden when no provider reports any.
export function DashboardMediaQuota({ dashboard, locale, loading }: DashboardMediaQuotaProps) {
  const { t } = useTranslation();
  const catalog = useProviders().data;
  const items = [...(dashboard?.mediaQuota ?? [])].sort((a, b) =>
    providerRank(a.provider, catalog) - providerRank(b.provider, catalog) || KIND_ORDER.indexOf(a.kind) - KIND_ORDER.indexOf(b.kind));
  if (!loading && items.length === 0) return null;

  return (
    <DashboardPanel
      id="dashboard-media-quota-title"
      title={t("dashboard.mediaQuotaTitle")}
      actions={<span className="text-[11px] text-muted-foreground">{t("dashboard.mediaQuotaScope")}</span>}
    >
      {loading ? (
        <div className="flex min-h-24 items-center justify-center"><Spinner className="size-5" /></div>
      ) : (
        <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4">
          {items.map((item) => {
            const color = providerColor(item.provider, catalog);
            const label = `${providerLabel(item.provider, catalog)} · ${t(`dashboard.mediaQuotaKinds.${item.kind}`, { defaultValue: item.kind })}`;
            const share = Math.min(100, item.remaining / Math.max(1, item.total) * 100);
            const detail = [
              t("dashboard.mediaQuotaAccounts", { count: item.accounts }),
              item.total > 0 ? null : t("dashboard.mediaQuotaRemainingOnly"),
              item.kind === "credits" ? t("dashboard.mediaQuotaShared") : null,
            ].filter(Boolean).join(" · ");
            return (
              <div key={`${item.provider}:${item.kind}`} className="min-w-0 rounded-md bg-muted/40 p-3">
                <div className="flex min-w-0 items-center gap-2">
                  <span className="size-2 shrink-0 rounded-full" style={{ backgroundColor: color }} />
                  <p className="truncate text-xs" title={label}>{label}</p>
                </div>
                <p className="mt-2 text-lg font-medium tabular-nums">
                  {formatNumber(item.remaining, locale)}
                  {item.total > 0 ? <span className="text-xs font-normal text-muted-foreground"> / {formatNumber(item.total, locale)}</span> : null}
                </p>
                {/* ChatGPT reports only what is left, so there is no total to draw against. */}
                {item.total > 0 ? (
                  <div
                    className="mt-2 h-1.5 overflow-hidden rounded-full bg-muted"
                    role="progressbar"
                    aria-label={label}
                    aria-valuemin={0}
                    aria-valuemax={item.total}
                    aria-valuenow={item.remaining}
                  >
                    <div className="h-full rounded-full" style={{ width: `${share}%`, backgroundColor: color }} />
                  </div>
                ) : null}
                <p className="mt-1.5 truncate text-[11px] text-muted-foreground" title={detail}>{detail}</p>
              </div>
            );
          })}
        </div>
      )}
    </DashboardPanel>
  );
}
