<script lang="ts">
  import { preferencesStore } from '$lib/stores/preferences';
  import { fade } from 'svelte/transition';

  $: mode = $preferencesStore.background;
  $: isDark = $preferencesStore.theme === 'dark';
</script>

<div class="wallpaper-layer" aria-hidden="true">
  {#if mode === 'aurora'}
    <div class="aurora-container" transition:fade={{ duration: 800 }}>
      <div class="blob blob-1"></div>
      <div class="blob blob-2"></div>
      <div class="blob blob-3"></div>
    </div>
  {:else if mode === 'midnight'}
    <div class="midnight-container" transition:fade={{ duration: 800 }}>
       <!-- Gradient fallback for now, can be enhanced later -->
    </div>
  {:else}
    <div class="plain-container" transition:fade={{ duration: 400 }}></div>
  {/if}
</div>

<style>
  .wallpaper-layer {
    position: fixed;
    inset: 0;
    z-index: -1;
    overflow: hidden;
    background-color: var(--sys-surface); /* Fallback */
  }

  /* --- Aurora Variant (Animated) --- */
  .aurora-container {
    position: absolute;
    inset: 0;
    background-color: #F6F8FC; /* Base light */
  }
  
  :global([data-theme='dark']) .aurora-container {
    background-color: #0f1219; /* Base dark */
  }

  .blob {
    position: absolute;
    border-radius: 50%;
    filter: blur(80px);
    opacity: 0.85;
    animation: drift 12s infinite alternate ease-in-out;
  }

  /* Light Mode Blobs - Bolder Colors */
  .blob-1 {
    top: -10%;
    left: -10%;
    width: 50vw;
    height: 50vw;
    background: #C7D2FE; /* Indigo-200 */
    animation-duration: 9s;
  }
  
  .blob-2 {
    top: 20%;
    right: -20%;
    width: 60vw;
    height: 60vw;
    background: #bfdbfe; /* Blue-200 */
    animation-duration: 13s;
    animation-direction: alternate-reverse;
  }

  .blob-3 {
    bottom: -20%;
    left: 20%;
    width: 40vw;
    height: 40vw;
    background: #a5f3fc; /* Cyan-200 */
    animation-duration: 11s;
  }

  /* Dark Mode Blobs overrides */
  :global([data-theme='dark']) .blob-1 {
    background: #312e81; /* Indigo-900 */
    opacity: 0.6;
  }
  
  :global([data-theme='dark']) .blob-2 {
    background: #1e3a8a; /* Blue-900 */
    opacity: 0.5;
  }

  :global([data-theme='dark']) .blob-3 {
    background: #164e63; /* Cyan-900 */
    opacity: 0.7;
  }


  /* --- Midnight Variant (Static for now, mapped to app.css var) --- */
  .midnight-container {
    position: absolute;
    inset: 0;
    background: var(--bg-midnight);
  }

  /* --- Plain Variant --- */
  .plain-container {
    position: absolute;
    inset: 0;
    background: var(--bg-plain);
  }

  @keyframes drift {
    0% { transform: translate(0, 0) scale(1); }
    100% { transform: translate(150px, 60px) scale(1.25); }
  }
</style>
