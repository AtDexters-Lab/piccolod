<script lang="ts">
  import { goto } from '$app/navigation';
  import { getContext, onDestroy } from 'svelte';
  import type { Writable } from 'svelte/store';
  import SettingCard from '$lib/components/settings/SettingCard.svelte';
  import type { SettingsNavState } from '../types';
  import type { SettingsActivityContext } from '../activityContext';
  import { settingsActivityKey } from '../activityContext';

  const navStore = getContext<Writable<SettingsNavState> | null>('settings-nav');
  const activity = getContext<SettingsActivityContext | null>(settingsActivityKey);
  let navState: SettingsNavState = { sections: [], isSplit: false };
  const unsub = navStore?.subscribe((value) => (navState = value));
  onDestroy(() => unsub?.());

  const quickLinks = [
    { id: 'profile', label: 'Profile', href: '/apps/settings/profile', desc: 'Password and recovery key' },
    { id: 'remote', label: 'Remote access', href: '/apps/settings/remote-access', desc: 'piccolo.link domain' },
    { id: 'updates', label: 'Updates', href: '/apps/settings/updates', desc: 'OS version and policy' }
  ];
</script>

<div class="grid gap-4 md:grid-cols-2">
  <SettingCard title="Where to begin" description="Jump into a category">
    <div class="grid gap-3">
      {#each quickLinks as item}
        <button
          class="link-row"
          on:click={() => {
            if (activity?.setActive) {
              activity.setActive(item.id);
            } else {
              goto(item.href);
            }
          }}
        >
          <div>
            <p class="link-label">{item.label}</p>
            <p class="link-desc">{item.desc}</p>
          </div>
          <span aria-hidden="true">›</span>
        </button>
      {/each}
    </div>
  </SettingCard>

  <SettingCard
    title="Mobile stack navigation"
    description="On small screens, the list comes first. Tapping a category opens the detail view over the list."
  >
    <p class="text-sm text-muted">
      Return using the browser back gesture or the “Back” button shown on each detail screen. Desktop keeps the persistent sidebar
      for quick jumping between clusters.
    </p>
  </SettingCard>
</div>

{#if !navState.isSplit && navState.sections.length}
  <SettingCard title="Categories" description="Choose a section to configure">
    <div class="stacked-list">
      {#each navState.sections as section (section.id)}
        <div class="stacked-section">
          <p class="stacked-label">{section.label}</p>
          <div class="stacked-items">
            {#each section.items as item (item.id)}
              <button
                class="stacked-item"
                on:click={() => {
                  if (activity?.setActive) {
                    activity.setActive(item.id);
                  } else {
                    goto(item.href);
                  }
                }}
              >
                <div>
                  <p class="link-label">{item.label}</p>
                  {#if item.description}<p class="link-desc">{item.description}</p>{/if}
                </div>
                <span aria-hidden="true">›</span>
              </button>
            {/each}
          </div>
        </div>
      {/each}
    </div>
  </SettingCard>
{/if}

<style>
  .link-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    padding: 10px 12px;
    border-radius: 12px;
    border: 1px solid var(--card-border);
    background: rgba(var(--sys-surface-variant), 0.9);
    box-shadow: var(--elev-1);
    text-align: left;
  }

  .link-label {
    font-weight: 600;
    color: rgb(var(--sys-ink));
  }

  .link-desc {
    font-size: 13px;
    color: rgb(var(--sys-ink-muted));
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

  .stacked-item:hover,
  .link-row:hover {
    background: rgba(var(--sys-accent-rgb), 0.06);
    border-color: rgba(var(--sys-accent-rgb), 0.12);
  }
</style>
