<script lang="ts">
  import { getContext, onDestroy, onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import SettingCard from '$lib/components/settings/SettingCard.svelte';
  import Button from '$lib/components/ui/Button.svelte';
  import { checkForUpdates, fetchUpdateInfo, setAutoUpdate, type UpdateInfo } from '$lib/api/updates';
  import { toasts } from '$lib/stores/toasts';
  import type { Writable } from 'svelte/store';
  import type { SettingsNavState } from '../types';
  import type { SettingsActivityContext } from '../activityContext';
  import { settingsActivityKey } from '../activityContext';

  let info: UpdateInfo | null = null;
  let checking = false;
  let heroTone: 'ok' | 'warn' | 'error' = 'ok';
  let loadError = '';

  const navStore = getContext<Writable<SettingsNavState> | null>('settings-nav');
  const activity = getContext<SettingsActivityContext | null>(settingsActivityKey);
  let navState: SettingsNavState | null = null;
  const unsub = navStore?.subscribe((value) => (navState = value));
  onDestroy(() => unsub?.());

  const loadInfo = async () => {
    try {
      info = await fetchUpdateInfo();
      loadError = '';
    } catch (error) {
      loadError = (error as Error)?.message ?? 'Unable to load updates.';
      info = null;
      toasts.push({ message: loadError, variant: 'error', timeout: 4200 });
    }
  };

  const handleCheck = async () => {
    checking = true;
    try {
      info = await checkForUpdates();
      const message = info.available ? `Update ${info.latestVersion} available.` : 'Already up to date.';
      toasts.push({ message, variant: info.available ? 'success' : 'info', timeout: 3200 });
    } catch (error) {
      toasts.push({ message: (error as Error)?.message ?? 'Failed to check updates.', variant: 'error', timeout: 4200 });
    } finally {
      checking = false;
    }
  };

  const toggleAuto = async () => {
    if (!info) return;
    const next = !info.autoUpdate;
    const prev = info;
    info = { ...info, autoUpdate: next };
    try {
      info = await setAutoUpdate(next);
      toasts.push({ message: `Auto-update ${next ? 'enabled' : 'disabled'} (optimistic).`, variant: 'success', timeout: 2600 });
    } catch (error) {
      info = prev;
      toasts.push({ message: (error as Error)?.message ?? 'Failed to update policy.', variant: 'error', timeout: 4200 });
    }
  };

  onMount(loadInfo);

  $: heroTone = info?.available ? 'warn' : 'ok';
  $: heroPill = info ? `${info.currentVersion} · ${info.channel} channel` : 'Loading…';
</script>

{#if navState && !navState.isSplit}
  <div class="mb-3">
    <Button variant="ghost" size="compact" on:click={() => (activity?.setActive ? activity.setActive(null) : goto('/apps/settings'))}>
      Back
    </Button>
  </div>
{/if}

{#if info}
  <div class="card-stack">
    <div class="grid gap-6 md:grid-cols-2">
      <SettingCard title="Current state" description="OS version and channel">
        <ul class="text-sm text-muted space-y-2">
          <li><strong>Current:</strong> {info.currentVersion}</li>
          <li><strong>Latest:</strong> {info.latestVersion}</li>
          <li><strong>Channel:</strong> {info.channel}</li>
          {#if info.lastChecked}<li><strong>Last checked:</strong> {new Date(info.lastChecked).toLocaleString()}</li>{/if}
        </ul>
        <div class="flex gap-2 mt-3">
          <Button variant="primary" on:click={handleCheck} loading={checking}>Check for updates</Button>
          <Button variant="secondary" disabled={!info.available}>Apply update</Button>
        </div>
        {#if info.available}
          <p class="text-sm text-ink mt-2">Update {info.latestVersion} is ready. Applying will reboot.</p>
        {/if}
      </SettingCard>

      <SettingCard title="Policy" description="Auto-update and channel">
        <div class="flex items-center gap-3">
          <label class="switch">
            <input type="checkbox" checked={info.autoUpdate} on:change={toggleAuto} />
            <span class="slider" aria-hidden="true"></span>
          </label>
          <div>
            <p class="text-sm font-semibold text-ink">Auto-update</p>
            <p class="text-xs text-muted">Optimistic toggle; reverts and toasts on failure.</p>
          </div>
        </div>
        <p class="text-xs text-muted mt-3">Channel selection will be wired to the backend. Default is stable.</p>
      </SettingCard>
    </div>
  </div>
{:else}
  {#if loadError}
    <div class="card-stack">
      <SettingCard title="Updates unavailable" description={loadError}>
        <div class="flex gap-2">
          <Button variant="primary" size="compact" on:click={loadInfo}>Retry</Button>
        </div>
      </SettingCard>
    </div>
  {:else}
    <p class="text-sm text-muted">Loading update info…</p>
  {/if}
{/if}

<style>
  .switch {
    position: relative;
    display: inline-block;
    width: 48px;
    height: 26px;
  }

  .switch input {
    opacity: 0;
    width: 0;
    height: 0;
  }

  .slider {
    position: absolute;
    cursor: pointer;
    inset: 0;
    background: rgba(var(--sys-ink), 0.16);
    transition: background 140ms var(--motion-ease-standard);
    border-radius: 999px;
  }

  .slider::before {
    position: absolute;
    content: '';
    height: 20px;
    width: 20px;
    left: 3px;
    bottom: 3px;
    background: rgb(var(--sys-surface));
    transition: transform 140ms var(--motion-ease-standard);
    border-radius: 999px;
    box-shadow: var(--elev-1);
  }

  input:checked + .slider {
    background: rgb(var(--sys-accent-rgb));
  }

  input:checked + .slider::before {
    transform: translateX(20px);
  }
</style>
