import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { ApiError } from '@/api/http';
import type { Handoff, Lease } from '@/api/types';
import {
  acquireLease,
  cancelTakeover as apiCancelTakeover,
  currentLease,
  getTakeover,
  releaseLease,
  renewLease,
  requestTakeover,
} from '@/api/workspace';
import type { MessageKey } from '@/i18n';
import { isAbortError, newKey, sleep } from '@/util/async';
import { canOperate, useSession } from './session';
import { useWorkspace } from './workspace';

/**
 * Client side of the single-writer lease model.
 *
 * The Gateway holds the capability token for this session; the UI only tracks
 * the lease metadata (id, epoch, generation, expiry). A lease that is renewed
 * late, revoked, or whose generation changed is dropped here and the control
 * stream is closed by the desktop link; a revoked lease is never revived.
 * After a page capture the runtime restores control under a NEW lease id and
 * epoch: the UI re-reads GET leases/current and adopts it.
 */

const HEARTBEAT_MS = 10_000;
const TAKEOVER_POLL_MS = 500;
const TAKEOVER_MAX_MS = 70_000;
const RECONCILE_MAX_MS = 45_000;

export type TakeoverPhase = 'idle' | 'requesting' | 'draining' | 'drain-timeout' | 'acquiring' | 'failed';

export interface TakeoverView {
  phase: TakeoverPhase;
  handoff: Handoff | null;
  error: ApiError | null;
}

export interface LostLease {
  code: string;
  at: number;
}

interface ControlCtx {
  /** HUMAN lease held by this session at its current epoch, or null. */
  lease: Lease | null;
  lost: LostLease | null;
  takeover: TakeoverView;
  /** Non-null when taking control is currently impossible, with the reason. */
  blocked: MessageKey | null;
  /** True while an acquire/release round trip is running. */
  busy: boolean;
  error: ApiError | null;
  /** Acquires directly when idle, otherwise runs the takeover handshake with `reason`. */
  take: (reason: string) => Promise<Lease | null>;
  /** Re-posts the takeover with force after a drain timeout. The user accepted that earlier actions may still take effect. */
  forceTake: (reason: string) => Promise<void>;
  cancelTakeover: () => Promise<void>;
  release: () => Promise<void>;
  dismissLost: () => void;
  /** Re-reads the current lease (used after the desktop control stream is fenced). */
  reconcile: () => Promise<void>;
}

const Ctx = createContext<ControlCtx | null>(null);

const IDLE_TAKEOVER: TakeoverView = { phase: 'idle', handoff: null, error: null };

