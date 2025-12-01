<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import Button from '$lib/components/ui/Button.svelte';
  import { logout } from '$lib/api/setup';
  import { resetCsrfToken, primeCsrfToken } from '$lib/api/http';
  import { platformController } from '$lib/stores/platform';
  import { goto } from '$app/navigation';

  const dispatch = createEventDispatcher();

  export let bindHeaderEl: HTMLElement | null = null;

  let loggingOut = false;

  async function handleLogout() {
    loggingOut = true;
    try {
      await primeCsrfToken();
      await logout();
      resetCsrfToken();
      await platformController.refreshSession();
      goto('/login');
    } catch {
      // ignore errors during logout, just force redirect if needed or let user retry
    } finally {
      loggingOut = false;
    }
  }
</script>

<header 
  class="fixed left-0 right-0 top-0 z-50 px-6 py-4 sm:px-10 lg:px-16" 
  bind:this={bindHeaderEl}
>
  <div class="flex items-center justify-between rounded-2xl border border-white/40 bg-white/80 px-4 py-3 shadow-md backdrop-blur-2xl dark:border-white/10 dark:bg-slate-900/80">
    <div class="flex items-center gap-3">
      <div class="flex h-11 w-11 items-center justify-center rounded-2xl bg-gradient-to-br from-indigo-500 to-blue-600 text-base font-semibold text-white shadow-md">
        P
      </div>
      <div class="leading-tight">
        <p class="text-[11px] uppercase tracking-[0.16em] text-muted">Piccolo OS</p>
        <p class="text-sm font-semibold text-ink">Digital Sanctuary</p>
      </div>
    </div>
    <div class="flex items-center gap-3">
      <label class="relative hidden md:block">
        <input
          class="h-10 w-64 rounded-full border border-white/50 bg-white/70 px-10 text-sm text-ink placeholder:text-muted shadow-sm backdrop-blur-xl focus:outline-none focus:ring-2 focus:ring-blue-200 dark:border-white/10 dark:bg-slate-800/80"
          placeholder="Search apps or settings"
          type="search"
        />
        <span class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-muted">⌘K</span>
      </label>
      <div class="hidden items-center gap-2 rounded-full border border-white/50 bg-white/70 px-3 py-2 text-xs font-medium text-emerald-700 shadow-sm backdrop-blur-xl dark:border-white/10 dark:bg-slate-800/80 dark:text-emerald-200 sm:flex">
        <span class="inline-flex h-2 w-2 rounded-full bg-emerald-500" aria-hidden="true"></span>
        System healthy
      </div>
      <div class="flex gap-2">
        <Button variant="ghost" size="compact" on:click={handleLogout} loading={loggingOut}>
          Sign out
        </Button>
        <Button variant="primary" on:click={() => dispatch('openDrawer')}>
          Open Drawer
        </Button>
      </div>
    </div>
  </div>
</header>
