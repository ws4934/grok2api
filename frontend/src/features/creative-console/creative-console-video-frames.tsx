import { Film, Plus, Upload, X } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/shared/lib/cn";
import type { VideoFrameInput, VideoKeyframeInput } from "./creative-console-video";

export function VideoFrameControls({ lastFrame, keyframes, onLastFrameChange, onKeyframesChange, disabled }: {
  lastFrame?: VideoFrameInput;
  keyframes: VideoKeyframeInput[];
  onLastFrameChange: (value: VideoFrameInput | undefined) => void;
  onKeyframesChange: (value: VideoKeyframeInput[]) => void;
  disabled: boolean;
}) {
  const { t } = useTranslation();
  const count = Number(Boolean(lastFrame)) + keyframes.length;
  return <Popover>
    <PopoverTrigger asChild>
      <Button type="button" variant="ghost" size="sm" className={cn("h-8 gap-1.5 px-2 font-normal", count > 0 && "bg-secondary/70")} aria-label={t("creativeConsole.videoFrames")}>
        <Film />{t("creativeConsole.videoFrames")}{count > 0 ? ` ${count}` : ""}
      </Button>
    </PopoverTrigger>
    <PopoverContent align="start" className="w-[28rem] max-w-[calc(100vw-2rem)] p-4">
      <div className="mb-3 space-y-1">
        <h3 className="text-sm font-medium">{t("creativeConsole.videoFrames")}</h3>
        <p className="text-sm text-muted-foreground">{t("creativeConsole.videoFramesHint")}</p>
      </div>
      <div className="max-h-[55vh] space-y-4 overflow-y-auto">
        <section className="space-y-2">
          <div className="flex items-center justify-between gap-2 text-sm font-medium">
            <span>{t("creativeConsole.videoLastFrame")}</span>
            {lastFrame ? <Button type="button" variant="ghost" size="icon" className="size-7" disabled={disabled} aria-label={t("creativeConsole.videoRemoveLastFrame")} onClick={() => onLastFrameChange(undefined)}><X /></Button> : null}
          </div>
          <FrameImageInput value={lastFrame} onChange={onLastFrameChange} label={t("creativeConsole.videoLastFrame")} disabled={disabled} />
        </section>
        <section className="space-y-3 border-t pt-3">
          <div className="flex items-center justify-between text-sm font-medium"><span>{t("creativeConsole.videoKeyframes")}</span><span className="text-muted-foreground">{keyframes.length}/4</span></div>
          {keyframes.map((frame, index) => {
            const update = (patch: Partial<VideoKeyframeInput>) => onKeyframesChange(keyframes.map((item) => item.id === frame.id ? { ...item, ...patch } : item));
            const label = t("creativeConsole.videoKeyframe", { index: index + 1 });
            return <div key={frame.id} className="space-y-2 rounded-lg bg-secondary/40 p-3">
              <div className="flex items-center gap-2">
                <span className="min-w-0 flex-1 text-sm font-medium">{label}</span>
                <Input type="number" min="0" step="any" value={frame.timestamp} placeholder="0.33" className="h-8 w-24" aria-label={t("creativeConsole.videoKeyframeTime", { index: index + 1 })} disabled={disabled} onChange={(event) => update({ timestamp: event.target.value })} />
                <span className="text-sm text-muted-foreground">{t("creativeConsole.secondsShort")}</span>
                <Button type="button" variant="ghost" size="icon" className="size-7 shrink-0" disabled={disabled} aria-label={t("creativeConsole.videoRemoveKeyframe", { index: index + 1 })} onClick={() => onKeyframesChange(keyframes.filter((item) => item.id !== frame.id))}><X /></Button>
              </div>
              <FrameImageInput value={frame} label={label} disabled={disabled} onChange={(value) => update({ url: value?.url, file: value?.file, name: value?.name })} />
            </div>;
          })}
          <Button type="button" variant="secondary" size="sm" className="w-full" disabled={disabled || keyframes.length >= 4} onClick={() => onKeyframesChange([...keyframes, { id: crypto.randomUUID(), timestamp: "" }])}><Plus />{t("creativeConsole.videoAddKeyframe")}</Button>
        </section>
      </div>
    </PopoverContent>
  </Popover>;
}

function FrameImageInput({ value, onChange, label, disabled }: { value?: VideoFrameInput; onChange: (value: VideoFrameInput | undefined) => void; label: string; disabled: boolean }) {
  const { t } = useTranslation();
  const [error, setError] = useState("");
  return <div className="space-y-2">
    <Input value={value?.url ?? ""} placeholder="https://..." aria-label={`${label} URL`} disabled={disabled} onChange={(event) => { setError(""); onChange(event.target.value ? { url: event.target.value } : undefined); }} />
    <label className={cn("flex cursor-pointer items-center gap-2 rounded-md border px-3 py-2 text-sm hover:bg-accent", disabled && "pointer-events-none opacity-50")}>
      <Upload className="size-4 shrink-0" />
      <span className="min-w-0 flex-1 truncate">{value?.file?.name ?? value?.name ?? t("creativeConsole.uploadImage")}</span>
      <input type="file" accept="image/png,image/jpeg,image/webp,image/gif" className="sr-only" aria-label={`${label} ${t("creativeConsole.uploadImage")}`} disabled={disabled} onChange={(event) => {
        const file = event.target.files?.[0];
        event.target.value = "";
        if (!file) return;
        if (file.type && !file.type.startsWith("image/")) { setError(t("creativeConsole.errors.invalidImage")); return; }
        setError(""); onChange({ file, name: file.name });
      }} />
    </label>
    {value?.name && !value.file && !value.url ? <p className="text-sm text-destructive">{t("creativeConsole.videoFrameUploadAgain")}</p> : null}
    {error ? <p role="alert" className="text-sm text-destructive">{error}</p> : null}
  </div>;
}
