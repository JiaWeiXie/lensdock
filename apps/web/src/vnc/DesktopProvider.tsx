import { createContext, useContext, useEffect, useMemo, useState, useSyncExternalStore, type ReactNode } from 'react';
import { vncUrl } from '@/api/workspace';
import { useControl } from '@/state/control';
import { useSession } from '@/state/session';
import { useWorkspace } from '@/state/workspace';
import { RfbController, type RfbSnapshot, type StreamTarget } from './controller';
import { RemoteClipboardProvider } from './RemoteClipboardProvider';

const Ctx = createContext<RfbController | null>(null);

/**
 * Owns the single RfbController and keeps its stream target in step with the
 * lease: the control stream (pinned to lease epoch and runtime generation) is
 * used only while this session holds a live HUMAN lease; every other time the
 * UI watches through the backend-enforced view-only stream.
 */
export function DesktopProvider({ children }: { children: ReactNode }): ReactNode {
  const { actor } = useSession();
  const { workspaceId, workspace } = useWorkspace();
  const { lease, reconcile } = useControl();
  const controller = useMemo(() => new RfbController(), []);

  useEffect(() => {
    controller.onFenced = (): void => {
      void reconcile();
    };
    return () => {
      controller.onFenced = null;
    };
  }, [controller, reconcile]);

  useEffect(() => () => controller.dispose(), [controller]);

  const role = actor?.role;
  const generation = workspace?.runtimeGeneration;
  const controlState = workspace?.controlState;
  const leaseId = lease?.leaseId;
  const leaseEpoch = lease?.epoch;
  const leaseGeneration = lease?.runtimeGeneration;

  const target = useMemo<StreamTarget | null>(() => {
    if (!generation || role === 'agent') return null;
    if (leaseId && leaseEpoch !== undefined && leaseGeneration && controlState === 'HUMAN' && leaseGeneration === generation) {
      return {
        key: `control:${leaseId}:${leaseEpoch}:${leaseGeneration}`,
        url: vncUrl(workspaceId, 'control', { epoch: leaseEpoch, runtimeGeneration: leaseGeneration }),
        mode: 'control',
        generation: leaseGeneration,
      };
    }
    return { key: `view:${generation}`, url: vncUrl(workspaceId, 'view'), mode: 'view', generation };
  }, [workspaceId, generation, role, controlState, leaseId, leaseEpoch, leaseGeneration]);

  useEffect(() => {
    controller.setTarget(target);
  }, [controller, target]);

  return (
    <Ctx.Provider value={controller}>
      <RemoteClipboardProvider controller={controller} session={target?.mode === 'control' ? target.key : null}>
        {children}
      </RemoteClipboardProvider>
    </Ctx.Provider>
  );
}

export function useRfbController(): RfbController {
  const v = useContext(Ctx);
  if (!v) throw new Error('useRfbController outside DesktopProvider');
  return v;
}

export function useRfb(): RfbSnapshot {
  const c = useRfbController();
  return useSyncExternalStore(c.subscribe, c.getSnapshot);
}

/** Re-renders once a second so ages ("last frame 12 s ago") stay current. */
export function useClock(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), intervalMs);
    return () => window.clearInterval(id);
  }, [intervalMs]);
  return now;
}
