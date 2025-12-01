<script lang="ts">
  import { onDestroy, onMount, setContext } from 'svelte';
  import { derived, get } from 'svelte/store';
  import Frame from './Frame.svelte';
  import Launcher from './Launcher.svelte';
  import Window from './Window.svelte';
  import Stage from './Stage.svelte';
  import Wallpaper from './Wallpaper.svelte';
  import SettingsActivity from '$lib/components/settings/SettingsActivity.svelte';
  import ToastHost from '$lib/components/ToastHost.svelte';

  import { appManifests, resolvedApps } from '$lib/stores/apps';
  import { LAYER_KIND_APP, LAYER_KIND_DRAWER, layerExists, layerStack, popLayer, pushLayer, resetStack, topLayer } from '$lib/stores/layers';
  import { createLayerLifecycle } from '$lib/stores/layerLifecycle';
  import { preferencesStore } from '$lib/stores/preferences';
  import type { AppManifest, ResolvedApp } from '$lib/types/apps';

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
  
  let safeAreas = { top: '0px', bottom: '0px' };

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
    safeAreas = {
      top: `${Math.round(top)}px`,
      bottom: `${Math.round(bottom)}px`
    };
    document.documentElement.style.setProperty('--safe-area-top', safeAreas.top);
    document.documentElement.style.setProperty('--safe-area-bottom', safeAreas.bottom);
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

    clearAppLayers(layerId);
    if (layerExists(layerId)) popLayer(layerId);
    pushLayer(entry);

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

  setContext('shell', {
    openDrawer,
    openApp: (app: ResolvedApp) => openApp(app)
  });

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

      if ($topLayer?.id && $topLayer.id !== 'stage') {
        closeTopLayerFromHistory();
        if (historyLayerCount > 0) historyLayerCount -= 1;
      }
    };

    window.addEventListener('popstate', handlePopState);
    unsubscribers.push(() => window.removeEventListener('popstate', handlePopState));

    appManifests.set(seedApps);

    // Initial safe area set - might be 0 if elements not bound yet
    setTimeout(setSafeAreas, 0); 
    const handleResize = () => setSafeAreas();
    window.addEventListener('resize', handleResize);
    resizeObserver = new ResizeObserver(setSafeAreas);
    // bindings are reactive, observer needs real elements.
    // We observe in a reactive statement or after mount if elements are bound?
    // elements are bound via bind:this
    
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
  
  // Reactive observer hookup
  $: if (headerEl && resizeObserver) resizeObserver.observe(headerEl);
  $: if (dockEl && resizeObserver) resizeObserver.observe(dockEl);

  onDestroy(() => {
     // cleanup handled in onMount return
  });
</script>

<main class="relative min-h-screen text-ink">
  <Wallpaper />
  <Frame bind:bindHeaderEl={headerEl} on:openDrawer={openDrawer} />

  <Stage 
    safeAreaTop={safeAreas.top} 
    safeAreaBottom={safeAreas.bottom}
    hasActiveApp={!!activeApp}
    isForeground={$topLayer?.id === 'stage'}
  >
    <slot />
  </Stage>

  {#if activeApp}
    <Window 
      activeApp={activeApp}
      layerId={activeAppLayerId ?? ''}
      isTopLayer={$topLayer?.id === activeAppLayerId}
      reduceMotion={reduceMotion}
      safeAreaTop={safeAreas.top}
      safeAreaBottom={safeAreas.bottom}
      on:close={closeApp}
      on:switchApp={openDrawer}
      on:openNewTab={() => activeApp && openAppInNewTab(activeApp)}
    >
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
    </Window>
  {/if}

  <Launcher
     bind:bindDockEl={dockEl}
     isDrawerOpen={drawerOpen}
     pinnedApps={$pinnedApps}
     runningApps={$runningApps}
     drawerApps={$drawerApps}
     topLayer={$topLayer}
     reduceMotion={reduceMotion}
     safeAreaTop={safeAreas.top}
     safeAreaBottom={safeAreas.bottom}
     on:openApp={(e) => openApp(e.detail)}
     on:closeDrawer={closeDrawer}
  />

  <ToastHost />
</main>
