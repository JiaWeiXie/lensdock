import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { EventFeed, type FeedStatus, type LiveEvent } from '@/api/events';
import type { ApiError } from '@/api/http';
import type { Workspace } from '@/api/types';
import { getWorkspace } from '@/api/workspace';

const POLL_VISIBLE_MS = 3000;
const POLL_HIDDEN_MS = 10000;
const MAX_EVENTS = 100;

interface WorkspaceCtx {
  workspaceId: string;
  workspace: Workspace | null;
  /** Last polling failure; null while the Gateway answers. */
  error: ApiError | null;
  /** Wall-clock ms of the last successful workspace read. */
  lastOkAt: number | null;
  feedStatus: FeedStatus;
  /** Newest-first live events (secrets are never published on this stream). */
  events: LiveEvent[];
  /** Increments for every event received; stores use it as a refetch trigger. */
  eventTick: number;
  refresh: () => Promise<void>;
}

const Ctx = createContext<WorkspaceCtx | null>(null);

export function WorkspaceProvider({ workspaceId, children }: { workspaceId: string; children: ReactNode }): ReactNode {
  const [workspace, setWorkspace] = useState<Workspace | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [lastOkAt, setLastOkAt] = useState<number | null>(null);
  const [feedStatus, setFeedStatus] = useState<FeedStatus>('connecting');
  const [events, setEvents] = useState<LiveEvent[]>([]);
  const [eventTick, setEventTick] = useState(0);
  // Overlapping reads (poll tick + SSE hint) are never aborted: cancelling a request the server is
  // already answering surfaces as a failed network request. A sequence number drops stale answers.
  const seq = useRef(0);
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const refresh = useCallback(async (): Promise<void> => {
    seq.current += 1;
    const mine = seq.current;
    try {
      const ws = await getWorkspace(workspaceId);
      if (!mounted.current || mine !== seq.current) return;
      setWorkspace(ws);
      setError(null);
      setLastOkAt(Date.now());
    } catch (err) {
      if (!mounted.current || mine !== seq.current) return;
      setError(err as ApiError);
    }
  }, [workspaceId]);

  // Bounded polling: always runs, faster while the tab is visible.
  useEffect(() => {
    let timer: number | undefined;
    let stopped = false;
    const tick = (): void => {
      void refresh().finally(() => {
        if (stopped) return;
        timer = window.setTimeout(tick, document.visibilityState === 'visible' ? POLL_VISIBLE_MS : POLL_HIDDEN_MS);
      });
    };
    tick();
    return () => {
      stopped = true;
      window.clearTimeout(timer);
    };
  }, [refresh]);

  // Collaboration events are unavailable in NOVNC mode; the policy stream and
  // workspace polling keep ordinary browser control current without retries.
  const collaboration = workspace?.agentControl?.mode === 'AGENT';
  useEffect(() => {
    if (!collaboration) return;
    const feed = new EventFeed(workspaceId);
    let debounce: number | undefined;
    feed.onStatus(setFeedStatus);
    feed.onEvent((e) => {
      setEvents((prev) => [e, ...prev].slice(0, MAX_EVENTS));
      setEventTick((n) => n + 1);
      window.clearTimeout(debounce);
      debounce = window.setTimeout(() => void refresh(), 150);
    });
    feed.start();
    return () => {
      window.clearTimeout(debounce);
      feed.stop();
    };
  }, [workspaceId, refresh, collaboration]);

  const value = useMemo<WorkspaceCtx>(
    () => ({ workspaceId, workspace, error, lastOkAt, feedStatus, events, eventTick, refresh }),
    [workspaceId, workspace, error, lastOkAt, feedStatus, events, eventTick, refresh],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useWorkspace(): WorkspaceCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error('useWorkspace outside WorkspaceProvider');
  return v;
}
