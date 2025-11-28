<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import { derived, get } from 'svelte/store';
  import { fly, fade } from 'svelte/transition';
  import { cubicOut } from 'svelte/easing';
  import AppIcon from '$lib/components/AppIcon.svelte';
  import Dock from '$lib/components/Dock.svelte';
  import Button from '$lib/components/ui/Button.svelte';
  import { appManifests, resolvedApps } from '$lib/stores/apps';
  import { LAYER_KIND_APP, LAYER_KIND_DRAWER, layerExists, layerStack, popLayer, pushLayer, resetStack, topLayer } from '$lib/stores/layers';
  import { createLayerLifecycle } from '$lib/stores/layerLifecycle';
  import { layerState as layerStateAction } from '$lib/actions/layerState';
  import { scrollLock } from '$lib/actions/scrollLock';
  import type { AppManifest, ResolvedApp } from '$lib/types/apps';
  import SettingsActivity from '$lib/components/settings/SettingsActivity.svelte';

  const seedApps: AppManifest[] = [
    { id: 'files', displayName: 'Files', origin: 'https://files.piccolo.local', isSystem: true, pinned: true },
    { id: 'settings', displayName: 'Settings', isSystem: true, pinned: true },
    { id: 'photos', displayName: 'Photos', origin: 'https://photos.piccolo.local', pinned: true },
    { id: 'plex', displayName: 'Plex', origin: 'https://plex.local' },
    { id: 'immich', displayName: 'Immich', origin: 'https://immich.local' },
    { id: 'nextcloud', displayName: 'Nextcloud', origin: 'https://nextcloud.local' }
  ];

  const pinnedIds = new Set(seedApps.filter((app) => app.pinned).map((app) => app.id));
  const runningIds = new Set(['plex', 'immich']);

  const pinnedApps = derived(resolvedApps, ($apps) => $apps.filter((app) => pinnedIds.has(app.id)));
  const runningApps = derived(resolvedApps, ($apps) => $apps.filter((app) => runningIds.has(app.id)));
  const drawerApps = resolvedApps;

  let drawerOpen = false;
  let activeApp: ResolvedApp | null = null;
  let activeAppLayerId: string | null = null;
  let headerEl: HTMLElement;
  let dockEl: HTMLElement;
  let resizeObserver: ResizeObserver | null = null;
  const unsubscribers: Array<() => void> = [];
  let reduceMotion = false;
  let historyLayerCount = 0;
  let suppressNextPop = false;
  let startY: number | null = null;

  const SAFE_MARGIN = 12;

  const clearAppLayers = (keepId?: string) => {
    const stack = get(layerStack);
    stack
      .filter((entry) => entry.kind === 'app' && entry.id !== keepId)
      .forEach((entry) => popLayer(entry.id));
  };

  const openDrawerFromHistory = () => {
    drawerOpen = true;
    const drawerEntry = {
      id: 'drawer',
      kind: LAYER_KIND_DRAWER,
      scrollLock: true,
      safeArea: true,
      onForeground: () => document.body.style.setProperty('overscroll-behavior', 'contain'),
      onBackground: () => document.body.style.removeProperty('overscroll-behavior')
    };
    if (!layerExists('drawer')) {
      pushLayer(drawerEntry);
    } else {
      popLayer('drawer');
      pushLayer(drawerEntry);
    }
  };

  const openAppFromHistory = (appId: string) => {
    const match = get(resolvedApps).find((a) => a.id === appId);
    if (!match) return;
    const layerId = `app-${match.id}`;
    const entry = {
      id: layerId,
      kind: LAYER_KIND_APP,
      scrollLock: true,
      safeArea: true,
      payload: { appId: match.id },
      onForeground: () => document.body.classList.add('app-foreground'),
      onBackground: () => document.body.classList.remove('app-foreground')
    };

    clearAppLayers(layerId);
    if (layerExists(layerId)) popLayer(layerId);
    pushLayer(entry);

    activeAppLayerId = layerId;
    activeApp = match;
  };

  const pushHistoryLayer = (id: string) => {
    if (typeof window === 'undefined') return;
    history.pushState({ piccoloLayer: id }, '', window.location.href);
    historyLayerCount += 1;
  };

  const popHistoryLayer = () => {
    if (typeof window === 'undefined') return;
    if (historyLayerCount > 0) {
      suppressNextPop = true;
      history.back();
      historyLayerCount -= 1;
    }
  };

  const setSafeAreas = () => {
    const top = (headerEl?.getBoundingClientRect().height || 0) + SAFE_MARGIN;
    const bottom = (dockEl?.getBoundingClientRect().height || 0) + SAFE_MARGIN;
    document.documentElement.style.setProperty('--safe-area-top', `${Math.round(top)}px`);
    document.documentElement.style.setProperty('--safe-area-bottom', `${Math.round(bottom)}px`);
  };

  const closeDrawer = () => {
    drawerOpen = false;
    popLayer('drawer');
    popHistoryLayer();
  };

  const openDrawer = () => {
    drawerOpen = true;
    const drawerEntry = {
      id: 'drawer',
      kind: LAYER_KIND_DRAWER,
      scrollLock: true,
      safeArea: true,
      onForeground: () => document.body.style.setProperty('overscroll-behavior', 'contain'),
      onBackground: () => document.body.style.removeProperty('overscroll-behavior')
    };
    if (!layerExists('drawer')) {
      pushLayer(drawerEntry);
    } else {
      popLayer('drawer');
      pushLayer(drawerEntry);
    }
    pushHistoryLayer('drawer');
  };

  const openApp = (app: ResolvedApp) => {
    const layerId = `app-${app.id}`;
    const entry = {
      id: layerId,
      kind: LAYER_KIND_APP,
      scrollLock: true,
      safeArea: true,
      payload: { appId: app.id },
      onForeground: () => document.body.classList.add('app-foreground'),
      onBackground: () => document.body.classList.remove('app-foreground')
    };

    const hadForegroundApp = Boolean(activeAppLayerId);

    // single-visible: clear other apps, then push this one
    clearAppLayers(layerId);
    if (layerExists(layerId)) popLayer(layerId);
    pushLayer(entry);

    // replace drawer history entry with app when launching from drawer; otherwise push a new entry
    const launchingFromDrawer = layerExists('drawer');
    if (launchingFromDrawer) {
      if (historyLayerCount > 0) {
        history.replaceState({ piccoloLayer: layerId }, '', window.location.href);
      }
      drawerOpen = false;
      popLayer('drawer');
    } else if (hadForegroundApp && historyLayerCount > 0) {
      history.replaceState({ piccoloLayer: layerId }, '', window.location.href);
      historyLayerCount = 1;
    } else {
      pushHistoryLayer(layerId);
    }

    activeAppLayerId = layerId;
    activeApp = app;
  };

  const closeApp = () => {
    clearAppLayers();
    if (activeAppLayerId) popLayer(activeAppLayerId);
    popHistoryLayer();
    activeApp = null;
    activeAppLayerId = null;
  };

  const closeTopLayerFromHistory = () => {
    const currentTop = get(topLayer);
    if (!currentTop || currentTop.id === 'stage') return;
    if (currentTop.kind === LAYER_KIND_APP) {
      clearAppLayers();
      if (activeAppLayerId) popLayer(activeAppLayerId);
      activeApp = null;
      activeAppLayerId = null;
    } else if (currentTop.kind === LAYER_KIND_DRAWER) {
      drawerOpen = false;
      popLayer('drawer');
    }
  };

  const openAppInNewTab = (app: ResolvedApp) => {
    const url = new URL(window.location.href);
    url.searchParams.set('app', app.id);
    window.open(url.toString(), '_blank', 'noopener');
  };

  const hydrateAppFromUrl = () => {
    const url = new URL(window.location.href);
    const appParam = url.searchParams.get('app');
    if (appParam) {
      const match = get(resolvedApps).find((a) => a.id === appParam);
      if (match) {
        openApp(match);
      }
    }
  };

  onMount(() => {
    const media = window.matchMedia('(prefers-reduced-motion: reduce)');
    const updateMotion = () => (reduceMotion = media.matches);
    updateMotion();
    media.addEventListener('change', updateMotion);
    unsubscribers.push(() => media.removeEventListener('change', updateMotion));

    const handlePopState = (event: PopStateEvent) => {
      const targetId = event.state?.piccoloLayer as string | undefined;

      if (suppressNextPop) {
        suppressNextPop = false;
        return;
      }

      // Forward navigation to a stored overlay state
      if (targetId && $topLayer?.id === 'stage') {
        if (targetId === 'drawer') {
          openDrawerFromHistory();
        } else if (targetId.startsWith('app-')) {
          const appId = targetId.replace(/^app-/, '');
          openAppFromHistory(appId);
        }
        historyLayerCount = Math.max(historyLayerCount, 1);
        return;
      }

      // Back navigation: close current overlay
      if ($topLayer?.id && $topLayer.id !== 'stage') {
        closeTopLayerFromHistory();
        if (historyLayerCount > 0) historyLayerCount -= 1;
      }
    };

    window.addEventListener('popstate', handlePopState);
    unsubscribers.push(() => window.removeEventListener('popstate', handlePopState));

    appManifests.set(seedApps);

    setSafeAreas();
    const handleResize = () => setSafeAreas();
    window.addEventListener('resize', handleResize);
    resizeObserver = new ResizeObserver(setSafeAreas);
    if (headerEl) resizeObserver.observe(headerEl);
    if (dockEl) resizeObserver.observe(dockEl);

    const lifecycle = createLayerLifecycle().subscribe(() => {});
    unsubscribers.push(lifecycle);

    hydrateAppFromUrl();

    return () => {
      unsubscribers.forEach((fn) => fn());
      window.removeEventListener('resize', handleResize);
      resizeObserver?.disconnect();
      document.documentElement.style.removeProperty('--safe-area-top');
      document.documentElement.style.removeProperty('--safe-area-bottom');
      document.body.style.overflow = '';
      resetStack();
    };
  });

  onDestroy(() => {
    resizeObserver?.disconnect();
    document.documentElement.style.removeProperty('--safe-area-top');
    document.documentElement.style.removeProperty('--safe-area-bottom');
    document.body.style.overflow = '';
    resetStack();
  });
