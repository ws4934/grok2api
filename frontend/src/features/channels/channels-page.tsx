import { useState, type FormEvent, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { listClientKeys } from "@/features/client-keys/client-keys-api";
import { assignEgressAccounts, listAllEgressNodes, unassignEgressAccounts } from "@/features/settings/settings-api";
import { ImageWorkbench } from "./image-workbench";
import { channelsApi, type ChannelSettings, type ChannelView } from "./channels-api";
import "./channels-i18n";

const areaClass = "min-h-36 w-full rounded-md border bg-background p-3 font-mono text-sm";
const selectClass = "h-9 rounded-md border bg-background px-3 text-sm";
const tabs = ["accounts", "models", "access", "jobs", "settings"] as const;
const accountActions = [
  ["validate", "channels.accounts.validate"],
  ["refresh", "channels.accounts.refresh"],
  ["enable", "common.enable"],
  ["disable", "common.disable"],
  ["delete", "common.delete"],
] as const;
const settingsFields = ["output_token_limit", "vision_tokens", "account_concurrency", "image_workers"] as const;
const operations = ["chat", "responses", "image", "image_edit"] as const;

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="grid gap-2 text-sm">
      <span>{label}</span>
      {children}
    </label>
  );
}

function Panel({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="space-y-4 rounded-xl border bg-card p-5">
      <h2 className="font-semibold">{title}</h2>
      {children}
    </section>
  );
}

export function ChannelsPage() {
  const { t } = useTranslation();
  const query = useQuery({ queryKey: ["channels"], queryFn: channelsApi.list });
  const [selected, setSelected] = useState("");
  const active = query.data?.find((v) => v.descriptor.id === selected) ?? query.data?.[0];

  return (
    <div className="mx-auto max-w-7xl space-y-6 p-4 lg:p-8">
      <div>
        <h1 className="text-2xl font-semibold">{t("channels.title")}</h1>
        <p className="mt-2 text-sm text-muted-foreground">{t("channels.description")}</p>
      </div>
      {query.isPending && <p role="status">{t("channels.loading")}</p>}
      {query.error && <p role="alert">{query.error.message}</p>}
      <div className="flex flex-wrap gap-3">
        {query.data?.map((v) => (
          <Button
            key={v.descriptor.id}
            variant={active?.descriptor.id === v.descriptor.id ? "default" : "outline"}
            onClick={() => setSelected(v.descriptor.id)}
          >
            {v.descriptor.name} · {v.settings.enabled ? t("common.enabled") : t("channels.closed")}
          </Button>
        ))}
      </div>
      {active && <ChannelDetail key={active.descriptor.id} channel={active} />}
    </div>
  );
}