export function ControlProvider({ children }: { children: ReactNode }): ReactNode {
  const { actor } = useSession();
  const { workspaceId, workspace, refresh } = useWorkspace();
  const [lease, setLeaseState] = useState<Lease | null>(null);
  const [lost, setLost] = useState<LostLease | null>(null);
  const [takeover, setTakeover] = useState<TakeoverView>(IDLE_TAKEOVER);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);

  const leaseRef = useRef<Lease | null>(null);
  const workspaceRef = useRef(workspace);
  const takeoverAbort = useRef<AbortController | null>(null);
  const reconciling = useRef<Promise<void> | null>(null);
  const recoverTried = useRef(false);

  useEffect(() => {
    workspaceRef.current = workspace;
  }, [workspace]);

  const setLease = useCallback((next: Lease | null) => {
    leaseRef.current = next;
    setLeaseState(next);
  }, []);

  const lose = useCallback(
    (code: string) => {
      if (!leaseRef.current) return;
      setLease(null);
      setLost({ code, at: Date.now() });
      void refresh();
    },
    [refresh, setLease],
  );

  const adopt = useCallback(
    (next: Lease) => {
      const ws = workspaceRef.current;
      if (ws && ws.runtimeGeneration !== next.runtimeGeneration) {
        lose('GENERATION_MISMATCH');
        return;
      }
      setLease(next);
      setLost(null);
    },
    [lose, setLease],
  );

  const runReconcile = useCallback(async (): Promise<void> => {
    const hadLease = leaseRef.current !== null;
    const deadline = Date.now() + RECONCILE_MAX_MS;
    try {
      for (;;) {
        try {
          const current = await currentLease(workspaceId);
          if (current.mode === 'HUMAN') adopt(current);
          else lose('NOT_HUMAN');
          return;
        } catch (err) {
          const api = err instanceof ApiError ? err : null;
          const state = workspaceRef.current?.controlState;
          const transient = api !== null && (api.status === 0 || (api.status === 409 && state === 'CAPTURE'));
          if (transient && Date.now() < deadline) {
            await sleep(1000);
            continue;
          }
          if (hadLease) lose(api?.code ?? 'UNKNOWN');
          return;
        }
      }
    } finally {
      void refresh();
    }
  }, [adopt, lose, refresh, workspaceId]);

  // Concurrent callers (capture flow, workspace sync, fenced stream) share one in-flight read.
  const reconcile = useCallback((): Promise<void> => {
    if (reconciling.current) return reconciling.current;
    const run = runReconcile().finally(() => {
      reconciling.current = null;
    });
    reconciling.current = run;
    return run;
  }, [runReconcile]);

  // Heartbeat: renew every 10 s. Network trouble is tolerated only until the lease expires locally.
  const leaseId = lease?.leaseId;
  const leaseEpoch = lease?.epoch;
  useEffect(() => {
    if (!leaseId) return;
    let stopped = false;
    let timer: number | undefined;
    const tick = async (): Promise<void> => {
      const current = leaseRef.current;
      if (stopped || !current || current.leaseId !== leaseId) return;
      try {
        const renewed = await renewLease(workspaceId, current);
        if (stopped || leaseRef.current?.leaseId !== leaseId) return;
        if (renewed.leaseId === leaseId) setLease(renewed);
        else adopt(renewed);
      } catch (err) {
        if (stopped) return;
        const api = err instanceof ApiError ? err : null;
        if (api && (api.code === 'LEASE_ENDED' || api.code === 'EPOCH_MISMATCH') && workspaceRef.current?.controlState === 'CAPTURE') {
          void reconcile();
          return;
        }
        const networkOnly = api === null || api.status === 0 || api.status >= 500;
        if (networkOnly && Date.now() < Date.parse(current.expiresAt)) {
          timer = window.setTimeout(() => void tick(), 2000);
          return;
        }
        lose(networkOnly ? 'EXPIRED' : (api?.code ?? 'UNKNOWN'));
        return;
      }
      timer = window.setTimeout(() => void tick(), HEARTBEAT_MS);
    };
    timer = window.setTimeout(() => void tick(), HEARTBEAT_MS);
    return () => {
      stopped = true;
      window.clearTimeout(timer);
    };
  }, [leaseId, leaseEpoch, workspaceId, adopt, lose, reconcile, setLease]);

  // Follow server truth: a lease the runtime no longer attributes to us is gone.
  const controlState = workspace?.controlState;
  const holderId = workspace?.holderId;
  const generation = workspace?.runtimeGeneration;
  useEffect(() => {
    if (!workspace || !actor) return;
    const mine = leaseRef.current;
    if (mine) {
      if (generation !== mine.runtimeGeneration) {
        lose('GENERATION_MISMATCH');
      } else if (controlState === 'CAPTURE') {
        void reconcile();
      } else if (takeoverAbort.current === null && (controlState !== 'HUMAN' || holderId !== actor.id)) {
        lose('REVOKED');
      }
      return;
    }
    if (controlState === 'HUMAN' && holderId === actor.id && !recoverTried.current) {
      // A reload while holding control: the Gateway still maps this session to its lease.
      recoverTried.current = true;
      void reconcile();
    }
    if (controlState !== 'HUMAN') recoverTried.current = false;
  }, [workspace, actor, controlState, holderId, generation, lose, reconcile]);

  useEffect(
    () => () => {
      takeoverAbort.current?.abort();
    },
    [],
  );

  const acquire = useCallback(
    async (takeoverId?: string): Promise<Lease> => {
      const granted = await acquireLease(workspaceId, 'HUMAN', {
        idempotencyKey: newKey(),
        ...(takeoverId ? { takeoverId } : {}),
      });
      adopt(granted);
      void refresh();
      return granted;
    },
    [adopt, refresh, workspaceId],
  );

  const runTakeover = useCallback(
    async (reason: string, force: boolean): Promise<void> => {
      takeoverAbort.current?.abort();
      const controller = new AbortController();
      takeoverAbort.current = controller;
      const finish = (): void => {
        if (takeoverAbort.current === controller) takeoverAbort.current = null;
      };
      setError(null);
      setTakeover({ phase: 'requesting', handoff: null, error: null });
      try {
        const pending = await requestTakeover(workspaceId, reason, force, newKey());
        const started = Date.now();
        setTakeover({ phase: 'draining', handoff: null, error: null });
        for (;;) {
          if (Date.now() - started > TAKEOVER_MAX_MS) {
            throw new ApiError(0, 'TAKEOVER_TIMEOUT', 'takeover did not complete');
          }
          const handoff = await getTakeover(workspaceId, controller.signal);
          if (handoff.status === 'READY') {
            setTakeover({ phase: 'acquiring', handoff, error: null });
            await acquire(pending.requestId);
            setTakeover(IDLE_TAKEOVER);
            finish();
            return;
          }
          if (handoff.status === 'DRAIN_TIMEOUT') {
            setTakeover({ phase: 'drain-timeout', handoff, error: null });
            finish();
            return;
          }
          setTakeover({ phase: 'draining', handoff, error: null });
          await sleep(TAKEOVER_POLL_MS, controller.signal);
        }
      } catch (err) {
        finish();
        if (isAbortError(err)) return;
        const api = err instanceof ApiError ? err : new ApiError(0, 'UNKNOWN', 'takeover failed');
        setTakeover({ phase: 'failed', handoff: null, error: api });
      } finally {
        void refresh();
      }
    },
    [acquire, refresh, workspaceId],
  );

  const take = useCallback(
    async (reason: string): Promise<Lease | null> => {
      setError(null);
      setLost(null);
      const state = workspaceRef.current?.controlState;
      if (state === 'IDLE' && !workspaceRef.current?.handoff) {
        setBusy(true);
        try {
          return await acquire();
        } catch (err) {
          setError(err instanceof ApiError ? err : new ApiError(0, 'UNKNOWN', 'acquire failed'));
          return null;
        } finally {
          setBusy(false);
        }
      }
      await runTakeover(reason, false);
      return leaseRef.current;
    },
    [acquire, runTakeover],
  );

  const forceTake = useCallback((reason: string) => runTakeover(reason, true), [runTakeover]);

  const cancelTakeover = useCallback(async (): Promise<void> => {
    takeoverAbort.current?.abort();
    takeoverAbort.current = null;
    try {
      await apiCancelTakeover(workspaceId);
    } catch (err) {
      if (!(err instanceof ApiError && err.status === 404)) {
        setTakeover({ phase: 'failed', handoff: null, error: err instanceof ApiError ? err : null });
        return;
      }
    }
    setTakeover(IDLE_TAKEOVER);
    void refresh();
  }, [refresh, workspaceId]);

  const release = useCallback(async (): Promise<void> => {
    const mine = leaseRef.current;
    if (!mine) return;
    setBusy(true);
    setError(null);
    try {
      await releaseLease(workspaceId, mine.leaseId);
    } catch (err) {
      // 404/409: the lease already ended; the goal state (no lease) is reached.
      if (!(err instanceof ApiError && (err.status === 404 || err.status === 409))) {
        setError(err instanceof ApiError ? err : new ApiError(0, 'UNKNOWN', 'release failed'));
        setBusy(false);
        return;
      }
    }
    setLease(null);
    setBusy(false);
    void refresh();
  }, [refresh, setLease, workspaceId]);

  const dismissLost = useCallback(() => setLost(null), []);

  const blocked = useMemo<MessageKey | null>(() => {
    if (!actor || !canOperate(actor.role)) return 'control.blocked.role';
    if (!workspace) return 'control.blocked.unknown';
    if (workspace.maintenance || workspace.controlState === 'MAINTENANCE') return 'control.blocked.maintenance';
    if (workspace.controlState === 'ERROR_LOCKED' || workspace.locked) return 'control.blocked.locked';
    if (workspace.controlState === 'CAPTURE') return 'control.blocked.capture';
    if (!workspace.ready) return 'control.blocked.notReady';
    return null;
  }, [actor, workspace]);

  const value = useMemo<ControlCtx>(
    () => ({ lease, lost, takeover, blocked, busy, error, take, forceTake, cancelTakeover, release, dismissLost, reconcile }),
    [lease, lost, takeover, blocked, busy, error, take, forceTake, cancelTakeover, release, dismissLost, reconcile],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useControl(): ControlCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error('useControl outside ControlProvider');
  return v;
}
