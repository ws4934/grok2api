import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { History, Trash2 } from "lucide-react";
import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { deleteModelPrice, listModelPrices, putModelPrice } from "@/entities/model/model-api";
import { checkPriceInput, formatPriceUSD, priceComponents, priceComponentScale, priceComponentUnit, type PriceComponent, type PriceComponents, type PriceInputError, type PriceOperation, type RoutePriceDTO, type RoutePriceEntry } from "@/entities/model/model-price";
import type { ModelRouteDTO } from "@/entities/model/types";
import type { ProviderDTO } from "@/entities/provider/provider-api";
import { providerLabel } from "@/entities/provider/provider-presentation";
import { cn } from "@/shared/lib/cn";
import { formatDateTime } from "@/shared/lib/format";

export function ModelPriceDialog({ target, providers, onClose }: { target: RoutePriceEntry<ModelRouteDTO>; providers?: ProviderDTO[]; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const { route, operation, pricingModel, price } = target;
  const customID = price.source === "custom" ? price.id : undefined;
  const [values, setValues] = useState<PriceComponents>(() => customID ? price : {});
  const [errors, setErrors] = useState<Partial<Record<PriceComponent, PriceInputError>>>({});
  const [showHistory, setShowHistory] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const historyQuery = useQuery({
    queryKey: ["model-prices", route.provider, pricingModel],
    queryFn: () => listModelPrices({ provider: route.provider, model: pricingModel, includeHistory: true }),
    enabled: showHistory,
  });
  const history = historyQuery.data?.items.filter((item) => item.operation === operation) ?? [];

  // Refresh on failure too: a 404/409 means the shown price is stale.
  function refresh(): void {
    void queryClient.invalidateQueries({ queryKey: ["models"] });
    void queryClient.invalidateQueries({ queryKey: ["model-prices"] });
  }
  function finish(message: string): void {
    refresh();
    toast.success(message);
    onClose();
  }
  function fail(error: unknown): void {
    refresh();
    toast.error(error instanceof Error ? error.message : t("errors.generic"));
  }
  const saveMutation = useMutation({ mutationFn: putModelPrice, onSuccess: () => finish(t("models.priceSaved")), onError: fail });
  const deleteMutation = useMutation({ mutationFn: deleteModelPrice, onSuccess: () => finish(t("models.priceDeleted")), onError: fail });

  function submit(event: FormEvent): void {
    event.preventDefault();
    const result = checkPriceInput(operation, values);
    setErrors(result.errors);
    if (Object.keys(result.errors).length === 0) saveMutation.mutate({ provider: route.provider, pricingModel, operation, ...result.components });
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="flex max-h-[calc(100svh-2rem)] min-h-0 flex-col gap-0 overflow-hidden p-0 text-xs sm:max-w-[520px]">
        <DialogHeader className="shrink-0 px-5 py-4 pr-12">
          <DialogTitle>{t("models.editPrice")}</DialogTitle>
          <DialogDescription>{t("models.priceDescription", { provider: providerLabel(route.provider, providers), model: pricingModel, operation: t(`models.priceOperations.${operation}`) })}</DialogDescription>
        </DialogHeader>
        <form className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden" onSubmit={submit} noValidate>
          <div className="min-h-0 flex-1 space-y-3 overflow-y-auto overscroll-contain px-5 pb-4 pt-2">
            <section className="space-y-1.5 rounded-lg bg-muted/35 px-3 py-2.5">
              <div className="flex items-center justify-between gap-3">
                <span className="text-muted-foreground">{t("models.priceCurrent")}</span>
                <PriceSourceBadge source={price.source} />
              </div>
              <PriceDetails operation={operation} price={price} labelClassName="text-muted-foreground" />
            </section>
            {priceComponents(operation).map(({ component, required }) => {
              const error = errors[component];
              const id = `model-price-${component}`;
              const scale = priceComponentScale(component);
              return (
                <div key={component} className="space-y-2">
                  <Label htmlFor={id}>{t(`models.priceComponents.${component}`)}</Label>
                  <div className="relative">
                    <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-muted-foreground">$</span>
                    <Input
                      id={id}
                      className="pl-6 pr-28 tabular-nums"
                      inputMode="decimal"
                      autoComplete="off"
                      value={values[component] ?? ""}
                      placeholder={price.source === "official" ? price[component] : undefined}
                      aria-invalid={Boolean(error)}
                      onChange={(event) => setValues((current) => ({ ...current, [component]: event.target.value }))}
                    />
                    <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-muted-foreground">{t(`models.priceUnits.${priceComponentUnit(component)}`)}</span>
                  </div>
                  {error ? <p className="text-destructive">{t(`models.priceErrors.${error}`, { digits: scale.digits, limit: formatPriceUSD(String(scale.limit)) })}</p> : !required ? <p className="text-muted-foreground">{t(`models.priceDefaults.${component}`)}</p> : null}
                </div>
              );
            })}
            {showHistory ? (
              <section className="space-y-1.5 border-t pt-3">
                <div className="font-medium">{t("models.priceHistory")}</div>
                {historyQuery.isPending ? <div className="flex min-h-12 items-center justify-center"><Spinner /></div> : null}
                {historyQuery.isError ? <p className="text-destructive">{historyQuery.error.message}</p> : null}
                {historyQuery.isSuccess && history.length === 0 ? <p className="text-muted-foreground">{t("models.priceHistoryEmpty")}</p> : null}
                {history.map((item) => (
                  <div key={item.id} className="rounded-md bg-muted/25 px-3 py-2">
                    <div className="flex items-center justify-between gap-3 text-muted-foreground">
                      <span className="tabular-nums">#{item.id} · {formatDateTime(item.createdAt, i18n.language)}</span>
                      <span className={cn(item.active && "text-emerald-700 dark:text-emerald-300")}>{item.active ? t("models.priceHistoryActive") : t("models.priceHistorySuperseded", { time: formatDateTime(item.supersededAt, i18n.language) })}</span>
                    </div>
                    <PriceDetails operation={operation} price={item} labelClassName="text-muted-foreground" />
                  </div>
                ))}
              </section>
            ) : null}
            {confirmDelete ? <p className="text-destructive">{t("models.priceDeleteHint")}</p> : null}
          </div>
          <DialogFooter className="shrink-0 gap-2 bg-muted/20 px-5 py-3.5 sm:justify-between sm:gap-0">
            <div className="flex gap-2">
              <Button type="button" variant="ghost" size="sm" aria-pressed={showHistory} onClick={() => setShowHistory((current) => !current)}><History />{t("models.priceHistory")}</Button>
              {customID ? (
                <Button type="button" variant={confirmDelete ? "destructive" : "ghost"} size="sm" className={cn(!confirmDelete && "text-destructive hover:text-destructive")} disabled={deleteMutation.isPending} onClick={() => confirmDelete ? deleteMutation.mutate(customID) : setConfirmDelete(true)}>
                  {deleteMutation.isPending ? <Spinner /> : <Trash2 />}{t(confirmDelete ? "models.priceDeleteConfirm" : "models.priceDelete")}
                </Button>
              ) : null}
            </div>
            <div className="flex gap-2">
              <Button type="button" variant="secondary" size="sm" onClick={onClose}>{t("common.cancel")}</Button>
              <Button type="submit" size="sm" disabled={saveMutation.isPending}>{saveMutation.isPending ? <Spinner /> : null}{t("common.save")}</Button>
            </div>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function PriceSourceBadge({ source }: { source: RoutePriceDTO["source"] }) {
  const { t } = useTranslation();
  const className = source === "custom" ? "bg-sky-500/10 text-sky-700 dark:text-sky-300" : source === "none" ? "text-amber-700 dark:text-amber-300" : undefined;
  return <Badge variant={source === "none" ? "outline" : "secondary"} className={className}>{t(`models.priceSources.${source}`)}</Badge>;
}

// PriceDetails lists a price's components, or why there are none to show.
export function PriceDetails({ operation, price, labelClassName }: { operation: PriceOperation; price: PriceComponents & { source?: RoutePriceDTO["source"] }; labelClassName: string }) {
  const { t } = useTranslation();
  const rows = priceComponents(operation).flatMap(({ component }) => {
    const value = price[component];
    return value === undefined ? [] : [{ component, value }];
  });
  if (rows.length === 0) return <p className={cn("leading-5", labelClassName)}>{t(price.source === "none" ? "models.priceUnpriced" : "models.priceOfficialVaries")}</p>;
  return (
    <div className="space-y-0.5">
      {rows.map(({ component, value }) => (
        <div key={component} className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3">
          <span className={labelClassName}>{t(`models.priceComponents.${component}`)}</span>
          <span className="break-all text-right font-mono tabular-nums">{formatPriceUSD(value)} {t(`models.priceUnits.${priceComponentUnit(component)}`)}</span>
        </div>
      ))}
    </div>
  );
}
