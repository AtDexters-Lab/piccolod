<script lang="ts">
  import { goto } from '$app/navigation';
  import { page } from '$app/stores';
  import { getContext } from 'svelte';
  import type { SettingsNavSection } from './types';
  import type { SettingsActivityContext } from './activityContext';
  import { settingsActivityKey } from './activityContext';

  export let sections: SettingsNavSection[] = [];
  export let activeId: string | null = null;

  const activity = getContext<SettingsActivityContext | null>(settingsActivityKey);

  const isActive = (itemId: string, href: string, currentPath: string) => {
    if (activeId) return activeId === itemId;
    return currentPath === href || currentPath.startsWith(`${href}/`);
  };

  const handleClick = (itemId: string, href: string, event: MouseEvent) => {
    event.preventDefault();
    if (activity?.setActive) {
      activity.setActive(itemId);
      return;
    }
    goto(href);
  };
</script>

<nav aria-label="Settings categories" class="nav-shell">
  {#each sections as section (section.id)}
    <div class="section">
      <p class="section-label">{section.label}</p>
      <ul class="section-list">
        {#each section.items as item (item.id)}
          {#if $page}
            {#key `${$page.url.pathname}-${activeId ?? 'na'}`}
              {#if isActive(item.id, item.href, $page.url.pathname)}
                <li>
                  <a
                    class="item active"
                    href={item.href}
                    aria-current="page"
                    on:click|preventDefault={(event) => handleClick(item.id, item.href, event)}
                  >
                    <div>
                      <p class="item-title">{item.label}</p>
                    </div>
                    <span aria-hidden="true">›</span>
                  </a>
                </li>
              {:else}
                <li>
                  <a class="item" href={item.href} on:click={(event) => handleClick(item.id, item.href, event)}>
                    <div>
                      <p class="item-title">{item.label}</p>
                    </div>
                    <span aria-hidden="true">›</span>
                  </a>
                </li>
              {/if}
            {/key}
          {/if}
          {/each}
        </ul>
      </div>
    {/each}
  </nav>

<style>
  .nav-shell {
    display: flex;
    flex-direction: column;
    gap: 18px;
    position: sticky;
    top: 0;
    max-height: calc(100vh - 32px);
  }

  .section {
    background: var(--card-bg);
    border: 1px solid var(--card-border);
    border-radius: 18px;
    padding: 18px 16px 12px;
    box-shadow: var(--shadow-soft);
  }

  .section-label {
    font-size: 11px;
    letter-spacing: 0.18em;
    text-transform: uppercase;
    color: rgb(var(--sys-ink-muted));
    margin: 0 0 8px;
  }

  .section-list {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .item {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    padding: 12px 14px;
    border-radius: 12px;
    border: 1px solid transparent;
    color: rgb(var(--sys-ink));
    text-decoration: none;
    background: transparent;
    transition: background 140ms var(--motion-ease-standard), border-color 140ms var(--motion-ease-standard), box-shadow 140ms var(--motion-ease-standard);
    border-radius: 999px;
  }

  .item:hover {
    background: rgba(var(--sys-ink), 0.06);
    border-color: transparent;
  }

  .item:focus-visible {
    outline: none;
    box-shadow: none;
    background: rgba(var(--sys-accent-rgb), 0.08);
  }

  .item-title {
    font-weight: 600;
    font-size: 14px;
  }

  .item.active {
    background: rgba(var(--sys-accent-rgb), 0.12);
    border-color: transparent;
    box-shadow: 0 10px 24px rgba(var(--sys-accent-rgb), 0.12);
    color: rgb(var(--sys-accent-rgb));
    font-weight: 700;
  }
</style>
