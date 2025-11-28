<script lang="ts">
  import { goto } from '$app/navigation';
  import { getContext, onDestroy, onMount } from 'svelte';
  import Button from '$lib/components/ui/Button.svelte';
  import SettingCard from '$lib/components/settings/SettingCard.svelte';
  import { acknowledgeStaleness, fetchRecoveryKeyStatus, generateRecoveryKey } from '$lib/api/setup';
  import { platformState, platformController } from '$lib/stores/platform';
  import { toasts } from '$lib/stores/toasts';
  import type { Writable } from 'svelte/store';
  import type { SettingsNavState } from '../types';
  import type { SettingsActivityContext } from '../activityContext';
  import { settingsActivityKey } from '../activityContext';

  const navStore = getContext<Writable<SettingsNavState> | null>('settings-nav');
  const activity = getContext<SettingsActivityContext | null>(settingsActivityKey);
  let navState: SettingsNavState | null = null;
  const unsub = navStore?.subscribe((value) => (navState = value));
  onDestroy(() => unsub?.());

  let recoveryStatusLoading = false;
  let recoveryStatusError = '';
  let recoveryPresent = false;
  let recoveryStale = false;
  let generatedWords: string[] = [];
  $: userLabel = $platformState.session?.user ?? 'admin';

  const loadRecoveryStatus = async () => {
    recoveryStatusLoading = true;
    recoveryStatusError = '';
    try {
      const status = await fetchRecoveryKeyStatus();
      recoveryPresent = Boolean(status.present);
      recoveryStale = Boolean(status.stale);
    } catch (error) {
      recoveryStatusError = (error as Error)?.message ?? 'Unable to load recovery key status.';
    } finally {
      recoveryStatusLoading = false;
    }
  };

  const handleGenerate = async () => {
    recoveryStatusError = '';
    try {
      generatedWords = await generateRecoveryKey();
      recoveryPresent = true;
      recoveryStale = false;
      toasts.push({ message: 'New recovery key generated. Save it offline.', variant: 'success', timeout: 4200 });
    } catch (error) {
      recoveryStatusError = (error as Error)?.message ?? 'Failed to generate recovery key.';
      toasts.push({ message: recoveryStatusError, variant: 'error', timeout: 5000 });
    }
  };

  const handleAcknowledge = async () => {
    try {
      await acknowledgeStaleness({ password: true, recovery: true });
      recoveryStale = false;
      toasts.push({ message: 'Staleness acknowledged.', variant: 'info', timeout: 3200 });
    } catch (error) {
      toasts.push({ message: (error as Error)?.message ?? 'Unable to acknowledge.', variant: 'error', timeout: 4200 });
    }
  };

  onMount(async () => {
    await platformController.refreshSession();
    await loadRecoveryStatus();
  });
</script>

{#if navState && !navState.isSplit}
  <div class="mb-3">
    <Button
      variant="ghost"
      size="compact"
      on:click={() => (activity?.setActive ? activity.setActive(null) : goto('/apps/settings'))}
    >
      Back
    </Button>
  </div>
{/if}

<div class="card-stack">
  <div class="grid gap-6 md:grid-cols-2">
    <SettingCard title="Password" description="Rotate the admin password">
      <p class="text-sm text-muted">
        Password rotation will move to server-backed flows. For now, use the recovery reset to rotate credentials, then sign back in.
      </p>
      <div class="flex gap-2 mt-3">
        <Button variant="primary" on:click={() => goto('/password-recovery')}>Reset via recovery</Button>
        <Button variant="secondary" on:click={() => goto('/unlock')}>Go to unlock</Button>
      </div>
    </SettingCard>

    <SettingCard title="Session" description="Authentication context">
      <ul class="text-sm text-muted space-y-2">
        <li><strong>User:</strong> {userLabel}</li>
        <li><strong>Authenticated:</strong> {$platformState.session?.authenticated ? 'Yes' : 'No'}</li>
        <li>
          <strong>Volumes locked:</strong>
          {$platformState.session?.volumesLocked === false ? 'Unlocked' : 'Locked'}
        </li>
        {#if $platformState.session?.expiresAt}
          <li><strong>Session expires:</strong> {$platformState.session?.expiresAt}</li>
        {/if}
      </ul>
    </SettingCard>
  </div>

<SettingCard
  title="Recovery key"
  description="Generate and save the 24-word key. Piccolo requires acknowledgement when the key is used."
  actions={true}
>
  <svelte:fragment slot="actions">
    <Button variant="secondary" size="compact" on:click={loadRecoveryStatus} loading={recoveryStatusLoading}>Refresh</Button>
    <Button variant="primary" size="compact" on:click={handleGenerate} disabled={recoveryStatusLoading}>
      {recoveryStatusLoading ? 'Preparing…' : 'Generate'}
    </Button>
  </svelte:fragment>

  {#if recoveryStatusError}
    <p class="text-sm text-red-600">{recoveryStatusError}</p>
  {/if}

  <div class="status-row">
    <span class="chip" data-tone={recoveryPresent ? 'ok' : 'warn'}>
      {recoveryPresent ? 'Key present' : 'Missing'}
    </span>
    {#if recoveryStale}
      <span class="chip" data-tone="warn">Stale — acknowledge</span>
    {/if}
    <Button variant="ghost" size="compact" on:click={handleAcknowledge} disabled={!recoveryStale}>
      Acknowledge staleness
    </Button>
  </div>

  {#if generatedWords.length}
    <div class="words">
      {#each generatedWords as word, index}
        <div class="word-chip">
          <span class="index">{index + 1}</span>
          <span>{word}</span>
        </div>
      {/each}
    </div>
    <p class="text-xs text-muted mt-2">Copy these words and store them offline. Regenerating replaces the previous key.</p>
  {:else if recoveryPresent}
    <p class="text-sm text-muted">Recovery key already exists. Regenerate only if you have rotated credentials or suspect compromise.</p>
  {:else}
    <p class="text-sm text-muted">No recovery key found. Generate one to avoid losing access.</p>
  {/if}
</SettingCard>
</div>

<style>
  .status-row {
    display: flex;
    gap: 8px;
    align-items: center;
    margin: 8px 0;
    flex-wrap: wrap;
  }

  .chip {
    display: inline-flex;
    align-items: center;
    padding: 6px 10px;
    border-radius: 12px;
    font-size: 12px;
    font-weight: 600;
  }

  .chip[data-tone='ok'] {
    background: rgb(var(--sys-success) / 0.14);
    color: #065f46;
  }

  .chip[data-tone='warn'] {
    background: rgb(var(--sys-warning) / 0.16);
    color: #92400e;
  }

  .words {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
    gap: 8px;
    margin-top: 10px;
  }

  .word-chip {
    border: 1px solid var(--card-border);
    border-radius: 12px;
    padding: 8px 10px;
    display: flex;
    gap: 8px;
    align-items: center;
    background: rgba(var(--sys-surface-variant), 0.9);
  }

  .index {
    width: 24px;
    height: 24px;
    border-radius: 999px;
    background: rgba(var(--sys-accent-rgb), 0.12);
    display: inline-flex;
    align-items: center;
    justify-content: center;
    font-size: 12px;
    font-weight: 600;
    color: rgb(var(--sys-ink));
  }
</style>
