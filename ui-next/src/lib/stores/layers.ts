import { derived, get, writable } from 'svelte/store';

export type LayerKind = 'stage' | 'app' | 'drawer' | 'modal';

export type LayerEntry = {
  id: string;
  kind: LayerKind;
  scrollLock?: boolean;
  focusTrap?: boolean;
  safeArea?: boolean;
  payload?: Record<string, unknown>;
  onForeground?: () => void;
  onBackground?: () => void;
};

const baseStack: LayerEntry[] = [{ id: 'stage', kind: 'stage', scrollLock: false, safeArea: false }];

export const layerStack = writable<LayerEntry[]>(baseStack);

export const topLayer = derived(layerStack, ($stack) => $stack[$stack.length - 1]);

export function resetStack(): void {
  layerStack.set([...baseStack]);
}

export function setStack(entries: LayerEntry[]): void {
  if (!entries.length || entries[0].kind !== 'stage') {
    throw new Error('Stack must start with stage');
  }
  layerStack.set(entries);
}

export function pushLayer(entry: LayerEntry): void {
  layerStack.update((stack) => [...stack, entry]);
}

export function popLayer(id?: string): void {
  layerStack.update((stack) => {
    if (!id) {
      if (stack.length <= 1) return stack; // keep stage
      return stack.slice(0, stack.length - 1);
    }
    const filtered = stack.filter((layer) => layer.id !== id);
    return filtered.length === 0 ? baseStack : filtered;
  });
}

export function bringToFront(id: string): void {
  layerStack.update((stack) => {
    const existing = stack.find((l) => l.id === id);
    if (!existing) return stack;
    const without = stack.filter((l) => l.id !== id);
    return [...without, existing];
  });
}

export function layerState(id: string) {
  return derived(layerStack, ($stack) => {
    const idx = $stack.findIndex((l) => l.id === id);
    const isForeground = idx === $stack.length - 1 && idx !== -1;
    return {
      inStack: idx !== -1,
      isForeground,
      positionFromTop: idx === -1 ? null : $stack.length - 1 - idx,
      entry: idx === -1 ? null : $stack[idx]
    };
  });
}

export function layerExists(id: string): boolean {
  return get(layerStack).some((l) => l.id === id);
}
