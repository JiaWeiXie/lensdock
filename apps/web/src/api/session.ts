import { ApiError, request, setCsrfToken } from './http';
import type { SessionInfo } from './types';

/** The current browser session, or anonymous with optional local re-entry. */
export interface SessionProbe {
  info: SessionInfo | null;
  /** True after local sign-out when explicit token-free re-entry is available. */
  localAvailable: boolean;
}

/** Reads the session and automatically enters locally unless pairing is required. */
export async function getSession(signal?: AbortSignal): Promise<SessionProbe> {
  try {
    const info = await request<SessionInfo>('/api/v1/session', { signal, quiet401: true });
    setCsrfToken(info.csrfToken);
    return { info, localAvailable: info.localAccess === true };
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) return { info: null, localAvailable: err.code === 'LOCAL_SIGNED_OUT' };
    throw err;
  }
}

/** Pairs a scoped token for an HttpOnly session cookie. The token is not retained. */
export async function pair(token: string): Promise<SessionInfo> {
  const info = await request<SessionInfo>('/api/v1/session', {
    method: 'POST',
    body: { token },
    quiet401: true,
  });
  setCsrfToken(info.csrfToken);
  return info;
}

/** Explicit local re-entry after logout. */
export async function enterLocal(): Promise<SessionInfo> {
  const info = await request<SessionInfo>('/api/v1/session', {
    method: 'POST',
    body: { localAccess: true },
    quiet401: true,
  });
  setCsrfToken(info.csrfToken);
  return info;
}

export async function logout(): Promise<void> {
  try {
    await request<void>('/api/v1/session', { method: 'DELETE', quiet401: true });
  } finally {
    setCsrfToken('');
  }
}