</script>

<svelte:head>
  <title>Piccolo OS — Digital Sanctuary</title>
</svelte:head>

<main class="relative min-h-screen text-ink" style="background: var(--hero-gradient);">
  <header class="fixed left-0 right-0 top-0 z-50 px-6 py-4 sm:px-10 lg:px-16" bind:this={headerEl}>
    <div class="flex items-center justify-between rounded-2xl border border-white/40 bg-white/80 px-4 py-3 shadow-md backdrop-blur-2xl dark:border-white/10 dark:bg-slate-900/80">
      <div class="flex items-center gap-3">
        <div class="flex h-11 w-11 items-center justify-center rounded-2xl bg-gradient-to-br from-indigo-500 to-blue-600 text-base font-semibold text-white shadow-md">
          P
        </div>
        <div class="leading-tight">
          <p class="text-[11px] uppercase tracking-[0.16em] text-muted">Piccolo OS</p>
          <p class="text-sm font-semibold text-ink">Digital Sanctuary</p>
        </div>
      </div>
      <div class="flex items-center gap-3">
        <label class="relative hidden md:block">
          <input
            class="h-10 w-64 rounded-full border border-white/50 bg-white/70 px-10 text-sm text-ink placeholder:text-muted shadow-sm backdrop-blur-xl focus:outline-none focus:ring-2 focus:ring-blue-200 dark:border-white/10 dark:bg-slate-800/80"
            placeholder="Search apps or settings"
            type="search"
          />
          <span class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-muted">⌘K</span>
        </label>
        <div class="hidden items-center gap-2 rounded-full border border-white/50 bg-white/70 px-3 py-2 text-xs font-medium text-emerald-700 shadow-sm backdrop-blur-xl dark:border-white/10 dark:bg-slate-800/80 dark:text-emerald-200 sm:flex">
          <span class="inline-flex h-2 w-2 rounded-full bg-emerald-500" aria-hidden="true"></span>
          System healthy
        </div>
        <Button variant="primary" on:click={openDrawer}>
          Open Drawer
        </Button>
      </div>
    </div>
  </header>

  <section
    class={`stage relative z-0 px-6 sm:px-10 lg:px-16 ${activeApp ? 'stage--blocked' : ''}`}
    aria-label="Stage content"
    use:layerStateAction={{ entry: { id: 'stage', kind: 'stage' }, isForeground: $topLayer?.id === 'stage' }}
  >
    <div class="grid gap-6 lg:grid-cols-3">
      <article class="lg:col-span-2 rounded-3xl border border-white/40 bg-white/80 backdrop-blur-xl elev-3 p-6 text-ink shadow-lg dark:border-white/10 dark:bg-slate-900/85">
        <p class="meta-label">Stage</p>
        <h1 class="mt-3 text-3xl font-semibold">Your personal cloud OS</h1>
        <p class="mt-2 text-base text-muted max-w-2xl">
          The Stage keeps widgets like Storage and Memories alive under your apps. Launchers float above it, and closing an app
          restores the exact state you left behind.
        </p>
        <div class="mt-4 flex flex-wrap gap-3">
          <Button variant="primary" href="/setup">
            Continue setup
          </Button>
          <Button variant="secondary" href="/docs/foundation.md" target="_blank">
            View foundations
          </Button>
          <Button variant="ghost" on:click={openDrawer}>
            Browse apps
          </Button>
        </div>
        <div class="mt-6 grid gap-4 sm:grid-cols-2">
          <div class="rounded-2xl border border-white/40 bg-white/70 p-4 shadow-sm backdrop-blur-xl dark:border-white/10 dark:bg-slate-800/70">
            <p class="text-xs uppercase tracking-[0.18em] text-muted">Layer B · Stage</p>
            <p class="mt-2 text-sm text-ink">Widgets remain visible when apps close.</p>
          </div>
          <div class="rounded-2xl border border-white/40 bg-white/70 p-4 shadow-sm backdrop-blur-xl dark:border-white/10 dark:bg-slate-800/70">
            <p class="text-xs uppercase tracking-[0.18em] text-muted">Layer C · Launcher</p>
            <p class="mt-2 text-sm text-ink">Floating Dock for pinned apps; Drawer for everything else.</p>
          </div>
        </div>
      </article>

      <article class="rounded-3xl border border-white/30 bg-white/75 backdrop-blur-xl elev-2 p-5 shadow-md dark:border-white/10 dark:bg-slate-900/80">
        <p class="meta-label">Status</p>
        <h2 class="text-lg font-semibold">System pulse</h2>
        <ul class="mt-3 space-y-2 text-sm text-muted">
          <li class="flex items-center justify-between">
            <span>Updates</span>
            <span class="rounded-full bg-emerald-50 px-2 py-1 text-xs font-medium text-emerald-700 dark:bg-emerald-500/20 dark:text-emerald-100">Calm</span>
          </li>
          <li class="flex items-center justify-between">
            <span>Storage</span>
            <span class="text-xs font-medium text-ink">1.2 TB free</span>
          </li>
          <li class="flex items-center justify-between">
            <span>Memories</span>
            <span class="text-xs font-medium text-ink">3 albums live</span>
          </li>
        </ul>
        <div class="mt-4 rounded-2xl border border-white/40 bg-white/70 p-4 text-xs text-muted shadow-sm backdrop-blur-xl dark:border-white/10 dark:bg-slate-800/70">
          Floating Dock anchors pinned apps; running apps flare to the right. Drawer opens as a frosted full-screen overlay.
        </div>
      </article>
    </div>

    <div class="mt-8 grid gap-6 lg:grid-cols-3">
      <article class="rounded-2xl border border-white/30 bg-white/75 backdrop-blur-xl elev-2 p-5 shadow-md dark:border-white/10 dark:bg-slate-900/80">
        <p class="meta-label">Updates lane</p>
        <h2 class="text-lg font-semibold">MicroOS transactional update</h2>
        <p class="text-sm text-muted">
          UI will consume <code>/updates/os</code> to show staged snapshots, reboot prompts, and apply/rollback actions without leaving the Stage.
          Behavior aligns with <code>docs/rfc/20251124-microos-transactional-update.md</code>.
        </p>
      </article>
      <article class="rounded-2xl border border-white/30 bg-white/75 backdrop-blur-xl elev-2 p-5 shadow-md dark:border-white/10 dark:bg-slate-900/80">
        <p class="meta-label">Stack</p>
        <h2 class="text-lg font-semibold">Tooling installed</h2>
        <ul class="list-disc list-inside space-y-1 text-sm text-muted">
          <li>SvelteKit + TypeScript</li>
          <li>Tailwind CSS / PostCSS / Autoprefixer</li>
          <li>@tanstack/svelte-query (with provider)</li>
        </ul>
      </article>
      <article class="rounded-2xl border border-white/30 bg-white/75 backdrop-blur-xl elev-2 p-5 shadow-md dark:border-white/10 dark:bg-slate-900/80">
        <p class="meta-label">Docs</p>
        <h2 class="text-lg font-semibold">Source of truth</h2>
        <ul class="space-y-2 text-sm text-muted">
          <li><code>ui-next/docs/ui-architecture/00_interaction_model.md</code></li>
          <li><code>ui-next/docs/foundation.md</code></li>
          <li><code>ui-next/docs/theme-brief.md</code></li>
        </ul>
      </article>
    </div>
  </section>

  {#if activeApp}
    <section
      class="app-layer"
      use:layerStateAction={{ entry: { id: activeAppLayerId ?? '', kind: 'app'  }, isForeground: $topLayer?.id === activeAppLayerId }}
      use:scrollLock={$topLayer?.id === activeAppLayerId}
      in:fly={reduceMotion ? { duration: 0 } : { y: 16, duration: 180, easing: cubicOut }}
      out:fade={reduceMotion ? { duration: 0 } : { duration: 120 }}
      aria-live="polite"
      aria-label={`Active app ${activeApp.displayName}`}
      data-layer-state={$topLayer?.id === activeAppLayerId ? 'foreground' : 'background'}
    >
      <div class="app-shell">
        <div class="app-window" aria-label={`${activeApp.displayName} window`}>
          <header class="window-chrome">
            <div class="flex items-center gap-3">
              <div class="pill-icon">
                {activeApp.displayName.slice(0, 2).toUpperCase()}
              </div>
              <div class="leading-tight">
                <p class="text-[11px] uppercase tracking-[0.16em] text-muted">App</p>
                <p class="text-base font-semibold text-ink">{activeApp.displayName}</p>
              </div>
            </div>
            <div class="flex items-center gap-2">
              <Button variant="ghost" size="compact" on:click={closeApp}>Close</Button>
              <Button variant="secondary" size="compact" on:click={openDrawer}>Switch app</Button>
              <Button variant="ghost" size="compact" on:click={() => activeApp && openAppInNewTab(activeApp)}>Open in new tab</Button>
            </div>
          </header>

          <div class="app-body" aria-label={`${activeApp.displayName} content`}>
            {#if activeApp.id === 'settings'}
              <SettingsActivity />
            {:else}
              <div class="rounded-3xl border border-white/40 bg-white/85 p-6 shadow-lg backdrop-blur-2xl dark:border-white/10 dark:bg-slate-900/85">
                <p class="meta-label">Layer B · Activity</p>
                <h3 class="mt-2 text-xl font-semibold">Immersive view</h3>
                <p class="mt-2 text-sm text-muted">
                  App content stretches edge-to-edge while respecting the safe areas above and below the Dock/Top Bar. Scroll to see how content flows under
                  the Frame while controls remain reachable.
                </p>
                <div class="mt-4 grid gap-4 sm:grid-cols-2">
                  <div class="rounded-2xl border border-white/50 bg-white/80 p-4 backdrop-blur-xl dark:border-white/10 dark:bg-slate-800/80">
                    <p class="text-xs uppercase tracking-[0.18em] text-muted">Safe area</p>
                    <p class="mt-1 text-sm text-ink">Top padding from measured bar height: <code>var(--safe-area-top)</code></p>
                  </div>
                  <div class="rounded-2xl border border-white/50 bg-white/80 p-4 backdrop-blur-xl dark:border-white/10 dark:bg-slate-800/80">
                    <p class="text-xs uppercase tracking-[0.18em] text-muted">Single task</p>
                    <p class="mt-1 text-sm text-ink">Only one heavy activity is mounted at a time; switchers replace the current view.</p>
                  </div>
                </div>
              </div>
            {/if}
          </div>
        </div>
      </div>
    </section>
  {/if}

  <div
    class="dock-wrapper fixed left-1/2 z-50 -translate-x-1/2"
    bind:this={dockEl}
    aria-label="App dock"
  >
    <Dock pinned={$pinnedApps} running={$runningApps} on:select={(e) => openApp(e.detail)} />
  </div>

  {#if drawerOpen}
    <div
      class="fixed inset-0 z-[100] bg-white/75 backdrop-blur-2xl transition-opacity dark:bg-slate-900/85"
      transition:fade={reduceMotion ? { duration: 0 } : { duration: 120 }}
    >
      <div
        class="drawer-surface mx-auto max-w-5xl px-6 py-8 sm:py-10"
        aria-label="App drawer"
        use:layerStateAction={{ entry: { id: 'drawer', kind: 'drawer' }, isForeground: $topLayer?.id === 'drawer' }}
        use:scrollLock={$topLayer?.id === 'drawer'}
        transition:fly={reduceMotion ? { duration: 0 } : { y: 12, duration: 160, easing: cubicOut }}
        on:touchstart={(e) => startY = e.touches[0]?.clientY ?? null}
        on:touchmove={(e) => {
          if (startY === null) return;
          const delta = e.touches[0].clientY - startY;
          if (delta > 38) {
            startY = null;
            closeDrawer();
          }
        }}
        on:touchend={() => (startY = null)}
      >
        <div class="flex items-center justify-between gap-4">
          <div>
            <p class="meta-label">Layer C · Drawer</p>
            <h2 class="text-2xl font-semibold">Launch an app</h2>
            <p class="text-sm text-muted">Full-screen activities float above the Stage. Close to return exactly where you left off.</p>
          </div>
          <Button variant="secondary" on:click={closeDrawer}>
            Close
          </Button>
        </div>
        <div class="drawer-grid mt-6 grid grid-cols-3 gap-5 sm:grid-cols-4">
          {#each $drawerApps as app (app.id)}
            <AppIcon app={app} size={64} on:select={(e) => openApp(e.detail)} />
          {/each}
        </div>
      </div>
    </div>
  {/if}
</main>

<style>
  .stage {
    min-height: 100vh;
    padding-top: var(--safe-area-top);
    padding-bottom: var(--safe-area-bottom);
    overflow-y: auto;
    scrollbar-gutter: stable;
    position: relative;
    z-index: 0;
  }

  .stage--blocked {
    overflow: hidden;
    pointer-events: none;
  }

  .dock-wrapper {
    bottom: 8px;
  }

  [data-layer-state='background'] {
    pointer-events: none;
    overflow: hidden !important;
  }

  .drawer-surface {
    padding-top: var(--safe-area-top);
    padding-bottom: var(--safe-area-bottom);
  }

  .drawer-grid {
    justify-items: center;
    align-items: flex-start;
  }

  .app-layer {
    position: fixed;
    inset: 0;
    z-index: 10;
    padding: var(--safe-area-top) 16px var(--safe-area-bottom) 16px;
    overflow: hidden;
    background: radial-gradient(circle at 20% 10%, rgba(255, 255, 255, 0.7), rgba(235, 240, 250, 0.8));
  }

  :global([data-theme='dark']) .app-layer {
    background: radial-gradient(circle at 20% 10%, rgba(26, 32, 48, 0.9), rgba(12, 16, 28, 0.95));
  }

  .app-shell {
    margin: 0 auto;
    max-width: 1100px;
    display: flex;
    flex-direction: column;
    height: calc(100vh - var(--safe-area-top) - var(--safe-area-bottom) - 16px);
    align-items: center;
  }

  .app-window {
    width: min(1080px, calc(100vw - 32px));
    max-height: calc(100vh - var(--safe-area-top) - var(--safe-area-bottom));
    background: rgba(255, 255, 255, 0.94);
    border-radius: 22px;
    border: 1px solid rgba(255, 255, 255, 0.65);
    box-shadow: 0 28px 70px rgba(18, 24, 40, 0.2);
    backdrop-filter: blur(18px);
    pointer-events: auto;
    display: flex;
    flex-direction: column;
    overflow: hidden;
    min-height: 0;
  }

  :global([data-theme='dark']) .app-window {
    background: rgba(18, 22, 34, 0.92);
    border-color: rgba(255, 255, 255, 0.08);
    box-shadow: 0 28px 70px rgba(0, 0, 0, 0.4);
  }

  .window-chrome {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding: 12px 16px;
    border-bottom: 1px solid rgba(255, 255, 255, 0.5);
    background: linear-gradient(180deg, rgba(255, 255, 255, 0.92), rgba(255, 255, 255, 0.86));
  }

  :global([data-theme='dark']) .window-chrome {
    border-color: rgba(255, 255, 255, 0.08);
    background: linear-gradient(180deg, rgba(22, 26, 38, 0.96), rgba(18, 22, 34, 0.9));
  }

  .app-body {
    padding: 0;
    flex: 1;
    overflow-y: auto;
    overflow-x: visible;
    min-height: 0;
  }

  .pill-icon {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 36px;
    height: 36px;
    border-radius: 12px;
    background: linear-gradient(180deg, #3d66ff, #2f5af3);
    color: white;
    font-weight: 700;
    font-size: 13px;
  }

  @media (max-width: 540px) {
    .dock-wrapper {
      bottom: 12px;
    }

    .drawer-grid {
      grid-template-columns: repeat(3, minmax(0, 1fr));
    }

    .window-chrome {
      flex-direction: column;
      align-items: flex-start;
      gap: 8px;
    }
  }
</style>
