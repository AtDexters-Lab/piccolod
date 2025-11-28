export type UpdateChannel = 'stable' | 'beta';

export type UpdateInfo = {
  currentVersion: string;
  latestVersion: string;
  channel: UpdateChannel;
  autoUpdate: boolean;
  lastChecked?: string;
  available: boolean;
};

let updateInfo: UpdateInfo = {
  currentVersion: '0.4.0',
  latestVersion: '0.4.0',
  channel: 'stable',
  autoUpdate: true,
  available: false,
  lastChecked: new Date().toISOString()
};

const delay = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

export async function fetchUpdateInfo(): Promise<UpdateInfo> {
  await delay(140);
  return { ...updateInfo };
}

export async function checkForUpdates(): Promise<UpdateInfo> {
  await delay(400);
  updateInfo = {
    ...updateInfo,
    latestVersion: '0.4.1',
    available: updateInfo.currentVersion !== '0.4.1',
    lastChecked: new Date().toISOString()
  };
  return { ...updateInfo };
}

export async function setAutoUpdate(auto: boolean): Promise<UpdateInfo> {
  await delay(160);
  updateInfo = { ...updateInfo, autoUpdate: auto };
  return { ...updateInfo };
}
