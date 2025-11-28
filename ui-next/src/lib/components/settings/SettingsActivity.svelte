<script lang="ts">
  import { onDestroy, onMount, setContext } from 'svelte';
  import { writable } from 'svelte/store';
  import SettingsNav from './SettingsNav.svelte';
  import type { SettingsNavSection, SettingsNavState } from './types';
  import Button from '$lib/components/ui/Button.svelte';
  import OverviewSection from './sections/OverviewSection.svelte';
  import ProfileSection from './sections/ProfileSection.svelte';
  import AppearanceSection from './sections/AppearanceSection.svelte';
  import RemoteAccessSection from './sections/RemoteAccessSection.svelte';
  import UpdatesSection from './sections/UpdatesSection.svelte';
  import { settingsActivityKey, type SettingsActivityContext } from './activityContext';
  import { fade, fly } from 'svelte/transition';

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
  const activityContext: SettingsActivityContext = {
    setActive: (id) => setActive(id),
    getActive: () => activeId,
    isEmbedded: true
  };
  setContext(settingsActivityKey, activityContext);

  let isSplit = true;
  let activeId: string | null = null;

  const setActive = (id: string | null) => {
    activeId = id;
  };

  onMount(() => {
    const mq = window.matchMedia('(min-width: 768px)');
    const apply = () => {
      isSplit = mq.matches;
      navStore.set({ sections: navSections, isSplit });
      if (isSplit) {
        activeId = activeId ?? 'profile';
      } else {
        activeId = null;
      }
    };
    apply();
    mq.addEventListener('change', apply);
    return () => mq.removeEventListener('change', apply);
  });

  onDestroy(() => {
    navStore.set({ sections: navSections, isSplit: true });
  });
</script>

<div class="settings-surface">
  <div class="settings-shell" data-mode={isSplit ? 'split' : 'stack'}>
    {#if isSplit}
      <aside class="nav-pane">
        <div class="nav-inner">
          <SettingsNav sections={navSections} activeId={activeId} />
        </div>
      </aside>
      <section class="detail-pane">
        <div class="detail-inner">
          {#key activeId ?? 'overview'}
            <div in:fly={{ y: 8, duration: 140 }} out:fly={{ y: 8, duration: 120 }}>
              {#if activeId === 'profile'}
                <ProfileSection />
              {:else if activeId === 'appearance'}
                <AppearanceSection />
              {:else if activeId === 'remote'}
                <RemoteAccessSection />
              {:else if activeId === 'updates'}
                <UpdatesSection />
              {:else}
                <OverviewSection />
              {/if}
            </div>
          {/key}
        </div>
      </section>
    {:else}
      {#if activeId === null}
        <OverviewSection />
        <div class="stacked-list">
          {#each navSections as section (section.id)}
            <div class="stacked-section">
              <p class="stacked-label">{section.label}</p>
              <div class="stacked-items">
                {#each section.items as item (item.id)}
                  <button class="stacked-item" on:click={() => setActive(item.id)}>
                    <div>
                      <p class="item-title">{item.label}</p>
                      {#if item.description}<p class="item-desc">{item.description}</p>{/if}
                    </div>
                    <span aria-hidden="true">›</span>
                  </button>
                {/each}
              </div>
            </div>
          {/each}
        </div>
      {:else if activeId === 'profile'}
        <ProfileSection />
      {:else if activeId === 'appearance'}
        <AppearanceSection />
      {:else if activeId === 'remote'}
        <RemoteAccessSection />
      {:else if activeId === 'updates'}
        <UpdatesSection />
      {/if}
    {/if}
  </div>
</div>

<style>
  .settings-surface {
    height: 100%;
    display: flex;
    flex-direction: column;
  }

  .settings-shell {
    display: grid;
    grid-template-columns: 1fr;
    height: 100%;
    min-height: 0;
  }

  .settings-shell[data-mode='split'] {
    grid-template-columns: 260px 1fr;
    align-items: stretch;
  }

  .nav-pane {
    overflow: visible;
    height: 100%;
    min-height: 0;
    padding-right: 8px;
  }

  .nav-inner {
    height: 100%;
    min-height: 0;
    overflow-y: auto;
    padding: 28px 28px 36px 28px;
    display: flex;
    flex-direction: column;
    gap: var(--panel-gap);
  }

  .detail-pane {
    display: flex;
    flex-direction: column;
    gap: 18px;
    overflow: visible;
    min-height: 0;
    padding-right: 8px;
  }

  .detail-inner {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 28px 28px 48px 28px;
    display: flex;
    flex-direction: column;
    gap: var(--panel-gap);
  }

  .stacked-list {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .stacked-section {
    border: 1px solid var(--card-border);
    border-radius: 12px;
    padding: 10px;
    background: rgba(var(--sys-surface-variant), 0.9);
  }

  .stacked-label {
    font-size: 12px;
    letter-spacing: 0.16em;
    text-transform: uppercase;
    color: rgb(var(--sys-ink-muted));
    margin-bottom: 6px;
  }

  .stacked-items {
    display: grid;
    gap: 8px;
  }

  .stacked-item {
    width: 100%;
    padding: 10px 12px;
    border-radius: 10px;
    border: 1px solid transparent;
    background: transparent;
    display: flex;
    align-items: center;
    justify-content: space-between;
  }

  .stacked-item:hover {
    background: rgba(var(--sys-accent-rgb), 0.06);
    border-color: rgba(var(--sys-accent-rgb), 0.12);
  }

  .item-title {
    font-weight: 600;
    font-size: 14px;
  }

  .item-desc {
    font-size: 12px;
    color: rgb(var(--sys-ink-muted));
    margin-top: 2px;
  }

  /* no additional mobile tweaks needed post-header removal */
</style>
