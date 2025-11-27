<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import AppIcon from '$lib/components/AppIcon.svelte';
  import type { ResolvedApp } from '$lib/types/apps';

  export let pinned: ResolvedApp[] = [];
  export let running: ResolvedApp[] = [];

  $: pinnedIds = new Set(pinned.map((app) => app.id));
  $: runningExclusive = running.filter((app) => !pinnedIds.has(app.id));

  const dispatch = createEventDispatcher<{ select: ResolvedApp }>();

  const handleSelect = (event: CustomEvent<ResolvedApp>) => {
    dispatch('select', event.detail);
  };
</script>

<nav class="dock-glass flex items-center gap-3 overflow-x-auto" aria-label="Pinned and running apps">
  <div class="flex items-center gap-3 flex-shrink-0">
    {#if pinned.length === 0}
      <span class="text-xs text-muted">No pinned apps yet</span>
    {:else}
      {#each pinned as app (app.id)}
        <AppIcon app={app} size={48} on:select={handleSelect} />
      {/each}
    {/if}
  </div>

  {#if runningExclusive.length}
    <span class="dock-divider" aria-hidden="true"></span>
    <div class="flex items-center gap-3 flex-shrink-0">
      {#each runningExclusive as app (app.id)}
        <AppIcon app={app} size={48} on:select={handleSelect} />
      {/each}
    </div>
  {/if}
</nav>

<style>
  nav.dock-glass {
    padding: 12px 16px;
    border-radius: 22px;
    border: 1px solid rgba(255, 255, 255, 0.55);
    background: rgba(255, 255, 255, 0.78);
    box-shadow: 0 20px 50px rgba(18, 24, 40, 0.14);
    backdrop-filter: blur(20px);
    max-width: min(1100px, calc(100vw - 24px));
    width: max-content;
    scrollbar-width: none;
  }

  nav.dock-glass::-webkit-scrollbar {
    display: none;
  }

  :global([data-theme='dark']) nav.dock-glass {
    background: rgba(30, 36, 54, 0.82);
    border-color: rgba(255, 255, 255, 0.12);
    box-shadow: 0 20px 50px rgba(0, 0, 0, 0.35);
  }

  .dock-divider {
    display: inline-flex;
    width: 1px;
    height: 42px;
    background: rgba(20, 24, 33, 0.12);
    margin: 0 10px;
  }

  :global([data-theme='dark']) .dock-divider {
    background: rgba(255, 255, 255, 0.16);
  }

  @media (max-width: 640px) {
    nav.dock-glass {
      padding: 10px 12px;
      gap: 0.75rem;
      max-width: min(100%, calc(100vw - 12px));
    }

    .dock-divider {
      height: 36px;
      margin: 0 8px;
    }
  }
</style>
