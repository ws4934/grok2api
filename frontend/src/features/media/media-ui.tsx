import { AlertCircle, RefreshCw } from "lucide-react";
import { createElement, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import type { ProviderDTO } from "@/entities/provider/provider-api";
import { providerIcon } from "@/entities/provider/provider-icon";
import { providerColor, providerLabel, providerShortLabel } from "@/entities/provider/provider-presentation";
import { mediaAssetPath } from "@/features/media/media-model";
import { cn } from "@/shared/lib/cn";

export function ProviderTag({ id, providers, short = false }: { id: string; providers?: ProviderDTO[]; short?: boolean }) {
  if (!id) return <span className="text-muted-foreground">-</span>;
  return (
    <span className="flex min-w-0 items-center gap-1" title={providerLabel(id, providers)}>
      {createElement(providerIcon(id), { className: "size-3 shrink-0", style: { color: providerColor(id, providers) } })}
      <span className="truncate">{short ? providerShortLabel(id, providers) : providerLabel(id, providers)}</span>
    </span>
  );
}

export function VideoPreview({ assetId }: { assetId: string }) {
  const { t } = useTranslation();
  const videoRef = useRef<HTMLVideoElement>(null);
  const [state, setState] = useState<"loading" | "ready" | "error">("loading");

  function retry(): void {
    setState("loading");
    videoRef.current?.load();
  }

  return (
    <div className="relative flex min-h-56 w-full items-center justify-center overflow-hidden rounded-lg bg-black sm:min-h-80">
      <video
        ref={videoRef}
        className={cn("h-auto max-h-[60vh] w-auto max-w-full object-contain", state === "error" && "invisible")}
        src={mediaAssetPath("video", assetId)}
        controls
        playsInline
        preload="auto"
        onLoadStart={() => setState("loading")}
        onLoadedMetadata={(event) => showFirstVideoFrame(event.currentTarget)}
        onLoadedData={() => setState("ready")}
        onCanPlay={() => setState("ready")}
        onEnded={(event) => showFirstVideoFrame(event.currentTarget)}
        onError={() => setState("error")}
      />
      {state === "loading" ? (
        <div className="pointer-events-none absolute inset-0 flex items-center justify-center bg-black">
          <Spinner className="size-5 text-white" />
          <span className="sr-only">{t("common.loading")}</span>
        </div>
      ) : null}
      {state === "error" ? (
        <div className="absolute inset-0 flex flex-col items-center justify-center gap-3 bg-black px-6 text-center text-white">
          <AlertCircle className="size-6 text-red-400" />
          <p className="text-sm">{t("media.videoUnavailable")}</p>
          <Button type="button" variant="secondary" size="sm" onClick={retry}>
            <RefreshCw />
            {t("common.retry")}
          </Button>
        </div>
      ) : null}
    </div>
  );
}

function showFirstVideoFrame(video: HTMLVideoElement): void {
  if (!Number.isFinite(video.duration) || video.duration <= 0) return;
  video.currentTime = Math.min(0.01, video.duration / 2);
}
