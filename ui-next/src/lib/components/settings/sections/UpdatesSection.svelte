<script lang="ts">
  import { getContext, onDestroy } from "svelte";
  import { goto } from "$app/navigation";
  import { createQuery, useQueryClient } from "@tanstack/svelte-query";
  import SettingCard from "$lib/components/settings/SettingCard.svelte";
  import SettingsSkeleton from "$lib/components/settings/SettingsSkeleton.svelte";
  import Button from "$lib/components/ui/Button.svelte";
  import Badge from "$lib/components/ui/Badge.svelte";
  import {
    checkForUpdates,
    fetchUpdateInfo,
    setAutoUpdate,
  } from "$lib/api/updates";
  import { toasts } from "$lib/stores/toasts";
  import type { Writable } from "svelte/store";
  import type { SettingsNavState } from "../types";
  import type { SettingsActivityContext } from "../activityContext";
  import { settingsActivityKey } from "../activityContext";

  const queryClient = useQueryClient();
  const query = createQuery(() => ({
    queryKey: ["updateInfo"],
    queryFn: fetchUpdateInfo,
    staleTime: 1000 * 60 * 5, // 5 minutes
  }));

  let checking = false;
  let heroTone: "ok" | "warn" | "error" = "ok";

  const navStore = getContext<Writable<SettingsNavState> | null>(
    "settings-nav",
  );
  const activity = getContext<SettingsActivityContext | null>(
    settingsActivityKey,
  );
  let navState: SettingsNavState | null = null;
  const unsub = navStore?.subscribe((value) => (navState = value));
  onDestroy(() => unsub?.());

  const handleCheck = async () => {
    checking = true;
    try {
      const info = await checkForUpdates();
      await queryClient.invalidateQueries({ queryKey: ["updateInfo"] });
      const message = info.available
        ? `Update ${info.latestVersion} available.`
        : "Already up to date.";
      toasts.push({
        message,
        variant: info.available ? "success" : "info",
        timeout: 3200,
      });
    } catch (error) {
      toasts.push({
        message: (error as Error)?.message ?? "Failed to check updates.",
        variant: "error",
        timeout: 4200,
      });
    } finally {
      checking = false;
    }
  };

  const toggleAuto = async () => {
    if (!query.data) return;
    const next = !query.data.autoUpdate;
    try {
      await setAutoUpdate(next);
      await queryClient.invalidateQueries({ queryKey: ["updateInfo"] });
      toasts.push({
        message: `Auto-update ${next ? "enabled" : "disabled"}.`,
        variant: "success",
        timeout: 2600,
      });
    } catch (error) {
      toasts.push({
        message: (error as Error)?.message ?? "Failed to update policy.",
        variant: "error",
        timeout: 4200,
      });
    }
  };
</script>

{#if navState && !navState.isSplit}
  <div class="mb-3">
    <Button
      variant="ghost"
      size="compact"
      on:click={() =>
        activity?.setActive ? activity.setActive(null) : goto("/apps/settings")}
    >
      Back
    </Button>
  </div>
{/if}

{#if query.data}
  <div class="card-stack">
    <div class="grid gap-6 md:grid-cols-2">
      <SettingCard title="Current state" description="OS version and channel">
        <div
          class="grid grid-cols-[auto_1fr] items-center gap-x-4 gap-y-3 text-sm"
        >
          <span class="font-medium text-ink">Current:</span>
          <span class="text-muted font-mono">{query.data.currentVersion}</span>

          <span class="font-medium text-ink">Latest:</span>
          <span class="text-muted font-mono">{query.data.latestVersion}</span>

          <span class="font-medium text-ink">Channel:</span>
          <div><Badge variant="neutral">{query.data.channel}</Badge></div>

          {#if query.data.lastChecked}
            <span class="font-medium text-ink">Last checked:</span>
            <span class="text-muted"
              >{new Date(query.data.lastChecked).toLocaleString()}</span
            >
          {/if}
        </div>

        <div class="mt-6 flex gap-2">
          <Button variant="primary" on:click={handleCheck} loading={checking}
            >Check for updates</Button
          >
          <Button variant="secondary" disabled={!query.data.available}
            >Apply update</Button
          >
        </div>

        {#if query.data.available}
          <div
            class="mt-4 flex items-center gap-2 rounded-lg bg-surface-variant/30 p-3"
          >
            <Badge variant="success">Ready</Badge>
            <p class="text-sm text-ink">
              Update {query.data.latestVersion} is ready to install.
            </p>
          </div>
        {/if}
      </SettingCard>

      <SettingCard title="Policy" description="Auto-update and channel">
        <div class="flex items-center gap-3">
          <label class="switch">
            <input
              type="checkbox"
              checked={query.data.autoUpdate}
              on:change={toggleAuto}
            />
            <span class="slider" aria-hidden="true"></span>
          </label>
          <div>
            <p class="text-sm font-semibold text-ink">Auto-update</p>
            <p class="text-xs text-muted">
              Optimistic toggle; reverts and toasts on failure.
            </p>
          </div>
        </div>
        <p class="text-xs text-muted mt-3">
          Channel selection will be wired to the backend. Default is stable.
        </p>
      </SettingCard>
    </div>
  </div>
{:else if query.isLoading}
  <SettingsSkeleton />
{:else if query.isError}
  <div class="card-stack">
    <SettingCard
      title="Updates unavailable"
      description="Error loading update info."
    >
      <div class="flex gap-2">
        <Button
          variant="primary"
          size="compact"
          on:click={() => query.refetch()}>Retry</Button
        >
      </div>
    </SettingCard>
  </div>
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
    content: "";
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
