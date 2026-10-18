import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { z } from "zod";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { i18n } from "@/shared/i18n";
import { channelsApi } from "./channels-api";
import "./channels-i18n";

const selectClass = "h-9 rounded-md border bg-background px-3 text-sm";
const responseSchema = z.object({
  id: z.string(),
  status: z.string(),
  error: z.object({ message: z.string() }).nullable(),
  output: z.array(z.object({
    id: z.string(),
    type: z.string(),
    status: z.string(),
    result: z.string().nullable(),
    output_format: z.string().optional(),
  })),
});

async function publicRequest(key: string, path: string, init: { method?: string; body?: object; idempotency?: string } = {}) {
  const response = await fetch(`/v1/responses${path}`, {
    method: init.method ?? "GET",
    headers: {
      Authorization: `Bearer ${key}`,
      ...(init.body ? { "Content-Type": "application/json" } : {}),
      ...(init.idempotency ? { "Idempotency-Key": init.idempotency } : {}),
    },
    ...(init.body ? { body: JSON.stringify(init.body) } : {}),
  });
  const result: unknown = await response.json();
  if (!response.ok) {
    const error = z.object({ error: z.object({ message: z.string() }) }).safeParse(result);
    throw new Error(error.success ? error.data.error.message : `HTTP ${response.status}`);
  }
  return responseSchema.parse(result);
}

async function readImage(file: File): Promise<{ name: string; data: string }> {
  if (!file.type.startsWith("image/") || file.size > 20 * 1024 * 1024) throw new Error(i18n.t("channels.workbench.imageTooLarge"));
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve({ name: file.name, data: String(reader.result) });
    reader.onerror = () => reject(new Error(i18n.t("channels.workbench.readFailed")));
    reader.readAsDataURL(file);
  });
}

