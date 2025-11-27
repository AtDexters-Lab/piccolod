import { derived, type Readable } from 'svelte/store';
import { layerStack, topLayer } from '$lib/stores/layers';
import type { LayerEntry } from '$lib/stores/layers';

export function createLayerLifecycle(): Readable<void> {
  let lastForeground: LayerEntry | undefined;

  return derived([layerStack, topLayer], ([$stack, $top]) => {
    const next = $top;

    // If foreground unchanged, do nothing.
    if (lastForeground?.id === next?.id) return;

    // Fire background for previous foreground even if it no longer exists in stack.
    lastForeground?.onBackground?.();

    // Fire foreground for new top.
    next?.onForeground?.();

    // Update cached entry; if next is undefined, we still clear reference.
    lastForeground = next;
  });
}
