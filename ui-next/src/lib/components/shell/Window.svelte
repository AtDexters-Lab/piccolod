<script lang="ts">
  import { createEventDispatcher } from "svelte";
  import { fly, fade } from "svelte/transition";
  import { cubicOut } from "svelte/easing";
  import Button from "$lib/components/ui/Button.svelte";
  import { layerState as layerStateAction } from "$lib/actions/layerState";
  import { scrollLock } from "$lib/actions/scrollLock";
  import type { ResolvedApp } from "$lib/types/apps";

  export let activeApp: ResolvedApp;
  export let layerId: string;
  export let isTopLayer: boolean;
  export let reduceMotion: boolean;
  export let safeAreaTop: string = "0px";
  export let safeAreaBottom: string = "0px";

  const dispatch = createEventDispatcher<{
    close: void;
    switchApp: void;
    openNewTab: void;
  }>();
</script>

<section
  class="app-layer"
  use:layerStateAction={{
    entry: { id: layerId, kind: "app" },
    isForeground: isTopLayer,
  }}
  use:scrollLock={isTopLayer}
  in:fly={reduceMotion
    ? { duration: 0 }
    : { y: 16, duration: 180, easing: cubicOut }}
  out:fade={reduceMotion ? { duration: 0 } : { duration: 120 }}
  aria-live="polite"
  aria-label={`Active app ${activeApp.displayName}`}
  data-layer-state={isTopLayer ? "foreground" : "background"}
  style="--safe-area-top: {safeAreaTop}; --safe-area-bottom: {safeAreaBottom};"
>
  <div class="app-shell">
    <div class="app-window" aria-label={`${activeApp.displayName} window`}>
      <header class="window-chrome">
        <div class="flex items-center gap-3">
          <div class="pill-icon">
            {activeApp.displayName.slice(0, 2).toUpperCase()}
          </div>
          <div class="leading-tight">
            <p class="text-[11px] uppercase tracking-[0.16em] text-muted">
              App
            </p>
            <p class="font-display text-base font-semibold text-ink">
              {activeApp.displayName}
            </p>
          </div>
        </div>
        <div class="flex items-center gap-1">
          <Button
            variant="ghost"
            size="icon"
            on:click={() => dispatch("openNewTab")}
            title="Open in new tab"
          >
            <svg
              width="20"
              height="20"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-linecap="round"
              stroke-linejoin="round"
              ><path
                d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"
              /><polyline points="15 3 21 3 21 9" /><line
                x1="10"
                y1="14"
                x2="21"
                y2="3"
              /></svg
            >
          </Button>
          <Button
            variant="secondary"
            size="icon"
            on:click={() => dispatch("switchApp")}
            title="Switch app"
          >
            <svg
              width="20"
              height="20"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-linecap="round"
              stroke-linejoin="round"
              ><rect x="3" y="3" width="7" height="7" /><rect
                x="14"
                y="3"
                width="7"
                height="7"
              /><rect x="14" y="14" width="7" height="7" /><rect
                x="3"
                y="14"
                width="7"
                height="7"
              /></svg
            >
          </Button>
          <Button
            variant="ghost"
            size="icon"
            on:click={() => dispatch("close")}
            title="Close"
          >
            <svg
              width="20"
              height="20"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-linecap="round"
              stroke-linejoin="round"
              ><line x1="18" y1="6" x2="6" y2="18" /><line
                x1="6"
                y1="6"
                x2="18"
                y2="18"
              /></svg
            >
          </Button>
        </div>
      </header>

      <div class="app-body" aria-label={`${activeApp.displayName} content`}>
        <slot />
      </div>
    </div>
  </div>
</section>

<style>
  [data-layer-state="background"] {
    pointer-events: none;
    overflow: hidden !important;
  }

  .app-layer {
    position: fixed;
    inset: 0;
    z-index: 10;
    padding: var(--safe-area-top) 16px var(--safe-area-bottom) 16px;
    overflow: hidden;
    background: radial-gradient(
      circle at 20% 10%,
      rgba(255, 255, 255, 0.7),
      rgba(235, 240, 250, 0.8)
    );
  }

  :global([data-theme="dark"]) .app-layer {
    background: radial-gradient(
      circle at 20% 10%,
      rgba(26, 32, 48, 0.9),
      rgba(12, 16, 28, 0.95)
    );
  }

  .app-shell {
    margin: 0 auto;
    max-width: 1100px;
    display: flex;
    flex-direction: column;
    height: calc(100vh - var(--safe-area-top) - var(--safe-area-bottom) - 16px);
    align-items: center;
  }

  .app-window {
    width: min(1080px, calc(100vw - 32px));
    height: calc(100vh - var(--safe-area-top) - var(--safe-area-bottom));
    background: rgba(255, 255, 255, 0.94);
    border-radius: 22px;
    border: 1px solid rgba(255, 255, 255, 0.65);
    box-shadow: 0 28px 70px rgba(18, 24, 40, 0.2);
    backdrop-filter: blur(18px);
    pointer-events: auto;
    display: flex;
    flex-direction: column;
    overflow: hidden;
    min-height: 0;
  }

  :global([data-theme="dark"]) .app-window {
    background: rgba(18, 22, 34, 0.92);
    border-color: rgba(255, 255, 255, 0.08);
    box-shadow: 0 28px 70px rgba(0, 0, 0, 0.4);
  }

  .window-chrome {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding: 10px 16px;
    border-bottom: 1px solid rgba(255, 255, 255, 0.5);
    background: linear-gradient(
      180deg,
      rgba(255, 255, 255, 0.92),
      rgba(255, 255, 255, 0.86)
    );
  }

  :global([data-theme="dark"]) .window-chrome {
    border-color: rgba(255, 255, 255, 0.08);
    background: linear-gradient(
      180deg,
      rgba(22, 26, 38, 0.96),
      rgba(18, 22, 34, 0.9)
    );
  }

  .app-body {
    padding: 0;
    flex: 1;
    overflow-y: auto;
    overflow-x: visible;
    min-height: 0;
  }

  .pill-icon {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 36px;
    height: 36px;
    border-radius: 12px;
    background: linear-gradient(180deg, #3d66ff, #2f5af3);
    color: white;
    font-weight: 700;
    font-size: 13px;
  }

  @media (max-width: 540px) {
    .window-chrome {
      flex-direction: column;
      align-items: flex-start;
      gap: 8px;
    }
  }
</style>
