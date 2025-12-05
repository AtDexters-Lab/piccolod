type RemoteStatus = 'connected' | 'connecting' | 'disconnected' | 'error';

export type RemoteAccessState = {
  enabled: boolean;
  status: RemoteStatus;
  domain: string;
  endpoint: string;
  lastError?: string;
};

let remoteState: RemoteAccessState = {
  enabled: true,
  status: 'connected',
  domain: 'myhome.piccolo.link',
  endpoint: 'https://myhome.piccolo.link'
};

const delay = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

export async function fetchRemoteAccess(signal?: AbortSignal): Promise<RemoteAccessState> {
  if (signal?.aborted) throw new DOMException('Aborted', 'AbortError');
  await delay(180);
  return { ...remoteState };
}

export async function toggleRemoteAccess(enabled: boolean, signal?: AbortSignal): Promise<RemoteAccessState> {
  if (signal?.aborted) throw new DOMException('Aborted', 'AbortError');
  remoteState = { ...remoteState, enabled, status: enabled ? 'connected' : 'disconnected', lastError: undefined };
  await delay(220);
  return { ...remoteState };
}

export async function updateRemoteDomain(domain: string, signal?: AbortSignal): Promise<RemoteAccessState> {
  if (signal?.aborted) throw new DOMException('Aborted', 'AbortError');
  await delay(260);
  if (!domain) {
    remoteState = {
      ...remoteState,
      domain: '',
      endpoint: '',
      status: remoteState.enabled ? 'connected' : 'disconnected',
      lastError: undefined
    };
    return { ...remoteState };
  }
  if (!domain.includes('.')) {
    remoteState = { ...remoteState, status: 'error', lastError: 'Enter a valid domain.' };
    throw new Error(remoteState.lastError);
  }
  remoteState = {
    ...remoteState,
    domain,
    endpoint: `https://${domain}`,
    status: remoteState.enabled ? 'connected' : 'disconnected',
    lastError: undefined
  };
  return { ...remoteState };
}
