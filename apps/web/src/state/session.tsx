import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { ApiError, onUnauthenticated } from '@/api/http';
import { enterDemo, getSession, logout as apiLogout, pair } from '@/api/session';
import type { Actor, Role, SessionInfo } from '@/api/types';

/** Minimum gap between silent demo re-entries after a 401. */
const REPROBE_MIN_MS = 5000;

export type SessionStatus = 'loading' | 'anonymous' | 'ready' | 'unreachable';

interface SessionCtx {
  status: SessionStatus;
  actor: Actor | null;
  /** Last pairing failure; cleared on the next attempt. */
  loginError: ApiError | null;
  pairing: boolean;
  /** True while signed in through the Gateway's local demo no-auth (actor id `demo-human`). */
  demo: boolean;
  /** True when the Gateway offers demo entry to this signed-out browser. */
  demoAvailable: boolean;
  login: (token: string) => Promise<boolean>;
  enterDemo: () => Promise<boolean>;
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
  const [demoAvailable, setDemoAvailable] = useState(false);
  const [loginError, setLoginError] = useState<ApiError | null>(null);
  const [pairing, setPairing] = useState(false);
  const [probe, setProbe] = useState(0);
  const demoRef = useRef(false);
  const lastReprobe = useRef(0);

  useEffect(() => {
    demoRef.current = info?.demo === true;
  }, [info]);

  useEffect(() => {
    const controller = new AbortController();
    // Only the first probe shows the loading screen; a silent re-entry keeps the app mounted.
    if (!demoRef.current) setStatus('loading');
    getSession(controller.signal)
      .then((p) => {
        if (controller.signal.aborted) return;
        setInfo(p.info);
        setDemoAvailable(p.demoAvailable);
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
      // A demo session never expires server-side, so a 401 means the Gateway restarted and
      // dropped its in-memory sessions: silently re-enter instead of showing a login page.
      // Rate-limited so a persistent 401 cannot loop and allocate sessions.
      const now = Date.now();
      if (demoRef.current && now - lastReprobe.current > REPROBE_MIN_MS) {
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

  const demoLogin = useCallback(async (): Promise<boolean> => {
    setLoginError(null);
    setPairing(true);
    try {
      const s = await enterDemo();
      setInfo(s);
      setDemoAvailable(true);
      setStatus('ready');
      return true;
    } catch (err) {
      setLoginError(err instanceof ApiError ? err : new ApiError(0, 'NETWORK', 'demo entry failed'));
      return false;
    } finally {
      setPairing(false);
    }
  }, []);

  const logout = useCallback(async (): Promise<void> => {
    const wasDemo = demoRef.current;
    try {
      await apiLogout();
    } finally {
      // After a demo sign-out the Gateway waits for an explicit demo entry; offer it.
      if (wasDemo) setDemoAvailable(true);
      setInfo(null);
      setStatus('anonymous');
    }
  }, []);

  const retry = useCallback(() => setProbe((n) => n + 1), []);

  const value = useMemo<SessionCtx>(
    () => ({ status, actor: info?.actor ?? null, demo: info?.demo === true, demoAvailable, loginError, pairing, login, enterDemo: demoLogin, logout, retry }),
    [status, info, demoAvailable, loginError, pairing, login, demoLogin, logout, retry],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useSession(): SessionCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error('useSession outside SessionProvider');
  return v;
}