function ChannelDetail({ channel }: { channel: ChannelView }) {
  const { t } = useTranslation();
  const id = channel.descriptor.id;
  const client = useQueryClient();
  const [tab, setTab] = useState<(typeof tabs)[number]>("accounts");
  const [busy, setBusy] = useState(false);
  const run = async (action: () => Promise<unknown>, success = t("channels.done")) => {
    setBusy(true);
    try {
      await action();
      toast.success(success);
      await client.invalidateQueries({ queryKey: ["channels"] });
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t("channels.failed"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <div className="flex flex-wrap gap-2 border-b pb-3">
        {tabs.map((key) => (
          <Button key={key} variant={tab === key ? "secondary" : "ghost"} onClick={() => setTab(key)}>
            {t(`channels.tabs.${key}`)}
          </Button>
        ))}
      </div>
      {tab === "accounts" && <Accounts id={id} run={run} busy={busy} />}
      {tab === "models" && <Models id={id} run={run} busy={busy} />}
      {tab === "access" && <Access id={id} run={run} busy={busy} />}
      {tab === "jobs" && (
        <>
          <ImageWorkbench channel={id} />
          <Jobs id={id} run={run} busy={busy} />
        </>
      )}
      {tab === "settings" && (
        <Settings
          key={JSON.stringify(channel.settings)}
          id={id}
          initial={channel.settings}
          clearanceURL={channel.descriptor.clearance_url}
          run={run}
          busy={busy}
        />
      )}
    </>
  );
}

type Actions = { id: string; run: (fn: () => Promise<unknown>, success?: string) => Promise<void>; busy: boolean };

function Accounts({ id, run, busy }: Actions) {
  const { t } = useTranslation();
  const [offset, setOffset] = useState(0);
  const query = useQuery({ queryKey: ["channels", id, "accounts", offset], queryFn: () => channelsApi.accounts(id, offset) });
  const [selected, setSelected] = useState<number[]>([]);
  const [data, setData] = useState("");
  const [replace, setReplace] = useState(false);
  const [report, setReport] = useState("");
  const [session, setSession] = useState<{ id: string; authorize_url: string; expires_at: string } | null>(null);
  const [callback, setCallback] = useState("");
  const [editID, setEditID] = useState<number | null>(null);
  const [name, setName] = useState("");
  const [concurrency, setConcurrency] = useState(1);
  const [token, setToken] = useState("");
  const nodes = useQuery({ queryKey: ["egress-nodes", "channel", id], queryFn: () => listAllEgressNodes({ scope: id }) });
  const [nodeID, setNodeID] = useState("");
  const nodeName = (nodeId: number) => nodes.data?.items.find((node) => node.id === String(nodeId))?.name ?? `#${nodeId}`;

  const exportAccounts = (ids: number[]) => run(async () => {
    const result = await channelsApi.export(id, ids);
    const url = URL.createObjectURL(new Blob([JSON.stringify(result, null, 2)], { type: "application/json" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = `${id}-accounts.json`;
    a.click();
    URL.revokeObjectURL(url);
  }, t("channels.accounts.exported"));

  return (
    <div className="space-y-6">
      <Panel title={t("channels.accounts.title")}>
        <p className="text-sm text-muted-foreground">{t("channels.accounts.help")}</p>
        <div className="flex flex-wrap gap-2">
          {accountActions.map(([action, label]) => (
            <Button
              key={action}
              size="sm"
              variant="outline"
              disabled={busy || !selected.length}
              onClick={() => void run(async () => {
                const result = await channelsApi.actions(id, selected, action);
                setReport(JSON.stringify(result, null, 2));
                if (action === "delete") setSelected([]);
              })}
            >
              {t(label)}
            </Button>
          ))}
          <Button size="sm" variant="outline" disabled={busy || !selected.length} onClick={() => void exportAccounts(selected)}>
            {t("channels.accounts.exportSelected")}
          </Button>
          <Button size="sm" variant="outline" disabled={busy} onClick={() => void exportAccounts([])}>
            {t("channels.accounts.exportAll")}
          </Button>
          <select aria-label={t("channels.accounts.egress")} className={selectClass} value={nodeID} onChange={(event) => setNodeID(event.target.value)}>
            <option value="">{t("channels.accounts.chooseEgress")}</option>
            {nodes.data?.items.map((node) => <option key={node.id} value={node.id}>{node.name}</option>)}
          </select>
          <Button
            size="sm"
            variant="outline"
            disabled={busy || !selected.length || !nodeID}
            onClick={() => void run(async () => setReport(JSON.stringify(await assignEgressAccounts(nodeID, id, selected.map(String)), null, 2)))}
          >
            {t("channels.accounts.bindEgress")}
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={busy || !selected.length}
            onClick={() => void run(async () => setReport(JSON.stringify(await unassignEgressAccounts(id, selected.map(String)), null, 2)))}
          >
            {t("channels.accounts.unbindEgress")}
          </Button>
        </div>
        {query.error && <p role="alert">{query.error.message}</p>}
        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm">
            <thead>
              <tr className="border-b">
                <th className="p-2">{t("channels.accounts.select")}</th>
                <th>{t("channels.accounts.account")}</th>
                <th>{t("channels.accounts.status")}</th>
                <th>{t("channels.accounts.quota")}</th>
                <th>{t("channels.accounts.credentials")}</th>
                <th>{t("channels.accounts.egress")}</th>
                <th>{t("common.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {query.data?.items.map((a) => (
                <tr key={a.id} className="border-b">
                  <td className="p-2">
                    <input
                      aria-label={t("common.selectItem", { name: a.name })}
                      type="checkbox"
                      checked={selected.includes(a.id)}
                      onChange={(event) => setSelected(event.target.checked ? [...selected, a.id] : selected.filter((v) => v !== a.id))}
                    />
                  </td>
                  <td className="py-3">
                    {a.name}
                    <div className="text-xs text-muted-foreground">
                      {a.identity.email} · {a.identity.plan ?? t("channels.accounts.unknownPlan")}
                    </div>
                  </td>
                  <td>
                    {a.enabled ? t(`channels.status.${a.status}`, { defaultValue: a.status }) : t("common.disabled")}
                    <div className="max-w-64 text-xs text-destructive">{a.failure}</div>
                  </td>
                  <td>
                    {a.quotas?.map((q) => (
                      <div key={q.feature}>{q.feature}: {q.remaining ?? t("channels.accounts.unknown")}</div>
                    ))}
                  </td>
                  <td>
                    {a.refreshable ? t("channels.accounts.refreshable") : t("channels.accounts.notRefreshable")}
                    <div className="text-xs">{a.expires_at ? new Date(a.expires_at).toLocaleString() : t("channels.accounts.expiryUnknown")}</div>
                  </td>
                  <td>{a.egress_node_id ? nodeName(a.egress_node_id) : t("channels.accounts.noEgress")}</td>
                  <td>
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => {
                        setEditID(a.id);
                        setName(a.name);
                        setConcurrency(a.max_concurrent);
                        setToken("");
                      }}
                    >
                      {t("channels.accounts.edit")}
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div className="flex items-center gap-3 text-sm">
          <Button size="sm" variant="outline" disabled={!offset} onClick={() => setOffset(Math.max(0, offset - 50))}>
            {t("common.previousPage")}
          </Button>
          <span>{t("channels.accounts.total", { total: query.data?.total ?? 0 })}</span>
          <Button size="sm" variant="outline" disabled={offset + 50 >= (query.data?.total ?? 0)} onClick={() => setOffset(offset + 50)}>
            {t("common.nextPage")}
          </Button>
        </div>
        {editID !== null && (
          <form
            className="grid gap-3 rounded-lg border p-4"
            onSubmit={(event) => {
              event.preventDefault();
              void run(async () => {
                const credentials: object | undefined = token.trim() ? JSON.parse(token) : undefined;
                await channelsApi.update(id, editID, { name, max_concurrent: concurrency, ...(credentials ? { credentials } : {}) });
                setToken("");
                setEditID(null);
              });
            }}
          >
            <Field label={t("channels.accounts.name")}>
              <Input value={name} onChange={(event) => setName(event.target.value)} required />
            </Field>
            <Field label={t("channels.accounts.concurrency")}>
              <Input type="number" min={1} max={256} value={concurrency} onChange={(event) => setConcurrency(Number(event.target.value))} />
            </Field>
            <Field label={t("channels.accounts.replaceCredentials")}>
              <textarea className={areaClass} value={token} onChange={(event) => setToken(event.target.value)} autoComplete="off" spellCheck={false} />
            </Field>
            <div className="flex gap-2">
              <Button disabled={busy}>{t("common.save")}</Button>
              <Button
                type="button"
                variant="outline"
                onClick={() => {
                  setToken("");
                  setEditID(null);
                }}
              >
                {t("common.cancel")}
              </Button>
            </div>
          </form>
        )}
      </Panel>
      <div className="grid gap-6 xl:grid-cols-2">
        <Panel title={t("channels.accounts.importTitle")}>
          <Field label={t("channels.accounts.importData")}>
            <textarea
              className={areaClass}
              value={data}
              onChange={(event) => setData(event.target.value)}
              placeholder={'{"access_token":"…","refresh_token":"…","client_profile":"platform_pkce_v1"}'}
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <Input
            type="file"
            accept=".json,.jsonl,.txt"
            aria-label={t("channels.accounts.importFile")}
            onChange={(event) => {
              const file = event.target.files?.[0];
              if (file) void file.text().then(setData);
            }}
          />
          <label className="flex gap-2 text-sm">
            <input type="checkbox" checked={replace} onChange={(event) => setReplace(event.target.checked)} />
            {t("channels.accounts.replaceDuplicates")}
          </label>
          <Button
            disabled={busy || !data.trim()}
            onClick={() => void run(async () => {
              const result = await channelsApi.import(id, data, replace);
              setReport(JSON.stringify(result, null, 2));
              setData("");
            })}
          >
            {t("channels.accounts.import")}
          </Button>
        </Panel>
        <Panel title={t("channels.accounts.oauthTitle")}>
          <p className="text-sm text-muted-foreground">{t("channels.accounts.oauthHelp")}</p>
          <Button disabled={busy} variant="outline" onClick={() => void run(async () => setSession(await channelsApi.startOAuth(id)))}>
            {t("channels.accounts.oauthStart")}
          </Button>
          {session && (
            <>
              <a className="block break-all text-sm text-primary underline" href={session.authorize_url} target="_blank" rel="noreferrer">
                {t("channels.accounts.oauthOpen")}
              </a>
              <Field label={t("channels.accounts.oauthCallback")}>
                <Input type="password" autoComplete="off" value={callback} onChange={(event) => setCallback(event.target.value)} />
              </Field>
              <Button
                disabled={busy || !callback}
                onClick={() => void run(async () => {
                  await channelsApi.completeOAuth(id, session.id, callback);
                  setCallback("");
                  setSession(null);
                }, t("channels.accounts.oauthSaved"))}
              >
                {t("channels.accounts.oauthComplete")}
              </Button>
            </>
          )}
        </Panel>
      </div>
      {report && (
        <Panel title={t("channels.accounts.report")}>
          <pre className="max-h-64 overflow-auto text-xs">{report}</pre>
        </Panel>
      )}
    </div>
  );
}

function Settings({ id, initial, clearanceURL, run, busy }: Actions & { initial: ChannelSettings; clearanceURL?: string }) {
  const { t } = useTranslation();
  const [value, setValue] = useState(initial);
  const models = useQuery({ queryKey: ["channels", id, "models"], queryFn: () => channelsApi.models(id) });
  // Clients address channel routes without the "<channel>/" namespace, so store the unprefixed name.
  const prefix = `${id}/`.toLowerCase();
  const imageRoutes = models.data
    ?.filter((m) => m.Enabled && m.Capability === "image")
    .map((m) => (m.PublicID.toLowerCase().startsWith(prefix) ? m.PublicID.slice(prefix.length) : m.PublicID)) ?? [];
  if (value.default_image_model && !imageRoutes.includes(value.default_image_model)) imageRoutes.push(value.default_image_model);

  return (
    <Panel title={t("channels.settings.title")}>
      <form
        className="grid max-w-2xl gap-4"
        onSubmit={(event: FormEvent) => {
          event.preventDefault();
          void run(() => channelsApi.settings(id, value));
        }}
      >
        <label className="flex gap-2">
          <input type="checkbox" checked={value.enabled} onChange={(event) => setValue({ ...value, enabled: event.target.checked })} />
          {t("channels.settings.enabled")}
        </label>
        {clearanceURL && (
          <div className="grid gap-2 text-sm">
            <div className="flex items-center gap-2">
              <Switch
                id="channel-cloudflare-clearance"
                checked={value.cloudflare_clearance}
                onCheckedChange={(checked) => setValue({ ...value, cloudflare_clearance: checked })}
              />
              <Label htmlFor="channel-cloudflare-clearance">{t("channels.settings.clearance")}</Label>
            </div>
            <p className="text-muted-foreground">{t("channels.settings.clearanceHelp")}</p>
          </div>
        )}
        <Field label={t("channels.settings.imageModel")}>
          <Input value={value.image_conversation_model} onChange={(event) => setValue({ ...value, image_conversation_model: event.target.value })} />
        </Field>
        <Field label={t("channels.settings.defaultImageModel")}>
          <select className={selectClass} value={value.default_image_model} onChange={(event) => setValue({ ...value, default_image_model: event.target.value })}>
            <option value="">{t("channels.settings.noDefaultImageModel")}</option>
            {imageRoutes.map((name) => <option key={name} value={name}>{name}</option>)}
          </select>
        </Field>
        <p className="text-sm text-muted-foreground">{t("channels.settings.defaultImageModelHelp")}</p>
        {settingsFields.map((key) => (
          <Field key={key} label={t(`channels.settings.${key}`)}>
            <Input
              type="number"
              min={key === "vision_tokens" ? 0 : 1}
              value={value[key]}
              onChange={(event) => setValue({ ...value, [key]: Number(event.target.value) })}
            />
          </Field>
        ))}
        <p className="text-sm text-muted-foreground">{t("channels.settings.help")}</p>
        <Button disabled={busy}>{t("channels.settings.save")}</Button>
      </form>
    </Panel>
  );
}

function Models({ id, run, busy }: Actions) {
  const { t } = useTranslation();
  const prices = useQuery({ queryKey: ["channels", id, "prices"], queryFn: () => channelsApi.prices(id) });
  const models = useQuery({ queryKey: ["channels", id, "models"], queryFn: () => channelsApi.models(id) });
  const [priceQuality, setPriceQuality] = useState("");
  const [priceSize, setPriceSize] = useState("");
  const [model, setModel] = useState("");
  const [publicID, setPublicID] = useState("");
  const [operation, setOperation] = useState("chat");
  const [input, setInput] = useState("0");
  const [output, setOutput] = useState("0");
  const [image, setImage] = useState("0");
  const operationLabel = (raw: string) => t(`channels.operations.${raw}`, { defaultValue: raw });

  return (
    <div className="space-y-6">
      <Panel title={t("channels.models.title")}>
        <p className="text-sm text-muted-foreground">{t("channels.models.help")}</p>
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label={t("channels.models.upstream")}>
            <Input value={model} onChange={(event) => setModel(event.target.value)} required />
          </Field>
          <Field label={t("channels.models.operation")}>
            <select className={selectClass} value={operation} onChange={(event) => setOperation(event.target.value)}>
              {operations.map((v) => <option key={v} value={v}>{operationLabel(v)}</option>)}
            </select>
          </Field>
          <Field label={t("channels.models.input")}>
            <Input type="number" min="0" step="any" value={input} onChange={(event) => setInput(event.target.value)} />
          </Field>
          <Field label={t("channels.models.output")}>
            <Input type="number" min="0" step="any" value={output} onChange={(event) => setOutput(event.target.value)} />
          </Field>
          <Field label={t("channels.models.image")}>
            <Input type="number" min="0" step="any" value={image} onChange={(event) => setImage(event.target.value)} />
          </Field>
          <Field label={t("channels.models.quality")}>
            <Input value={priceQuality} onChange={(event) => setPriceQuality(event.target.value)} placeholder="high" />
          </Field>
          <Field label={t("channels.models.size")}>
            <Input value={priceSize} onChange={(event) => setPriceSize(event.target.value)} placeholder="1024x1024" />
          </Field>
          <Field label={t("channels.models.publicID")}>
            <Input value={publicID} onChange={(event) => setPublicID(event.target.value)} />
          </Field>
        </div>
        <div className="flex gap-2">
          <Button
            disabled={busy || !model}
            onClick={() => void run(() => channelsApi.savePrice(id, {
              model,
              quality: priceQuality,
              size: priceSize,
              operation: operation === "responses" ? "chat" : operation,
              input_per_million_ticks: Math.round(Number(input) * 1e10),
              output_per_million_ticks: Math.round(Number(output) * 1e10),
              image_ticks: Math.round(Number(image) * 1e10),
            }))}
          >
            {t("channels.models.savePrice")}
          </Button>
          <Button
            variant="outline"
            disabled={busy || !model || !publicID}
            onClick={() => void run(() => channelsApi.createModel(id, { public_id: publicID, upstream_model: model, operation, enabled: true }))}
          >
            {t("channels.models.createRoute")}
          </Button>
        </div>
      </Panel>
      <Panel title={t("channels.models.routes")}>
        {models.error && <p role="alert">{models.error.message}</p>}
        {models.data?.map((m) => (
          <div key={m.ID} className="flex items-center justify-between gap-3 border-b py-2 text-sm">
            <span>
              {m.PublicID} → {m.UpstreamModel} · {operationLabel(m.Capability)} · {m.Enabled ? t("channels.models.open") : t("channels.models.closed")}
            </span>
            <Button size="sm" variant="ghost" disabled={busy} onClick={() => void run(() => channelsApi.setModelEnabled(id, m.ID, !m.Enabled))}>
              {m.Enabled ? t("channels.models.closeRoute") : t("channels.models.openRoute")}
            </Button>
            <Button size="sm" variant="ghost" disabled={busy} onClick={() => void run(() => channelsApi.deleteModel(id, m.ID))}>
              {t("channels.models.removeRoute")}
            </Button>
          </div>
        ))}
      </Panel>
      <Panel title={t("channels.models.prices")}>
        {prices.data?.map((p) => (
          <div key={p.id} className="border-b py-2 text-sm">
            v{p.version} · {p.model} · {operationLabel(p.operation)} ·{" "}
            {t("channels.models.priceRow", {
              input: p.input_per_million_ticks / 1e10,
              output: p.output_per_million_ticks / 1e10,
              image: p.image_ticks / 1e10,
            })}
          </div>
        ))}
      </Panel>
    </div>
  );
}

function Access({ id, run, busy }: Actions) {
  const { t } = useTranslation();
  const query = useQuery({ queryKey: ["channels", id, "grants"], queryFn: () => channelsApi.grants(id) });
  // ponytail: one page at the backend MaxPageSize (2000); page through when a deployment has more keys.
  const keys = useQuery({ queryKey: ["client-keys", "channel-grants"], queryFn: () => listClientKeys({ page: 1, pageSize: 2000 }) });
  const [key, setKey] = useState("");
  const names = new Map(keys.data?.items.map((k) => [Number(k.id), k.name] as const));
  const available = keys.data?.items.filter((k) => !query.data?.includes(Number(k.id))) ?? [];
  const error = query.error ?? keys.error;

  return (
    <Panel title={t("channels.access.title")}>
      <p className="text-sm text-muted-foreground">{t("channels.access.help")}</p>
      {error && <p role="alert">{error.message}</p>}
      <form
        className="flex max-w-lg gap-3"
        onSubmit={(event) => {
          event.preventDefault();
          void run(async () => {
            await channelsApi.grant(id, Number(key), true);
            setKey("");
          });
        }}
      >
        <Label className="sr-only" htmlFor="channel-key-id">{t("channels.access.key")}</Label>
        <select id="channel-key-id" className={`${selectClass} min-w-0 flex-1`} value={key} onChange={(event) => setKey(event.target.value)} required>
          <option value="">{t("channels.access.choose")}</option>
          {available.map((k) => <option key={k.id} value={k.id}>{k.name} · #{k.id}</option>)}
        </select>
        <Button disabled={busy}>{t("channels.access.grant")}</Button>
      </form>
      {query.data?.map((value) => (
        <div key={value} className="flex items-center justify-between border-b py-2">
          <span>{names.has(value) ? `${names.get(value)} · #${value}` : t("channels.access.unnamed", { id: value })}</span>
          <Button variant="ghost" size="sm" disabled={busy} onClick={() => void run(() => channelsApi.grant(id, value, false))}>
            {t("channels.access.revoke")}
          </Button>
        </div>
      ))}
    </Panel>
  );
}

function Jobs({ id, run, busy }: Actions) {
  const { t } = useTranslation();
  const [offset, setOffset] = useState(0);
  const query = useQuery({
    queryKey: ["channels", id, "jobs", offset],
    queryFn: () => channelsApi.jobs(id, offset),
    refetchInterval: (q) => (q.state.data?.items.some((j) => ["queued", "running"].includes(j.status)) ? 3000 : false),
  });

  return (
    <Panel title={t("channels.jobs.title")}>
      {query.error && <p role="alert">{query.error.message}</p>}
      <p className="text-sm text-muted-foreground">{t("channels.jobs.help")}</p>
      {query.data?.items.map((j) => (
        <div key={j.id} className="space-y-2 rounded-lg border p-3">
          <div className="flex flex-wrap justify-between text-sm">
            <code>{j.id}</code>
            <span>
              {t("channels.jobs.progress", {
                status: t(`channels.status.${j.status}`, { defaultValue: j.status }),
                done: j.items.filter((i) => i.status === "succeeded").length,
                total: j.items.length,
              })}
            </span>
          </div>
          {j.items.map((i) => (
            <div key={i.id} className="text-sm text-muted-foreground">
              #{i.index + 1} · {t(`channels.status.${i.stage}`, { defaultValue: i.stage })} · {i.width ? `${i.width}×${i.height} ${i.mime_type}` : ""} · {i.error}
            </div>
          ))}
          {j.items.some((i) => i.status === "uncertain") && (
            <Button
              size="sm"
              variant="outline"
              disabled={busy}
              onClick={() => {
                if (window.confirm(t("channels.jobs.retryConfirm"))) {
                  void run(() => channelsApi.retryJob(id, j.id, crypto.randomUUID()), t("channels.jobs.retried"));
                }
              }}
            >
              {t("channels.jobs.retry")}
            </Button>
          )}
        </div>
      ))}
      {query.data?.items.length === 0 && <p>{t("channels.jobs.empty")}</p>}
      <div className="flex items-center gap-3 text-sm">
        <Button variant="outline" disabled={!offset} onClick={() => setOffset(Math.max(0, offset - 50))}>
          {t("common.previousPage")}
        </Button>
        <span>{t("channels.jobs.total", { total: query.data?.total ?? 0 })}</span>
        <Button variant="outline" disabled={offset + 50 >= (query.data?.total ?? 0)} onClick={() => setOffset(offset + 50)}>
          {t("common.nextPage")}
        </Button>
      </div>
    </Panel>
  );
}
