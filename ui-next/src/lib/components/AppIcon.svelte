<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import { createFallbackIcon } from '$lib/stores/apps';
  import type { ResolvedApp } from '$lib/types/apps';

  export let app: ResolvedApp;
  export let size = 56;

  let loadError = false;
  let fallbackIcon = createFallbackIcon(app.displayName);
  let resolvedIcon = app.resolvedIcon;
  let currentIcon = resolvedIcon || fallbackIcon;

  $: fallbackIcon = createFallbackIcon(app.displayName);
  $: resolvedIcon = app.resolvedIcon;
  $: if (resolvedIcon) {
    loadError = false;
  }
  $: currentIcon = loadError ? fallbackIcon : resolvedIcon || fallbackIcon;

  const dispatch = createEventDispatcher<{ select: ResolvedApp }>();

  const handleError = () => {
    loadError = true;
  };

  const handleSelect = () => {
    dispatch('select', app);
  };
</script>

<div
  class="flex flex-col items-center gap-1 cursor-pointer select-none"
  style={`width: clamp(56px, 20vw, ${size + 12}px);`}
  title={app.displayName}
  role="button"
  tabindex="0"
  on:click={handleSelect}
  on:keydown={(e) => (e.key === 'Enter' || e.key === ' ') && handleSelect()}
>
  <div
    class="rounded-2xl border border-white/50 bg-white/70 backdrop-blur-2xl shadow-lg flex items-center justify-center overflow-hidden ring-1 ring-white/40"
    style={`width: clamp(52px, 18vw, ${size}px); height: clamp(52px, 18vw, ${size}px);`}
  >
    <img
      src={currentIcon}
      alt={`${app.displayName} icon`}
      class="h-full w-full object-cover"
      loading="lazy"
      on:error={handleError}
    />
  </div>
  <p class="text-[11px] text-center text-muted leading-tight truncate w-full">
    {app.displayName}
    {#if app.isSystem}
      <span class="ml-1 inline-flex items-center rounded-full bg-blue-50 px-1.5 py-[1px] text-[10px] font-medium text-blue-700">
        sys
      </span>
    {/if}
  </p>
</div>
