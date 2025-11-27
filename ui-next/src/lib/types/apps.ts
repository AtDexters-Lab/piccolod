export type IconResolution = 'provided' | 'label' | 'catalog' | 'favicon' | 'fallback';

export interface AppManifest {
  id: string;
  displayName: string;
  origin?: string;
  iconUrl?: string;
  labels?: Record<string, string>;
  isSystem?: boolean;
  pinned?: boolean;
}

export interface ResolvedApp extends AppManifest {
  resolvedIcon: string;
  iconResolution: IconResolution;
}
