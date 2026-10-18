import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { z } from "zod";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { channelsApi, jobSchema } from "./channels-api";

const selectClass = "h-9 rounded-md border bg-background px-3 text-sm";
async function publicRequest(key: string, path: string, body?: object, idempotency?: string): Promise<unknown> {
 const response = await fetch(`/v1/images/jobs${path}`, { method: body ? "POST" : "GET", headers: { Authorization: `Bearer ${key}`, ...(body ? { "Content-Type": "application/json" } : {}), ...(idempotency ? { "Idempotency-Key": idempotency } : {}) }, ...(body ? { body: JSON.stringify(body) } : {}) });
 const result: unknown = await response.json();
 if (!response.ok) { const error = z.object({ error: z.object({ message: z.string() }) }).safeParse(result); throw new Error(error.success ? error.data.error.message : `HTTP ${response.status}`); }
 return result;
}
async function readImage(file: File): Promise<{ name: string; data: string }> {
 if (!file.type.startsWith("image/") || file.size > 20 * 1024 * 1024) throw new Error("请选择小于 20 MiB 的图片");
 return new Promise((resolve, reject) => { const reader = new FileReader(); reader.onload = () => resolve({ name: file.name, data: String(reader.result) }); reader.onerror = () => reject(new Error("读取图片失败")); reader.readAsDataURL(file); });
}
export function ImageWorkbench({ channel }: { channel: string }) {
 const models = useQuery({ queryKey: ["channels", channel, "models"], queryFn: () => channelsApi.models(channel) });
 const [key, setKey] = useState("");
 const [operation, setOperation] = useState("image");
 const [model, setModel] = useState("");
 const [prompt, setPrompt] = useState("");
 const [count, setCount] = useState(1);
 const [size, setSize] = useState("auto");
 const [quality, setQuality] = useState("auto");
 const [format, setFormat] = useState("png");
 const [background, setBackground] = useState("auto");
 const [inputs, setInputs] = useState<Array<{ name: string; data: string }>>([]);
 const [mask, setMask] = useState<{ name: string; data: string } | null>(null);
 const [inputError, setInputError] = useState("");
 const [active, setActive] = useState<{ id: string; key: string } | null>(null);
 const options = models.data?.filter(m => m.Enabled && m.Capability === operation) ?? [];
 const selected = options.some(m => m.PublicID === model) ? model : options[0]?.PublicID ?? "";
 const submit = useMutation({ mutationFn: async () => {
  if (mask && inputs.length !== 1) throw new Error("蒙版编辑需要恰好一张源图");
  if (background === "transparent" && format === "jpeg") throw new Error("JPEG 不支持透明背景");
  return jobSchema.parse(await publicRequest(key, "", { operation, model: selected, prompt, n: count, size, quality, output_format: format, background, ...(operation === "image_edit" ? { images: inputs.map(v => v.data), ...(mask ? { mask: mask.data } : {}) } : {}) }, crypto.randomUUID()));
 }, onSuccess: job => setActive({ id: job.id, key }) });
 const job = useQuery({ queryKey: ["channel-workbench-job", active?.id], queryFn: async () => jobSchema.parse(await publicRequest(active!.key, `/${active!.id}`)), enabled: !!active, refetchInterval: q => !q.state.data || ["queued", "running"].includes(q.state.data.status) ? 2000 : false });
 const results = useQuery({ queryKey: ["channel-workbench-result", active?.id], queryFn: async () => z.object({ data: z.array(z.object({ url: z.string() })) }).parse(await publicRequest(active!.key, `/${active!.id}/result`)), enabled: !!active && ["succeeded", "partially_succeeded"].includes(job.data?.status ?? "") });
 const cancel = useMutation({ mutationFn: () => publicRequest(active!.key, `/${active!.id}/cancel`, {}), onSuccess: () => job.refetch() });
 const error = inputError || submit.error?.message || job.error?.message || results.error?.message || cancel.error?.message;
 return <section className="space-y-4 rounded-xl border bg-card p-5"><h2 className="font-semibold">图片生成与编辑</h2><p className="text-sm text-muted-foreground">使用已授权客户端密钥创建持久任务。尺寸是上游请求提示，实际结果可能不同。密钥只保存在当前页面内存。</p>
 <form className="space-y-4" onSubmit={e => { e.preventDefault(); submit.mutate(); }}><div className="grid gap-3 sm:grid-cols-2"><label className="grid gap-2 text-sm">客户端 API Key<Input type="password" autoComplete="off" value={key} onChange={e => setKey(e.target.value)} required /></label><label className="grid gap-2 text-sm">操作<select className={selectClass} value={operation} onChange={e => setOperation(e.target.value)}><option value="image">文生图</option><option value="image_edit">图片编辑</option></select></label><label className="grid gap-2 text-sm">模型<select className={selectClass} value={selected} onChange={e => setModel(e.target.value)}>{options.map(m => <option key={m.ID} value={m.PublicID}>{m.PublicID}</option>)}</select></label><label className="grid gap-2 text-sm">张数<Input type="number" min={1} max={10} value={count} onChange={e => setCount(Number(e.target.value))} /></label></div>
 <label className="grid gap-2 text-sm">提示词<textarea className="min-h-28 rounded-md border bg-background p-3" value={prompt} onChange={e => setPrompt(e.target.value)} required /></label>
 <div className="grid gap-3 sm:grid-cols-4"><label className="grid gap-2 text-sm">尺寸<select className={selectClass} value={size} onChange={e => setSize(e.target.value)}>{["auto", "1024x1024", "1536x1024", "1024x1536"].map(v => <option key={v}>{v}</option>)}</select></label><label className="grid gap-2 text-sm">质量<select className={selectClass} value={quality} onChange={e => setQuality(e.target.value)}>{["auto", "low", "medium", "high"].map(v => <option key={v}>{v}</option>)}</select></label><label className="grid gap-2 text-sm">输出格式<select className={selectClass} value={format} onChange={e => setFormat(e.target.value)}>{["png", "jpeg", "webp"].map(v => <option key={v}>{v}</option>)}</select></label><label className="grid gap-2 text-sm">背景<select className={selectClass} value={background} onChange={e => setBackground(e.target.value)}><option value="auto">自动</option><option value="transparent">透明</option><option value="opaque">不透明</option></select></label></div>
 {operation === "image_edit" && <div className="grid gap-4 sm:grid-cols-2"><label className="grid gap-2 text-sm">参考图（最多 8 张）<Input type="file" accept="image/*" multiple onChange={e => { const files = Array.from(e.target.files ?? []); setInputError(""); if (files.length > 8 || files.reduce((n, f) => n + f.size, 0) > 20 * 1024 * 1024) { setInputError("参考图最多 8 张，总大小不超过 20 MiB"); return; } void Promise.all(files.map(readImage)).then(setInputs).catch(e => setInputError(String(e))); }} /><div className="flex flex-wrap gap-2">{inputs.map((v, i) => <img key={i} src={v.data} alt={v.name} className="h-24 w-24 rounded border object-contain" />)}</div></label><label className="grid gap-2 text-sm">PNG 蒙版（透明区域表示需要编辑）<Input type="file" accept="image/png" onChange={e => { const file = e.target.files?.[0]; if (!file) { setMask(null); return; } void readImage(file).then(setMask).catch(e => setInputError(String(e))); }} />{mask && <><img src={mask.data} alt="编辑蒙版" className="h-24 w-24 rounded border object-contain" /><Button type="button" variant="ghost" onClick={() => setMask(null)}>移除蒙版</Button></>}</label></div>}
 <Button disabled={submit.isPending || !key || !selected || (operation === "image_edit" && !inputs.length)}>创建图片任务</Button></form>
 {error && <p role="alert" className="text-sm text-destructive">{error}</p>}{active && <div className="space-y-2 rounded-md border p-3 text-sm"><code>{active.id}</code><p>{job.data?.status ?? "正在读取…"}</p>{job.data?.items.map(item => <p key={item.id}>#{item.index + 1} · {item.stage} {item.error}</p>)}{["queued", "running"].includes(job.data?.status ?? "") && <Button variant="outline" size="sm" disabled={cancel.isPending} onClick={() => cancel.mutate()}>取消未提交任务</Button>}</div>}
 {results.data && <div className="grid gap-3 sm:grid-cols-2">{results.data.data.map((result, i) => <a key={result.url} href={result.url} target="_blank" rel="noreferrer"><img src={result.url} alt={`图片结果 ${i+1}`} className="w-full rounded border object-contain" /></a>)}</div>}</section>;
}
