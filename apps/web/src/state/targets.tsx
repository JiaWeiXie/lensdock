import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import type { ApiError } from '@/api/http';
import type { BrowserTarget } from '@/api/types';
import { listTargets } from '@/api/workspace';
import { useWorkspace } from './workspace';

const POLL_MS = 5000;

interface TargetsCtx {
  targets: BrowserTarget[];
  error: ApiError | null;
  /** True once the first answer arrived (successful or not). */
  loaded: boolean;
  refresh: () => Promise<void>;
}

const Ctx = createContext<TargetsCtx | null>(null);

/** Page targets of the shared Chromium, polled while the runtime is ready. */
export function TargetsProvider({ children }: { children: ReactNode }): ReactNode {
  const { workspaceId, workspace } = useWorkspace();
  const [targets, setTargets] = useState<BrowserTarget[]>([]);
  const [error, setError] = useState<ApiError | null>(null);
  const [loaded, setLoaded] = useState(false);
  // Superseded reads are dropped by sequence number, never aborted (an aborted request that the
  // server already handled is reported by the browser as a failed request).
  const seq = useRef(0);
  const mounted = useRef(true);
  const ready = workspace?.ready === true;
  const generation = workspace?.runtimeGeneration;

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const refresh = useCallback(async (): Promise<void> => {
    seq.current += 1;
    const mine = seq.current;
    if (!ready) {
      setTargets([]);
      setLoaded(true);
      return;
    }
    try {
      const res = await listTargets(workspaceId);
      if (!mounted.current || mine !== seq.current) return;
      setTargets(res.targets.filter((x) => x.type === 'page'));
      setError(null);
    } catch (err) {
      if (!mounted.current || mine !== seq.current) return;
      setError(err as ApiError);
      setTargets([]);
    } finally {
      if (mounted.current && mine === seq.current) setLoaded(true);
    }
  }, [workspaceId, ready]);

  useEffect(() => {
    let timer: number | undefined;
    let stopped = false;
    const tick = (): void => {
      void refresh().finally(() => {
        if (!stopped) timer = window.setTimeout(tick, POLL_MS);
      });
    };
    tick();
    return () => {
      stopped = true;
      window.clearTimeout(timer);
    };
    // A new browser generation invalidates every earlier target list.
  }, [refresh, generation]);

  const value = useMemo<TargetsCtx>(() => ({ targets, error, loaded, refresh }), [targets, error, loaded, refresh]);
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useTargets(): TargetsCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error('useTargets outside TargetsProvider');
  return v;
}
