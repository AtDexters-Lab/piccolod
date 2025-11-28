<script lang="ts">
  import { getContext, onDestroy, onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import SettingCard from '$lib/components/settings/SettingCard.svelte';
  import Button from '$lib/components/ui/Button.svelte';
  import { toasts } from '$lib/stores/toasts';
  import { fetchRemoteAccess, toggleRemoteAccess, updateRemoteDomain, type RemoteAccessState } from '$lib/api/remote';
  import type { Writable } from 'svelte/store';
  import type { SettingsNavState } from '../types';
  import type { SettingsActivityContext } from '../activityContext';
  import { settingsActivityKey } from '../activityContext';

  let remote: RemoteAccessState | null = null;
  let loading = false;
  let domainInput = '';
  let dangerOpen = false;
  let dangerConfirm = '';
  const dangerPhrase = 'disable-remote';
  let tone: 'ok' | 'warn' | 'error' = 'ok';

  const navStore = getContext<Writable<SettingsNavState> | null>('settings-nav');
  const activity = getContext<SettingsActivityContext | null>(settingsActivityKey);
  let navState: SettingsNavState | null = null;
  const unsub = navStore?.subscribe((value) => (navState = value));
  onDestroy(() => unsub?.());

  const loadRemote = async () => {
    loading = true;
    try {
      remote = await fetchRemoteAccess();
      domainInput = remote.domain;
    } catch (error) {
      toasts.push({ message: (error as Error)?.message ?? 'Unable to load remote access.', variant: 'error', timeout: 4200 });
    } finally {
      loading = false;
    }
  };

  const toggleEnabled = async () => {
    if (!remote) return;
    const next = !remote.enabled;
    const previous = { ...remote };
    remote = { ...remote, enabled: next, status: next ? 'connecting' : 'disconnected' };
    try {
      remote = await toggleRemoteAccess(next);
      toasts.push({
        message: next ? 'Remote access enabled (optimistic apply).' : 'Remote access disabled.',
        variant: 'success',
        timeout: 2600
      });
    } catch (error) {
      remote = previous;
      toasts.push({ message: (error as Error)?.message ?? 'Failed to toggle remote access.', variant: 'error', timeout: 4200 });
    }
  };

  const saveDomain = async () => {
    if (!remote) return;
    const previous = { ...remote };
    remote = { ...remote, domain: domainInput, lastError: undefined };
    try {
      remote = await updateRemoteDomain(domainInput);
      toasts.push({ message: 'Domain updated.', variant: 'success', timeout: 2600 });
    } catch (error) {
      const message = (error as Error)?.message ?? 'Unable to update domain.';
      remote = { ...previous, lastError: message, status: 'error' };
      toasts.push({ message, variant: 'error', timeout: 4200 });
    }
  };

  const openDanger = () => {
    dangerOpen = true;
    dangerConfirm = '';
  };

  const confirmDanger = async () => {
    if (dangerConfirm !== dangerPhrase) {
      toasts.push({ message: `Type "${dangerPhrase}" to confirm.`, variant: 'warning', timeout: 3200 });
      return;
    }
    if (!remote) return;
    try {
      remote = await toggleRemoteAccess(false);
      remote = await updateRemoteDomain('');
      domainInput = '';
      toasts.push({ message: 'Remote access disabled and domain cleared.', variant: 'info', timeout: 3200 });
    } catch (error) {
      toasts.push({ message: (error as Error)?.message ?? 'Failed to disable remote access.', variant: 'error', timeout: 4200 });
    } finally {
      dangerOpen = false;
    }
  };

  onMount(loadRemote);

  $: tone = remote?.status === 'error' ? 'error' : remote?.status === 'disconnected' ? 'warn' : 'ok';
</script>

{#if navState && !navState.isSplit}
  <div class="mb-3">
    <Button variant="ghost" size="compact" on:click={() => (activity?.setActive ? activity.setActive(null) : goto('/apps/settings'))}>
      Back
    </Button>
  </div>
{/if}

{#if remote}
  <div class="card-stack">
    <div class="grid gap-6 md:grid-cols-2">
      <SettingCard title="Remote link" description="Reach your Piccolo over the internet" actions={true}>
        <svelte:fragment slot="actions">
          <Button variant="ghost" size="compact" on:click={toggleEnabled} loading={loading}>
            {remote.enabled ? 'Disable' : 'Enable'}
          </Button>
      </svelte:fragment>
      <div class="flex items-center gap-3">
        <span class="chip" data-tone={tone}>
          <span class="dot" aria-hidden="true"></span>
          {remote.status}
        </span>
        {#if remote.endpoint}<code class="text-sm text-ink">{remote.endpoint}</code>{/if}
      </div>
      {#if remote.lastError}
        <p class="text-sm text-red-600 mt-2">{remote.lastError}</p>
      {/if}
      <p class="text-xs text-muted mt-2">Status updates will connect to the real API; current data is stubbed.</p>
    </SettingCard>

    <SettingCard title="Domain" description="piccolo.link or custom domain" actions={true}>
      <svelte:fragment slot="actions">
        <Button variant="primary" size="compact" on:click={saveDomain}>Save</Button>
      </svelte:fragment>
      <label class="text-sm font-medium text-ink" for="domain">Domain</label>
      <input
        id="domain"
        class="input"
        type="text"
        placeholder="myhome.piccolo.link"
        bind:value={domainInput}
        disabled={loading}
      />
      <p class="text-xs text-muted mt-2">Enter a reachable hostname. Validation and DNS checks will use the backend once exposed.</p>
    </SettingCard>
    </div>

    <SettingCard title="Danger zone" description="Disable remote access" actions={true}>
      <svelte:fragment slot="actions">
        <Button variant="secondary" size="compact" on:click={openDanger}>Open</Button>
      </svelte:fragment>
      <p class="text-sm text-muted">
        Destructive actions require typing a confirmation phrase to avoid accidental lockouts, per the “calm control” pattern.
      </p>
    </SettingCard>
  </div>
{:else}
  <p class="text-sm text-muted">Loading remote access…</p>
{/if}

{#if dangerOpen}
  <div class="modal">
    <div class="modal-card" role="dialog" aria-label="Disable remote access">
      <p class="text-sm font-semibold text-ink">Disable remote access</p>
      <p class="text-sm text-muted">Type "{dangerPhrase}" to confirm. This clears the domain and turns off piccolo.link reachability.</p>
      <input class="input mt-2" bind:value={dangerConfirm} placeholder={dangerPhrase} />
      <div class="flex gap-2 justify-end mt-3">
        <Button variant="ghost" size="compact" on:click={() => (dangerOpen = false)}>Cancel</Button>
        <Button variant="primary" size="compact" on:click={confirmDanger}>Disable</Button>
      </div>
    </div>
  </div>
{/if}

<style>
  .chip {
    display: inline-flex;
    align-items: center;
    padding: 6px 10px;
    border-radius: 12px;
    font-size: 12px;
    font-weight: 600;
    gap: 6px;
  }

  .chip[data-tone='ok'] {
    background: rgb(var(--sys-success) / 0.14);
    color: #065f46;
  }

  .chip[data-tone='warn'] {
    background: rgb(var(--sys-warning) / 0.16);
    color: #92400e;
  }

  .chip[data-tone='error'] {
    background: rgb(var(--sys-critical) / 0.16);
    color: #991b1b;
  }

  .dot {
    width: 8px;
    height: 8px;
    border-radius: 999px;
    background: currentColor;
    box-shadow: 0 0 0 6px currentColor / 0.12;
  }

  .input {
    width: 100%;
    border: 1px solid var(--card-border);
    background: rgba(var(--sys-surface-variant), 0.95);
    border-radius: 12px;
    padding: 10px 12px;
    margin-top: 6px;
  }

  .modal {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.35);
    display: grid;
    place-items: center;
    z-index: 120;
    backdrop-filter: blur(4px);
  }

  .modal-card {
    width: min(420px, 90vw);
    background: var(--card-bg);
    border: 1px solid var(--card-border);
    border-radius: 16px;
    padding: 16px;
    box-shadow: var(--shadow-strong);
  }
</style>
