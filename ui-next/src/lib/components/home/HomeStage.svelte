<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import Button from '$lib/components/ui/Button.svelte';
  import { getContext } from 'svelte';

  const { openDrawer } = getContext<{ openDrawer: () => void }>('shell');

  let time = new Date();
  let timer: number;

  onMount(() => {
    timer = window.setInterval(() => {
      time = new Date();
    }, 1000);
  });

  onDestroy(() => {
    if (timer) clearInterval(timer);
  });

  $: hours = time.getHours();
  $: minutes = time.getMinutes().toString().padStart(2, '0');
  $: dateString = time.toLocaleDateString(undefined, { weekday: 'long', month: 'long', day: 'numeric' });
</script>

<div class="mx-auto max-w-4xl pt-12 sm:pt-24 grid gap-6 sm:grid-cols-2 lg:grid-cols-3">
  <!-- Clock Widget -->
  <div class="sm:col-span-2 lg:col-span-1 flex flex-col justify-center p-2 text-ink/90 dark:text-white/90">
    <h1 class="text-6xl font-light tracking-tight font-display">{hours}:{minutes}</h1>
    <p class="text-lg font-medium opacity-80 mt-1">{dateString}</p>
  </div>

  <!-- System Pulse Widget -->
  <article class="backdrop-blur-xl bg-white/40 dark:bg-slate-900/40 border border-white/30 dark:border-white/10 rounded-3xl p-5 shadow-lg transition-transform hover:scale-[1.02]">
    <div class="flex items-center justify-between mb-4">
      <h2 class="text-base font-semibold text-ink dark:text-white">System</h2>
      <span class="inline-flex h-2 w-2 rounded-full bg-emerald-500 shadow-[0_0_8px_rgba(16,185,129,0.6)]"></span>
    </div>
    
    <div class="space-y-3">
      <div class="flex items-center justify-between text-sm">
        <span class="text-ink/70 dark:text-white/70">Status</span>
        <span class="font-medium text-emerald-700 dark:text-emerald-400 bg-emerald-100/50 dark:bg-emerald-500/20 px-2 py-0.5 rounded-full text-xs">Healthy</span>
      </div>
      
      <div class="flex items-center justify-between text-sm">
        <span class="text-ink/70 dark:text-white/70">Storage</span>
        <span class="font-medium text-ink dark:text-white">1.2 TB free</span>
      </div>

      <div class="w-full bg-black/10 dark:bg-white/10 rounded-full h-1.5 mt-1">
        <div class="bg-indigo-500 h-1.5 rounded-full" style="width: 25%"></div>
      </div>
    </div>
  </article>

  <!-- Quick Actions / Memories Widget Placeholder -->
  <article class="backdrop-blur-xl bg-white/40 dark:bg-slate-900/40 border border-white/30 dark:border-white/10 rounded-3xl p-5 shadow-lg flex flex-col justify-between transition-transform hover:scale-[1.02]">
    <div>
      <h2 class="text-base font-semibold text-ink dark:text-white mb-2">Memories</h2>
      <p class="text-sm text-ink/70 dark:text-white/70">3 albums from this week</p>
    </div>
    <div class="mt-4 flex gap-2">
      <div class="h-12 w-12 rounded-xl bg-indigo-100/50 dark:bg-indigo-500/20 border border-white/20"></div>
      <div class="h-12 w-12 rounded-xl bg-purple-100/50 dark:bg-purple-500/20 border border-white/20"></div>
      <div class="h-12 w-12 rounded-xl bg-blue-100/50 dark:bg-blue-500/20 border border-white/20"></div>
    </div>
  </article>
</div>
