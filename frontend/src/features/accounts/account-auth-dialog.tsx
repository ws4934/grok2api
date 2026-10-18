import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, RefreshCw } from "lucide-react";
import { useEffect, useEffectEvent, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { authPollDelayMs, authSessionRemainingMs } from "@/features/accounts/account-ui";
import { completeAccountAuthorization, startAccountAuthorization, type AccountAuthResultDTO } from "@/features/accounts/accounts-api";
import { ApiError } from "@/shared/api/client";
import { CopyButton } from "@/shared/components/copy-button";
import { LoadingState } from "@/shared/components/data-state";
import { formatDateTime } from "@/shared/lib/format";

type Props = {
  provider: string;
  providerName: string;
  // Provider guidance for the callback URL step.
  help?: ReactNode;
  onClose: () => void;
};

// Interactive account login. The session kind from auth/start picks the flow: "device" shows a
// user code and polls complete; "redirect" (PKCE) opens the authorize URL and completes with the
// callback URL the admin pastes back.
export function AccountAuthDialog({ provider, providerName, help, onClose }: Props) {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const startedRef = useRef(false);
  const [expired, setExpired] = useState(false);
  const [failed, setFailed] = useState(false);
  const [callbackUrl, setCallbackUrl] = useState("");

  const start = useMutation({
    mutationFn: () => startAccountAuthorization(provider),
    onMutate: () => {
      setExpired(false);
      setFailed(false);
      setCallbackUrl("");
    },
    onError: (error) => showError(error),
  });
  const session = start.data;
  const { mutate: startSession } = start;

  function finish(result: AccountAuthResultDTO): void {
    void queryClient.invalidateQueries({ queryKey: ["accounts"] });
    if ((result.syncFailed ?? 0) > 0 || result.account?.modelSyncFailed) toast.warning(t("accounts.createdWithSyncFailure"));
    else toast.success(t("accounts.created"));
    onClose();
  }

  function showError(error: unknown): void {
    toast.error(error instanceof Error ? error.message : t("errors.generic"));
  }

  const deviceCompleted = useEffectEvent(finish);
  // 410: the session expired, was denied, or belongs to another admin; start over.
  const deviceFailed = useEffectEvent((error: unknown) => {
    if (error instanceof ApiError && error.status === 410) setExpired(true);
    else setFailed(true);
    showError(error);
  });

  const complete = useMutation({
    mutationFn: (sessionId: string) => completeAccountAuthorization(provider, sessionId, callbackUrl.trim()),
    onSuccess: (result) => { if (result.status === "completed") finish(result); },
    onError: (error) => { if (error instanceof ApiError && error.status === 410) setExpired(true); },
  });

  // Start once on open; the ref keeps StrictMode's effect replay from opening a second session.
  useEffect(() => {
    if (startedRef.current) return;
    startedRef.current = true;
    startSession();
  }, [startSession]);

  useEffect(() => {
    const remaining = session ? authSessionRemainingMs(session.expiresAt) : undefined;
    if (remaining === undefined) return;
    const timer = window.setTimeout(() => setExpired(true), remaining);
    return () => window.clearTimeout(timer);
  }, [session]);

  useEffect(() => {
    if (!session || session.kind !== "device" || expired || failed) return;
    const controller = new AbortController();
    let timeout = 0;
    const poll = async () => {
      try {
        const result = await completeAccountAuthorization(provider, session.sessionId, undefined, controller.signal);
        if (result.status === "completed") {
          deviceCompleted(result);
          return;
        }
        timeout = window.setTimeout(poll, authPollDelayMs(session.intervalSeconds));
      } catch (error) {
        if (controller.signal.aborted) return;
        if (error instanceof ApiError && error.status === 429) {
          timeout = window.setTimeout(poll, authPollDelayMs(session.intervalSeconds, true));
          return;
        }
        deviceFailed(error);
      }
    };
    timeout = window.setTimeout(poll, authPollDelayMs(session.intervalSeconds));
    return () => {
      controller.abort();
      window.clearTimeout(timeout);
    };
  }, [provider, session, expired, failed]);

  const expiresAt = session ? t("accounts.expiresAt", { time: formatDateTime(session.expiresAt, i18n.language) }) : "";
  const retry = (message: string) => (
    <div className="flex items-center justify-between gap-3">
      <p className="text-xs text-muted-foreground">{message}</p>
      <Button type="button" variant="secondary" size="sm" className="shrink-0" onClick={() => startSession()}><RefreshCw />{t("common.retry")}</Button>
    </div>
  );

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose(); }}>
      <DialogContent className="max-w-[460px] pb-6">
        <DialogHeader className="pr-7">
          <DialogTitle>{t("accounts.authTitle", { provider: providerName })}</DialogTitle>
          <DialogDescription>{t(session?.kind === "redirect" ? "accounts.redirectDescription" : "accounts.deviceDescription")}</DialogDescription>
        </DialogHeader>
        {start.isPending ? <LoadingState className="min-h-28" /> : null}
        {start.isError ? retry(start.error.message) : null}
        {session && expired ? retry(t("accounts.authExpired")) : null}
        {session?.kind === "device" && !expired ? (
          <div className="space-y-4">
            <div className="rounded-lg bg-muted/50 px-3 py-2.5">
              <span className="text-[11px] text-muted-foreground">{t("accounts.userCode")}</span>
              <div className="mt-0.5 flex items-center justify-between gap-3">
                <code className="min-w-0 select-all font-mono text-xl font-semibold tracking-[0.08em] tabular-nums">{session.userCode}</code>
                <CopyButton value={session.userCode ?? ""} className="-mr-1 size-7" onCopied={() => toast.success(t("common.copied"))} />
              </div>
              <p className="mt-2 text-[11px] leading-4 text-muted-foreground">{expiresAt}</p>
            </div>
            {failed ? retry(t("apiErrors.deviceLoginFailed")) : (
              <div className="flex min-h-10 items-center justify-between gap-4 pt-1" aria-live="polite">
                <span className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground"><Spinner className="size-3.5" />{t("accounts.waiting")}</span>
                <Button type="button" size="sm" className="shrink-0" onClick={() => window.open(session.verificationUriComplete || session.verificationUri, "_blank", "noopener,noreferrer")}>
                  <Link />{t("accounts.openVerification")}
                </Button>
              </div>
            )}
          </div>
        ) : null}
        {session?.kind === "redirect" && !expired ? (
          <form className="space-y-4" onSubmit={(event) => { event.preventDefault(); complete.mutate(session.sessionId); }}>
            <Button type="button" variant="secondary" size="sm" disabled={!session.authorizeUrl} onClick={() => window.open(session.authorizeUrl, "_blank", "noopener,noreferrer")}>
              <Link />{t("accounts.openAuthorization")}
            </Button>
            {help}
            <div className="space-y-2">
              <Label htmlFor="account-auth-callback">{t("accounts.callbackUrl")}</Label>
              <Input id="account-auth-callback" autoComplete="off" spellCheck={false} placeholder="https://" value={callbackUrl} onChange={(event) => setCallbackUrl(event.target.value)} />
              {complete.isError ? <p className="text-xs text-destructive">{complete.error.message}</p> : <p className="text-[11px] text-muted-foreground">{expiresAt}</p>}
            </div>
            <DialogFooter>
              <Button type="button" variant="secondary" size="sm" onClick={onClose}>{t("common.cancel")}</Button>
              <Button type="submit" size="sm" disabled={!callbackUrl.trim() || complete.isPending}>{complete.isPending ? <Spinner /> : null}{t("accounts.completeAuthorization")}</Button>
            </DialogFooter>
          </form>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}
