import { ApiError, request, setCsrfToken } from './http';
import type { SessionInfo } from './types';

/** What the Gateway said about the current browser: a session, or anonymous (maybe with demo entry on offer). */
export interface SessionProbe {
  info: SessionInfo | null;
  /** True after a demo sign-out: the Gateway runs local demo no-auth and waits for an explicit entry. */
  demoAvailable: boolean;
}

/**
 * Reads the cookie session. With the Gateway's opt-in local demo no-auth, this same-origin
 * fetch also creates the demo session, so no token is needed; otherwise 401 means anonymous.
 */
export async function getSession(signal?: AbortSignal): Promise<SessionProbe> {
  try {
    const info = await request<SessionInfo>('/api/v1/session', { signal, quiet401: true });
    setCsrfToken(info.csrfToken);
    return { info, demoAvailable: info.demo === true };
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) return { info: null, demoAvailable: err.code === 'DEMO_SIGNED_OUT' };
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

/** Explicit demo entry (only meaningful when the Gateway reported demo availability). */
export async function enterDemo(): Promise<SessionInfo> {
  const info = await request<SessionInfo>('/api/v1/session', {
    method: 'POST',
    body: { demo: true },
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
