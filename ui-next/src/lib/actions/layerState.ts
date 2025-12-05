import type { LayerEntry } from '$lib/stores/layers';

type LayerStateArgs = {
  entry: LayerEntry;
  isForeground: boolean;
};

export function layerState(node: HTMLElement, params: LayerStateArgs) {
  const apply = ({ isForeground }: LayerStateArgs) => {
    node.dataset.layerState = isForeground ? 'foreground' : 'background';
    node.style.pointerEvents = isForeground ? 'auto' : 'none';
    node.style.overflow = isForeground ? '' : 'hidden';
  };

  apply(params);

  return {
    update(next: LayerStateArgs) {
      apply(next);
    }
  };
}
