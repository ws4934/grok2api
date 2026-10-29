import { lazy, Suspense, type ComponentType, type LazyExoticComponent } from "react";

import { Spinner } from "@/components/ui/spinner";

// Keyed by the first path segment, so preloadRoute can find a page by URL.
const pageLoaders = {
  "system-logs": () => import("@/features/system-logs/system-logs-page"),
  "accounts": () => import("@/features/accounts/accounts-page"),
  "request-audits": () => import("@/features/audits/request-audits-page"),
  "client-keys": () => import("@/features/client-keys/client-keys-page"),
  "creative-console": () => import("@/features/creative-console/creative-console-page"),
  "dashboard": () => import("@/features/dashboard/dashboard-page"),
  "docs": () => import("@/features/docs/api-docs-page"),
  "gallery": () => import("@/features/media/gallery-page"),
  "media-jobs": () => import("@/features/media/media-jobs-page"),
  "models": () => import("@/features/models/models-page"),
  "quality-guard": () => import("@/features/quality-guard/quality-guard-page"),
  "settings": () => import("@/features/settings/settings-page"),
};
const loadAppShell = () => import("@/app/app-shell");

const SystemLogsPage = lazyNamed(pageLoaders["system-logs"], "SystemLogsPage");
const AccountsPage = lazyNamed(pageLoaders["accounts"], "AccountsPage");
const AppShell = lazyNamed(loadAppShell, "AppShell");
const RequestAuditsPage = lazyNamed(pageLoaders["request-audits"], "RequestAuditsPage");
const ClientKeysPage = lazyNamed(pageLoaders["client-keys"], "ClientKeysPage");
const CreativeConsolePage = lazyNamed(pageLoaders["creative-console"], "CreativeConsolePage");
const DashboardPage = lazyNamed(pageLoaders["dashboard"], "DashboardPage");
const ApiDocsPage = lazyNamed(pageLoaders["docs"], "ApiDocsPage");
const GalleryPage = lazyNamed(pageLoaders["gallery"], "GalleryPage");
const MediaJobsPage = lazyNamed(pageLoaders["media-jobs"], "MediaJobsPage");
const ModelsPage = lazyNamed(pageLoaders["models"], "ModelsPage");
const QualityGuardPage = lazyNamed(pageLoaders["quality-guard"], "QualityGuardPage");
const SettingsPage = lazyNamed(pageLoaders["settings"], "SettingsPage");

// preloadRoute starts downloading the app shell and the page for pathname
// while the session is still being restored, instead of after it.
export function preloadRoute(pathname: string) {
  const segment = pathname.split("/")[1] || "dashboard";
  void loadAppShell().catch(() => undefined);
  if (Object.hasOwn(pageLoaders, segment)) {
    void pageLoaders[segment as keyof typeof pageLoaders]().catch(() => undefined);
  }
}

function lazyNamed<T extends Record<K, ComponentType>, K extends keyof T>(loader: () => Promise<T>, exportName: K): LazyExoticComponent<T[K]> {
  return lazy(async () => ({ default: (await loader())[exportName] }));
}

function DeferredPage({ page: Page }: { page: ComponentType }) {
  return <Suspense fallback={<PageLoadingFallback />}><Page /></Suspense>;
}

export function DeferredAccountsPage() {
  return <DeferredPage page={AccountsPage} />;
}

export function DeferredAppShell() {
  return <Suspense fallback={<PageLoadingFallback fullScreen />}><AppShell /></Suspense>;
}

export function DeferredDashboardPage() {
  return <DeferredPage page={DashboardPage} />;
}

export function DeferredModelsPage() {
  return <DeferredPage page={ModelsPage} />;
}

export function DeferredQualityGuardPage() {
  return <DeferredPage page={QualityGuardPage} />;
}

export function DeferredClientKeysPage() {
  return <DeferredPage page={ClientKeysPage} />;
}

export function DeferredCreativeConsolePage() {
  return <DeferredPage page={CreativeConsolePage} />;
}

export function DeferredRequestAuditsPage() {
  return <DeferredPage page={RequestAuditsPage} />;
}

export function DeferredGalleryPage() {
  return <DeferredPage page={GalleryPage} />;
}

export function DeferredMediaJobsPage() {
  return <DeferredPage page={MediaJobsPage} />;
}

export function DeferredApiDocsPage() {
  return <DeferredPage page={ApiDocsPage} />;
}

export function DeferredSettingsPage() {
  return <DeferredPage page={SettingsPage} />;
}

function PageLoadingFallback({ fullScreen = false }: { fullScreen?: boolean }) {
  return (
    <div className={fullScreen ? "flex min-h-screen items-center justify-center bg-background" : "flex min-h-[calc(100vh-7rem)] items-center justify-center lg:min-h-[calc(100vh-10rem)]"}>
      <Spinner className="size-5" />
    </div>
  );
}

export function DeferredSystemLogsPage() { return <DeferredPage page={SystemLogsPage} />; }
