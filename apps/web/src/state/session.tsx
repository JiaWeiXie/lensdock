import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { ApiError, onUnauthenticated } from '@/api/http';
import { enterLocal, getSession, logout as apiLogout, pair } from '@/api/session';
import type { Actor, Role, SessionInfo } from '@/api/types';

/** Minimum gap between automatic local re-entries after a 401. */
const REPROBE_MIN_MS = 5000;

export type SessionStatus = 'loading' | 'anonymous' | 'ready' | 'unreachable';

interface SessionCtx {
  status: SessionStatus;
  actor: Actor | null;
  /** Last pairing failure; cleared on the next attempt. */
  loginError: ApiError | null;
  pairing: boolean;
  /** True for automatic local access without token pairing. */
  localAccess: boolean;
  /** True when the Gateway offers local re-entry after sign-out. */
  localAvailable: boolean;
  login: (token: string) => Promise<boolean>;
  enterLocal: () => Promise<boolean>;
  logout: () => Promise<void>;
  retry: () => void;
}

const Ctx = createContext<SessionCtx | null>(null);

export function canOperate(role: Role | undefined): boolean {
  return role === 'human' || role === 'admin';
}

export function SessionProvider({ children }: { children: ReactNode }): ReactNode {
  const [status, setStatus] = useState<SessionStatus>('loading');
  const [info, setInfo] = useState<SessionInfo | null>(null);
  const [localAvailable, setLocalAvailable] = useState(false);
  const [loginError, setLoginError] = useState<ApiError | null>(null);
  const [pairing, setPairing] = useState(false);
  const [probe, setProbe] = useState(0);
  const localRef = useRef(false);
  const lastReprobe = useRef(0);

  useEffect(() => {
    localRef.current = info?.localAccess === true;
  }, [info]);

  useEffect(() => {
    const controller = new AbortController();
    // Only the first probe shows the loading screen; a silent re-entry keeps the app mounted.
    if (!localRef.current) setStatus('loading');
    getSession(controller.signal)
      .then((p) => {
        if (controller.signal.aborted) return;
        setInfo(p.info);
        setLocalAvailable(p.localAvailable);
        setStatus(p.info ? 'ready' : 'anonymous');
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setStatus(err instanceof ApiError && err.status === 0 ? 'unreachable' : 'anonymous');
      });
    return () => controller.abort();
  }, [probe]);

  useEffect(() => {
    onUnauthenticated(() => {
      // Re-enter local access after a Gateway restart or configured expiry.
      // Back off on persistent 401s rather than continuously allocating sessions.
      const now = Date.now();
      if (localRef.current && now - lastReprobe.current > REPROBE_MIN_MS) {
        lastReprobe.current = now;
        setProbe((n) => n + 1);
        return;
      }
      setInfo(null);
      setStatus('anonymous');
    });
    return () => onUnauthenticated(null);
  }, []);

  const login = useCallback(async (token: string): Promise<boolean> => {
    setLoginError(null);
    setPairing(true);
    try {
      const s = await pair(token);
      setInfo(s);
      setStatus('ready');
      return true;
    } catch (err) {
      setLoginError(err instanceof ApiError ? err : new ApiError(0, 'NETWORK', 'pairing failed'));
      return false;
    } finally {
      setPairing(false);
    }
  }, []);

  const localLogin = useCallback(async (): Promise<boolean> => {
    setLoginError(null);
    setPairing(true);
    try {
      const s = await enterLocal();
      setInfo(s);
      setLocalAvailable(true);
      setStatus('ready');
      return true;
    } catch (err) {
      setLoginError(err instanceof ApiError ? err : new ApiError(0, 'NETWORK', 'local entry failed'));
      return false;
    } finally {
      setPairing(false);
    }
  }, []);

  const logout = useCallback(async (): Promise<void> => {
    const wasLocal = localRef.current;
    try {
      await apiLogout();
    } finally {
      // Sign-out remains effective until the user explicitly enters again.
      if (wasLocal) setLocalAvailable(true);
      setInfo(null);
      setStatus('anonymous');
    }
  }, []);

  const retry = useCallback(() => setProbe((n) => n + 1), []);

  const value = useMemo<SessionCtx>(
    () => ({ status, actor: info?.actor ?? null, localAccess: info?.localAccess === true, localAvailable, loginError, pairing, login, enterLocal: localLogin, logout, retry }),
    [status, info, localAvailable, loginError, pairing, login, localLogin, logout, retry],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useSession(): SessionCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error('useSession outside SessionProvider');
  return v;
}
