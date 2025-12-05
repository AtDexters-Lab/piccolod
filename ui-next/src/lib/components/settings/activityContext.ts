import type { Writable } from 'svelte/store';

export type SettingsActivityContext = {
  setActive: (id: string | null) => void;
  getActive: () => string | null;
  isEmbedded: boolean;
};

export const settingsActivityKey = Symbol('settings-activity');

export type SettingsNavWritable = Writable<{
  sections: Array<{
    id: string;
    label: string;
    items: Array<{ id: string; label: string; description?: string; href: string; badge?: string }>;
  }>;
  isSplit: boolean;
}>;
