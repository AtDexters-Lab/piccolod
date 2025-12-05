<script lang="ts">
  import { getContext, onDestroy, onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import SettingCard from '$lib/components/settings/SettingCard.svelte';
  import Button from '$lib/components/ui/Button.svelte';
  import { preferencesStore } from '$lib/stores/preferences';
  import { toasts } from '$lib/stores/toasts';
  import type { Writable } from 'svelte/store';
  import type { SettingsNavState } from '../types';
  import type { SettingsActivityContext } from '../activityContext';
  import { settingsActivityKey } from '../activityContext';

  let density: 'comfortable' | 'compact' = 'comfortable';
  const navStore = getContext<Writable<SettingsNavState> | null>('settings-nav');
  const activity = getContext<SettingsActivityContext | null>(settingsActivityKey);
  let navState: SettingsNavState | null = null;
  const unsub = navStore?.subscribe((value) => (navState = value));
  onDestroy(() => unsub?.());

  onMount(() => {
    if (typeof window === 'undefined') return;
    const saved = window.localStorage.getItem('piccolo.settings.density');
    if (saved === 'compact' || saved === 'comfortable') {
      density = saved;
    }
  });

  const updateDensity = (next: 'comfortable' | 'compact') => {
    density = next;
    if (typeof window !== 'undefined') {
      window.localStorage.setItem('piccolo.settings.density', next);
    }
    toasts.push({ message: `Density set to ${next} (local-only)`, variant: 'info', timeout: 2600 });
  };

  const setTheme = (theme: 'light' | 'dark') => {
    preferencesStore.update((current) => ({ ...current, theme }));
    toasts.push({ message: `Theme switched to ${theme} (local-only until prefs API lands)`, variant: 'info', timeout: 3000 });
  };

  const setBackground = (bg: 'aurora' | 'midnight' | 'plain') => {
    preferencesStore.update((current) => ({ ...current, background: bg }));
    toasts.push({ message: `Wallpaper set to ${bg}`, variant: 'success', timeout: 2400 });
  };

  const setAccent = (accent: string) => {
    preferencesStore.update((current) => ({ ...current, accent }));
    toasts.push({ message: 'Accent updated (local-only)', variant: 'success', timeout: 2400 });
  };
</script>

{#if navState && !navState.isSplit}
  <div class="mb-3">
    <Button variant="ghost" size="compact" on:click={() => (activity?.setActive ? activity.setActive(null) : goto('/apps/settings'))}>
      Back
    </Button>
  </div>
{/if}

<div class="card-stack">
  <div class="grid gap-6 md:grid-cols-2">
    <SettingCard title="Theme" description="Light or dark">
      <div class="grid grid-cols-2 gap-3">
        <button class="tile" on:click={() => setTheme('light')} data-active={$preferencesStore.theme === 'light'}>
          <p class="tile-title">Light</p>
          <p class="tile-desc">Bright frosted panels</p>
        </button>
        <button class="tile" on:click={() => setTheme('dark')} data-active={$preferencesStore.theme === 'dark'}>
          <p class="tile-title">Dark</p>
          <p class="tile-desc">Deep glass with cobalt accents</p>
        </button>
      </div>
    </SettingCard>

    <SettingCard title="Wallpaper" description="Hero canvas pattern">
      <div class="grid grid-cols-3 gap-3">
        {#each ['aurora', 'midnight', 'plain'] as option}
          <button class="tile" data-active={$preferencesStore.background === option} on:click={() => setBackground(option as 'aurora' | 'midnight' | 'plain')}>
            <p class="tile-title">{option}</p>
            <p class="tile-desc">{option === 'plain' ? 'Solid' : 'Gradient'}</p>
          </button>
        {/each}
      </div>
    </SettingCard>
  </div>

  <div class="card-stack">
    <SettingCard title="Accent color" description="Hex value" actions={true}>
      <svelte:fragment slot="actions">
        <Button variant="secondary" size="compact" on:click={() => setAccent('#2F5AF3')}>Reset</Button>
      </svelte:fragment>
      <div class="flex items-center gap-3">
        <input
          type="color"
          class="color"
          value={$preferencesStore.accent}
          on:input={(e) => setAccent((e.target as HTMLInputElement).value)}
          aria-label="Accent color"
        />
        <code class="text-sm text-muted">{$preferencesStore.accent}</code>
      </div>
      <p class="text-xs text-muted mt-2">TODO: Persist via server-side preferences endpoint.</p>
    </SettingCard>

    <SettingCard title="Density" description="Control spacing for tables and lists">
      <div class="grid grid-cols-2 gap-3">
        <button class="tile" data-active={density === 'comfortable'} on:click={() => updateDensity('comfortable')}>
          <p class="tile-title">Comfortable</p>
          <p class="tile-desc">Default spacing</p>
        </button>
        <button class="tile" data-active={density === 'compact'} on:click={() => updateDensity('compact')}>
          <p class="tile-title">Compact</p>
          <p class="tile-desc">Tighter rows</p>
        </button>
      </div>
      <p class="text-xs text-muted mt-2">Stored locally in <code>piccolo.settings.density</code> until server support is added.</p>
    </SettingCard>
  </div>
</div>

<style>
  .tile {
    text-align: left;
    padding: 12px;
    border-radius: 12px;
    border: 1px solid var(--card-border);
    background: rgba(var(--sys-surface-variant), 0.9);
    transition: border-color 140ms var(--motion-ease-standard), background 140ms var(--motion-ease-standard);
  }

  .tile[data-active='true'] {
    border-color: rgba(var(--sys-accent-rgb), 0.45);
    box-shadow: 0 10px 20px rgba(var(--sys-accent-rgb), 0.18);
  }

  .tile-title {
    font-weight: 600;
    margin-bottom: 2px;
    color: rgb(var(--sys-ink));
  }

  .tile-desc {
    font-size: 12px;
    color: rgb(var(--sys-ink-muted));
  }

  .color {
    width: 44px;
    height: 34px;
    border: 1px solid var(--card-border);
    border-radius: 10px;
    background: transparent;
    padding: 4px;
  }

</style>
