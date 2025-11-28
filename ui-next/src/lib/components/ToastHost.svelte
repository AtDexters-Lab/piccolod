<script lang="ts">
  import { fade, fly } from 'svelte/transition';
  import { toasts, type Toast } from '$lib/stores/toasts';

  const toneClass = (toast: Toast) => {
    switch (toast.variant) {
      case 'success':
        return 'bg-emerald-50 text-emerald-800 border-emerald-100 dark:bg-emerald-500/15 dark:text-emerald-100 dark:border-emerald-400/25';
      case 'warning':
        return 'bg-amber-50 text-amber-800 border-amber-100 dark:bg-amber-500/15 dark:text-amber-100 dark:border-amber-400/25';
      case 'error':
        return 'bg-red-50 text-red-800 border-red-100 dark:bg-red-500/15 dark:text-red-100 dark:border-red-400/25';
      default:
        return 'bg-white/90 text-ink border-white/60 dark:bg-slate-800/90 dark:text-white dark:border-white/10';
    }
  };
</script>

<div class="toast-stack" aria-live="polite" aria-atomic="true">
  {#each $toasts as toast (toast.id)}
    <article
      class={`toast-card border shadow-sm backdrop-blur-xl ${toneClass(toast)}`}
      in:fly={{ x: 12, duration: 150 }}
      out:fade={{ duration: 140 }}
      role="status"
    >
      <p class="text-sm font-medium leading-snug">{toast.message}</p>
      <button class="dismiss" on:click={() => toasts.remove(toast.id)} aria-label="Dismiss notification">✕</button>
    </article>
  {/each}
</div>

<style>
  .toast-stack {
    position: fixed;
    inset-inline: 0;
    bottom: 22px;
    display: flex;
    flex-direction: column;
    gap: 10px;
    align-items: center;
    pointer-events: none;
    z-index: 120;
  }

  .toast-card {
    pointer-events: auto;
    width: min(460px, calc(100% - 24px));
    border-radius: 14px;
    padding: 12px 14px 12px 14px;
    display: grid;
    grid-template-columns: 1fr auto;
    gap: 8px;
  }

  .dismiss {
    background: transparent;
    border: none;
    color: inherit;
    font-size: 14px;
    padding: 4px;
    border-radius: 10px;
    cursor: pointer;
  }

  .dismiss:hover {
    background: rgba(0, 0, 0, 0.05);
  }

  :global([data-theme='dark']) .dismiss:hover {
    background: rgba(255, 255, 255, 0.08);
  }
</style>
