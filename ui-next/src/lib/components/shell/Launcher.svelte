<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import { fade, fly } from 'svelte/transition';
  import { cubicOut } from 'svelte/easing';
  import AppIcon from '$lib/components/AppIcon.svelte';
  import Dock from '$lib/components/Dock.svelte';
  import Button from '$lib/components/ui/Button.svelte';
  import { layerState as layerStateAction } from '$lib/actions/layerState';
  import { scrollLock } from '$lib/actions/scrollLock';
  import type { ResolvedApp } from '$lib/types/apps';
  import type { LayerEntry as Layer } from '$lib/stores/layers';

  export let isDrawerOpen = false;
  export let pinnedApps: ResolvedApp[] = [];
  export let runningApps: ResolvedApp[] = [];
  export let drawerApps: ResolvedApp[] = [];
  export let topLayer: Layer | null = null;
  export let reduceMotion = false;
  export let bindDockEl: HTMLElement | null = null;
  export let safeAreaTop: string = '0px';
  export let safeAreaBottom: string = '0px';

  const dispatch = createEventDispatcher<{
    openApp: ResolvedApp;
    closeDrawer: void;
  }>();

  let startY: number | null = null;

  function handleOpenApp(app: ResolvedApp) {
    dispatch('openApp', app);
  }

  function closeDrawer() {
    dispatch('closeDrawer');
  }
</script>

<div
  class="dock-wrapper fixed left-1/2 z-50 -translate-x-1/2"
  bind:this={bindDockEl}
  aria-label="App dock"
>
  <Dock pinned={pinnedApps} running={runningApps} on:select={(e) => handleOpenApp(e.detail)} />
</div>

{#if isDrawerOpen}
  <div
    class="fixed inset-0 z-[100] bg-white/75 backdrop-blur-2xl transition-opacity dark:bg-slate-900/85"
    transition:fade={reduceMotion ? { duration: 0 } : { duration: 120 }}
    style="--safe-area-top: {safeAreaTop}; --safe-area-bottom: {safeAreaBottom};"
  >
    <div
      class="drawer-surface mx-auto max-w-5xl px-6 py-8 sm:py-10"
      aria-label="App drawer"
      use:layerStateAction={{ entry: { id: 'drawer', kind: 'drawer' }, isForeground: topLayer?.id === 'drawer' }}
      use:scrollLock={topLayer?.id === 'drawer'}
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
        {#each drawerApps as app (app.id)}
          <AppIcon app={app} size={64} on:select={(e) => handleOpenApp(e.detail)} />
        {/each}
      </div>
    </div>
  </div>
{/if}

<style>
  .dock-wrapper {
    bottom: 8px;
  }

  .drawer-surface {
    padding-top: var(--safe-area-top);
    padding-bottom: var(--safe-area-bottom);
  }

  .drawer-grid {
    justify-items: center;
    align-items: flex-start;
  }

  @media (max-width: 540px) {
    .dock-wrapper {
      bottom: 12px;
    }

    .drawer-grid {
      grid-template-columns: repeat(3, minmax(0, 1fr));
    }
  }
</style>
