import { writable } from 'svelte/store';

export type ToastVariant = 'info' | 'success' | 'warning' | 'error';

export type Toast = {
  id: string;
  message: string;
  variant: ToastVariant;
  timeout?: number;
};

function createToastStore() {
  const { subscribe, update } = writable<Toast[]>([]);

  const remove = (id: string) => {
    update((items) => items.filter((item) => item.id !== id));
  };

  const generateId = () => {
    if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
      return crypto.randomUUID();
    }
    return `toast-${Math.random().toString(16).slice(2)}`;
  };

  const push = (toast: Omit<Toast, 'id'> & { id?: string }) => {
    const id = toast.id ?? generateId();
    const { variant = 'info', ...rest } = toast;
    const entry: Toast = { ...rest, variant, id };
    update((items) => [...items, entry]);

    if (entry.timeout && entry.timeout > 0) {
      setTimeout(() => remove(id), entry.timeout);
    }

    return id;
  };

  return {
    subscribe,
    push,
    remove,
    clear: () => update(() => [])
  };
}

export const toasts = createToastStore();
