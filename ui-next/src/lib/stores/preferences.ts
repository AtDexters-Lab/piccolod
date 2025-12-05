import { writable } from 'svelte/store';

export type ThemeMode = 'light' | 'dark';
export type Preferences = {
  theme: ThemeMode;
  background: 'aurora' | 'midnight' | 'plain';
  accent: string;
};

const defaultPreferences: Preferences = {
  theme: 'light',
  background: 'aurora',
  accent: '#6266ff'
};

export const preferencesStore = writable<Preferences>(defaultPreferences);

let persistenceInitialized = false;
let loadedFromStorage = false;

export function initPreferencesPersistence(): void {
  if (persistenceInitialized || typeof window === 'undefined') return;

  const saved = window.localStorage.getItem('piccolo.preferences');
  if (saved) {
    try {
      const parsed = JSON.parse(saved) as Partial<Preferences>;
      preferencesStore.set({
        ...defaultPreferences,
        ...parsed,
        theme: parsed.theme === 'dark' ? 'dark' : parsed.theme === 'light' ? 'light' : defaultPreferences.theme
      });
      loadedFromStorage = true;
    } catch {
      // ignore malformed payloads
    }
  }

  preferencesStore.subscribe((value) => {
    window.localStorage.setItem('piccolo.preferences', JSON.stringify(value));
  });

  persistenceInitialized = true;
}

export function preferencesWereLoaded(): boolean {
  return loadedFromStorage;
}
