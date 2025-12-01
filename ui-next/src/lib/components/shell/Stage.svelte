<script lang="ts">
  import { layerState as layerStateAction } from '$lib/actions/layerState';

  export let isForeground: boolean = true;
  export let hasActiveApp: boolean = false;
  export let safeAreaTop: string = '0px';
  export let safeAreaBottom: string = '0px';
</script>

<section
  class={`stage relative z-0 px-6 sm:px-10 lg:px-16 ${hasActiveApp ? 'stage--blocked' : ''}`}
  aria-label="Stage content"
  use:layerStateAction={{ entry: { id: 'stage', kind: 'stage' }, isForeground }}
  style="--safe-area-top: {safeAreaTop}; --safe-area-bottom: {safeAreaBottom};"
>
  <slot />
</section>

<style>
  .stage {
    min-height: 100vh;
    padding-top: var(--safe-area-top);
    padding-bottom: var(--safe-area-bottom);
    overflow-y: auto;
    scrollbar-gutter: stable;
    position: relative;
    z-index: 0;
  }

  .stage--blocked {
    overflow: hidden;
    pointer-events: none;
  }
</style>
