import { derived, writable } from 'svelte/store';
import type { AppManifest, IconResolution, ResolvedApp } from '$lib/types/apps';

function hashString(value: string): number {
  let hash = 0;
  for (let i = 0; i < value.length; i += 1) {
    hash = (hash << 5) - hash + value.charCodeAt(i);
    hash |= 0;
  }
  return Math.abs(hash);
}

function initials(value: string): string {
  const parts = value.trim().split(/\s+/);
  if (parts.length === 1) {
    return parts[0].slice(0, 2).toUpperCase();
  }
  return (parts[0][0] + parts[1][0]).toUpperCase();
}

export function createFallbackIcon(label: string): string {
  const safeLabel = label?.trim() || 'App';
  const hue = hashString(safeLabel) % 360;
  const bg = `hsl(${hue}, 70%, 88%)`;
  const fg = `hsl(${hue}, 35%, 32%)`;
  const text = initials(safeLabel);
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 96 96" role="img" aria-label="${safeLabel} icon"><rect width="96" height="96" rx="24" fill="${bg}"/><text x="50%" y="52%" text-anchor="middle" font-family="Inter, system-ui, sans-serif" font-size="34" font-weight="600" fill="${fg}" dy="10">${text}</text></svg>`;
  return `data:image/svg+xml,${encodeURIComponent(svg)}`;
}

function normalizeKey(app: AppManifest): string {
  return (app.id || app.displayName || '').trim().toLowerCase();
}

const catalogIcons: Record<string, string> = {
  plex: createFallbackIcon('Plex'),
  nextcloud: createFallbackIcon('Nextcloud'),
  immich: createFallbackIcon('Immich'),
  paperless: createFallbackIcon('Paperless'),
  jellyfin: createFallbackIcon('Jellyfin'),
  files: createFallbackIcon('Files'),
  settings: createFallbackIcon('Settings'),
  photos: createFallbackIcon('Photos')
};

function faviconFrom(origin?: string): string | undefined {
  if (!origin) return undefined;
  try {
    return new URL('/favicon.ico', origin).toString();
  } catch {
    return undefined;
  }
}

export function resolveIconForApp(app: AppManifest): ResolvedApp {
  const catalogKey = normalizeKey(app);
  const candidates: Array<[IconResolution, string | undefined]> = [
    ['provided', app.iconUrl],
    ['label', app.labels?.['piccolo.ui.icon']],
    ['catalog', catalogIcons[catalogKey]],
    ['favicon', faviconFrom(app.origin)],
    ['fallback', createFallbackIcon(app.displayName)]
  ];

  for (const [iconResolution, url] of candidates) {
    if (url) {
      return { ...app, resolvedIcon: url, iconResolution };
    }
  }

  const fallback = createFallbackIcon(app.displayName);
  return { ...app, resolvedIcon: fallback, iconResolution: 'fallback' };
}

export const appManifests = writable<AppManifest[]>([]);

export const resolvedApps = derived(appManifests, ($apps) => $apps.map((app) => resolveIconForApp(app)));

export function setAppManifests(apps: AppManifest[]): void {
  appManifests.set(apps);
}
