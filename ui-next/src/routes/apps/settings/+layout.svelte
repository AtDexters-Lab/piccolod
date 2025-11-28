<script lang="ts">
  import { goto } from '$app/navigation';
  import { setContext } from 'svelte';
  import { onMount } from 'svelte';
  import { writable } from 'svelte/store';
  import SettingsNav from '$lib/components/settings/SettingsNav.svelte';
  import type { SettingsNavSection, SettingsNavState } from '$lib/components/settings/types';
  import Button from '$lib/components/ui/Button.svelte';

  const navSections: SettingsNavSection[] = [
    {
      id: 'personal',
      label: 'Identity & Appearance',
      items: [
        { id: 'profile', label: 'Profile', description: 'Password & recovery key', href: '/apps/settings/profile' },
        { id: 'appearance', label: 'Appearance', description: 'Theme, wallpaper, density', href: '/apps/settings/appearance', badge: 'Local only' }
      ]
    },
    {
      id: 'connectivity',
      label: 'Connectivity',
      items: [{ id: 'remote', label: 'Remote access', description: 'Piccolo.link and domains', href: '/apps/settings/remote-access' }]
    },
    {
      id: 'system',
      label: 'System & data',
      items: [{ id: 'updates', label: 'Updates', description: 'OS version & policy', href: '/apps/settings/updates' }]
    }
  ];

  const navStore = writable<SettingsNavState>({ sections: navSections, isSplit: true });
  setContext('settings-nav', navStore);

  let isSplit = true;

  onMount(() => {
    const mq = window.matchMedia('(min-width: 768px)');
    const apply = () => {
      isSplit = mq.matches;
      navStore.set({ sections: navSections, isSplit });
    };
    apply();
    mq.addEventListener('change', apply);
    return () => mq.removeEventListener('change', apply);
  });

  $: navStore.set({ sections: navSections, isSplit });
</script>

<svelte:head>
  <title>Settings · Piccolo OS</title>
</svelte:head>

<div class="settings-surface">
  <header class="settings-header">
    <div>
      <p class="meta">Control</p>
      <h1 class="title">Settings</h1>
      <p class="hint">Smart defaults, total control — split view on desktop, stack on mobile.</p>
    </div>
    <div class="actions">
      <Button variant="ghost" size="compact" on:click={() => goto('/')}>Back to Stage</Button>
    </div>
  </header>

  <div class="settings-shell" data-mode={isSplit ? 'split' : 'stack'}>
    {#if isSplit}
      <aside class="nav-pane">
        <SettingsNav sections={navSections} />
      </aside>
      <section class="detail-pane">
        <slot />
      </section>
    {:else}
      <section class="detail-pane">
        <slot />
      </section>
    {/if}
  </div>
</div>

<style>
  .settings-surface {
    max-width: 1200px;
    margin: 0 auto;
    padding: 18px 16px 32px;
  }

  .settings-header {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 16px;
  }

  .meta {
    margin: 0;
    text-transform: uppercase;
    letter-spacing: 0.18em;
    font-size: 11px;
    color: rgb(var(--sys-ink-muted));
  }

  .title {
    margin: 4px 0;
    font-size: 26px;
    font-weight: 700;
  }

  .hint {
    margin: 0;
    color: rgb(var(--sys-ink-muted));
    font-size: 14px;
  }

  .settings-shell {
    display: grid;
    grid-template-columns: 1fr;
    gap: 16px;
  }

  .settings-shell[data-mode='split'] {
    grid-template-columns: 280px 1fr;
    align-items: start;
  }

  .nav-pane {
    position: sticky;
    top: 16px;
  }

  .detail-pane {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  @media (max-width: 767px) {
    .settings-header {
      flex-direction: column;
      align-items: flex-start;
    }
  }
</style>
