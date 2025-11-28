export type SettingsNavItem = {
  id: string;
  label: string;
  description?: string;
  href: string;
  badge?: string;
};

export type SettingsNavSection = {
  id: string;
  label: string;
  items: SettingsNavItem[];
};

export type SettingsNavState = {
  sections: SettingsNavSection[];
  isSplit: boolean;
};