export function ImageWorkbench({ channel }: { channel: string }) {
  const { t } = useTranslation();
  const models = useQuery({ queryKey: ["channels", channel, "models"], queryFn: () => channelsApi.models(channel) });
  const [key, setKey] = useState("");
  const [operation, setOperation] = useState("image");
  const [model, setModel] = useState("");
  const [prompt, setPrompt] = useState("");
  const [size, setSize] = useState("auto");
  const [quality, setQuality] = useState("auto");
  const [format, setFormat] = useState("png");
  const [background, setBackground] = useState("auto");
  const [inputs, setInputs] = useState<Array<{ name: string; data: string }>>([]);
  const [mask, setMask] = useState<{ name: string; data: string } | null>(null);
  const [inputError, setInputError] = useState("");
  const [active, setActive] = useState<{ id: string; key: string } | null>(null);
  const options = models.data?.filter((m) => m.Enabled && m.Capability === operation) ?? [];
  const selected = options.some((m) => m.PublicID === model) ? model : options[0]?.PublicID ?? "";
  const statusLabel = (raw: string) => t(`channels.status.${raw}`, { defaultValue: raw });

  const submit = useMutation({
    mutationFn: async () => {
      const edit = operation === "image_edit";
      if (edit && mask && inputs.length !== 1) throw new Error(t("channels.workbench.maskNeedsOneImage"));
      if (background === "transparent" && format === "jpeg") throw new Error(t("channels.workbench.jpegNoTransparency"));
      const input = edit
        ? [{ role: "user", content: [{ type: "input_text", text: prompt }, ...inputs.map((v) => ({ type: "input_image", image_url: v.data }))] }]
        : prompt;
      const tool = {
        type: "image_generation",
        size,
        quality,
        background,
        output_format: format,
        ...(edit && mask ? { input_image_mask: { image_url: mask.data } } : {}),
      };
      return publicRequest(key, "", { method: "POST", body: { model: selected, background: true, input, tools: [tool] }, idempotency: crypto.randomUUID() });
    },
    onSuccess: (response) => setActive({ id: response.id, key }),
  });
  const job = useQuery({
    queryKey: ["channel-workbench-response", active?.id],
    queryFn: () => publicRequest(active!.key, `/${active!.id}`),
    enabled: !!active,
    refetchInterval: (q) => (!q.state.data || ["queued", "in_progress"].includes(q.state.data.status) ? 3000 : false),
  });
  const cancel = useMutation({
    mutationFn: () => publicRequest(active!.key, `/${active!.id}/cancel`, { method: "POST" }),
    onSuccess: () => job.refetch(),
  });
  const images = job.data?.output.filter((item) => item.result) ?? [];
  const error = inputError || submit.error?.message || job.error?.message || job.data?.error?.message || cancel.error?.message;

  return (
    <section className="space-y-4 rounded-xl border bg-card p-5">
      <h2 className="font-semibold">{t("channels.workbench.title")}</h2>
      <p className="text-sm text-muted-foreground">{t("channels.workbench.help")}</p>
      <form
        className="space-y-4"
        onSubmit={(event) => {
          event.preventDefault();
          submit.mutate();
        }}
      >
        <div className="grid gap-3 sm:grid-cols-3">
          <label className="grid gap-2 text-sm">
            {t("channels.workbench.apiKey")}
            <Input type="password" autoComplete="off" value={key} onChange={(event) => setKey(event.target.value)} required />
          </label>
          <label className="grid gap-2 text-sm">
            {t("channels.workbench.operation")}
            <select className={selectClass} value={operation} onChange={(event) => setOperation(event.target.value)}>
              <option value="image">{t("channels.workbench.textToImage")}</option>
              <option value="image_edit">{t("channels.workbench.imageEdit")}</option>
            </select>
          </label>
          <label className="grid gap-2 text-sm">
            {t("channels.workbench.model")}
            <select className={selectClass} value={selected} onChange={(event) => setModel(event.target.value)}>
              {options.map((m) => <option key={m.ID} value={m.PublicID}>{m.PublicID}</option>)}
            </select>
          </label>
        </div>
        <label className="grid gap-2 text-sm">
          {t("channels.workbench.prompt")}
          <textarea className="min-h-28 rounded-md border bg-background p-3" value={prompt} onChange={(event) => setPrompt(event.target.value)} required />
        </label>
        <div className="grid gap-3 sm:grid-cols-4">
          <label className="grid gap-2 text-sm">
            {t("channels.workbench.size")}
            <select className={selectClass} value={size} onChange={(event) => setSize(event.target.value)}>
              {["auto", "1024x1024", "1536x1024", "1024x1536"].map((v) => <option key={v}>{v}</option>)}
            </select>
          </label>
          <label className="grid gap-2 text-sm">
            {t("channels.workbench.quality")}
            <select className={selectClass} value={quality} onChange={(event) => setQuality(event.target.value)}>
              {["auto", "low", "medium", "high"].map((v) => <option key={v}>{v}</option>)}
            </select>
          </label>
          <label className="grid gap-2 text-sm">
            {t("channels.workbench.format")}
            <select className={selectClass} value={format} onChange={(event) => setFormat(event.target.value)}>
              {["png", "jpeg", "webp"].map((v) => <option key={v}>{v}</option>)}
            </select>
          </label>
          <label className="grid gap-2 text-sm">
            {t("channels.workbench.background")}
            <select className={selectClass} value={background} onChange={(event) => setBackground(event.target.value)}>
              <option value="auto">{t("channels.workbench.backgroundAuto")}</option>
              <option value="transparent">{t("channels.workbench.backgroundTransparent")}</option>
              <option value="opaque">{t("channels.workbench.backgroundOpaque")}</option>
            </select>
          </label>
        </div>
        {operation === "image_edit" && (
          <div className="grid gap-4 sm:grid-cols-2">
            <label className="grid gap-2 text-sm">
              {t("channels.workbench.references")}
              <Input
                type="file"
                accept="image/*"
                multiple
                onChange={(event) => {
                  const files = Array.from(event.target.files ?? []);
                  setInputError("");
                  if (files.length > 8 || files.reduce((n, f) => n + f.size, 0) > 20 * 1024 * 1024) {
                    setInputError(t("channels.workbench.referencesLimit"));
                    return;
                  }
                  void Promise.all(files.map(readImage)).then(setInputs).catch((e) => setInputError(String(e)));
                }}
              />
              <div className="flex flex-wrap gap-2">
                {inputs.map((v, i) => <img key={i} src={v.data} alt={v.name} className="h-24 w-24 rounded border object-contain" />)}
              </div>
            </label>
            <label className="grid gap-2 text-sm">
              {t("channels.workbench.mask")}
              <Input
                type="file"
                accept="image/png"
                onChange={(event) => {
                  const file = event.target.files?.[0];
                  if (!file) {
                    setMask(null);
                    return;
                  }
                  void readImage(file).then(setMask).catch((e) => setInputError(String(e)));
                }}
              />
              {mask && (
                <>
                  <img src={mask.data} alt={t("channels.workbench.maskPreview")} className="h-24 w-24 rounded border object-contain" />
                  <Button type="button" variant="ghost" onClick={() => setMask(null)}>{t("channels.workbench.removeMask")}</Button>
                </>
              )}
            </label>
          </div>
        )}
        <Button disabled={submit.isPending || !key || !selected || (operation === "image_edit" && !inputs.length)}>
          {t("channels.workbench.submit")}
        </Button>
      </form>
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      {active && (
        <div className="space-y-2 rounded-md border p-3 text-sm">
          <code>{active.id}</code>
          <p>{job.data ? statusLabel(job.data.status) : t("channels.workbench.loading")}</p>
          {job.data?.output.map((item, i) => (
            <p key={item.id}>#{i + 1} · {statusLabel(item.status)}</p>
          ))}
          {["queued", "in_progress"].includes(job.data?.status ?? "") && (
            <Button variant="outline" size="sm" disabled={cancel.isPending} onClick={() => cancel.mutate()}>
              {t("channels.workbench.cancel")}
            </Button>
          )}
        </div>
      )}
      {images.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2">
          {images.map((item, i) => {
            const extension = item.output_format ?? "png";
            const src = `data:image/${extension};base64,${item.result}`;
            return (
              <a key={item.id} href={src} download={`${item.id}.${extension}`}>
                <img src={src} alt={t("channels.workbench.result", { index: i + 1 })} className="w-full rounded border object-contain" />
              </a>
            );
          })}
        </div>
      )}
    </section>
  );
}
