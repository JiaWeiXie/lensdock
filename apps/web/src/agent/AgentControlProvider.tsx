import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import type { AgentControl, AgentMode } from '@/api/types';
import { agentEventsUrl, parseAgentControl, pauseAgent, putAgentMode, resumeAgent } from '@/api/workspace';
import { canOperate, useSession } from '@/state/session';
import { useWorkspace } from '@/state/workspace';
import { useAgentText } from './messages';

/**
 * Client side of the usage-mode policy (noVNC-only vs Agent collaboration) and the pause switch.
 *
 * The server is the only source of truth. The policy is derived from the workspace read (polled and
 * refreshed by event hints) plus the dedicated policy stream for instant cross-tab updates. Nothing
 * is persisted in the browser. Whenever the policy cannot be read, or is malformed, the state is
 * `unknown` and every collaboration surface stays hidden and every mutation disabled: it is never
 * assumed to be AGENT.
 */

export type AgentPolicy =
  | { kind: 'loading' }
  | { kind: 'unknown' }
  | { kind: 'known'; control: AgentControl };

/** `enabled` only for a known AGENT mode; hidden for NOVNC and for anything unknown. */
export type CollaborationState = 'enabled' | 'disabled' | 'unknown';

export type AgentAction = 'pause' | 'resume' | 'mode';

export interface AgentMutationError {
  action: AgentAction;
  error: unknown;
}

export type AgentStreamStatus = 'connecting' | 'open' | 'reconnecting';

interface AgentCtx {
  policy: AgentPolicy;
  /** null while loading or unknown. */
  mode: AgentMode | null;
  /** true only in a known AGENT mode that is paused. */
  paused: boolean;
  collaboration: CollaborationState;
  /** Drawing, screenshots, feedback, dock and editor are available. Same as `collaboration === 'enabled'`. */
  collaborationVisible: boolean;
  /** The signed-in actor may change the mode and pause/resume (human or admin). */
  canControl: boolean;
  /** Pause/resume/mode can be requested right now (known policy, right role, nothing in flight). */
  canMutate: boolean;
  busy: AgentAction | null;
  error: AgentMutationError | null;
  clearError: () => void;
  streamStatus: AgentStreamStatus;
  setMode: (mode: AgentMode) => Promise<boolean>;
  pause: () => Promise<boolean>;
  resume: () => Promise<boolean>;
}

const Ctx = createContext<AgentCtx | null>(null);

const STREAM_RETRY_MIN_MS = 2000;
const STREAM_RETRY_MAX_MS = 30000;
/** A stream-delivered policy is dropped in favour of the polled one after this many polls. */
const LIVE_POLLS = 2;

interface Live {
  control: AgentControl;
  /** Poll counter value when it arrived. */
  at: number;
}

export function AgentControlProvider({ children }: { children: ReactNode }): ReactNode {
  const { workspaceId, workspace, error: workspaceError, lastOkAt, refresh } = useWorkspace();
  const { actor } = useSession();
  const at = useAgentText();
  const [live, setLive] = useState<Live | null>(null);
  const [pollCount, setPollCount] = useState(0);
  const [busy, setBusy] = useState<AgentAction | null>(null);
  const [error, setError] = useState<AgentMutationError | null>(null);
  const [streamStatus, setStreamStatus] = useState<AgentStreamStatus>('connecting');
  const [announcement, setAnnouncement] = useState('');
  const inFlight = useRef(false);
  const mounted = useRef(true);
  const pollCountRef = useRef(0);
  const revisionRef = useRef(-1);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  // Every successful workspace read ages a stream-delivered policy.
  useEffect(() => {
    if (lastOkAt === null) return;
    pollCountRef.current += 1;
    setPollCount(pollCountRef.current);
  }, [lastOkAt]);

  const policy = useMemo<AgentPolicy>(() => {
    if (workspaceError) return { kind: 'unknown' };
    if (!workspace) return { kind: 'loading' };
    const polled = parseAgentControl(workspace.agentControl);
    if (!polled) return { kind: 'unknown' };
    const streamed = live && pollCount - live.at < LIVE_POLLS ? live.control : null;
    return { kind: 'known', control: streamed && streamed.revision > polled.revision ? streamed : polled };
  }, [workspace, workspaceError, live, pollCount]);

  const control = policy.kind === 'known' ? policy.control : null;
  useEffect(() => {
    revisionRef.current = control ? control.revision : -1;
  }, [control]);

  /** Newer-or-equal revisions only: a late answer can never roll the UI back. */
  const apply = useCallback((next: AgentControl): void => {
    if (next.revision < revisionRef.current) return;
    setLive({ control: next, at: pollCountRef.current });
  }, []);

  // Policy stream: immediate cross-tab updates. Polling above is the safety net for gaps.
  useEffect(() => {
    let source: EventSource | null = null;
    let retry: number | undefined;
    let delay = STREAM_RETRY_MIN_MS;
    let stopped = false;

    const open = (): void => {
      setStreamStatus('connecting');
      const es = new EventSource(agentEventsUrl(workspaceId), { withCredentials: true });
      source = es;
      es.onopen = (): void => {
        delay = STREAM_RETRY_MIN_MS;
        setStreamStatus('open');
      };
      es.addEventListener('agent-control', (m) => {
        let parsed: unknown;
        try {
          parsed = JSON.parse((m as MessageEvent<string>).data);
        } catch {
          return;
        }
        const next = parseAgentControl(parsed);
        if (!next) return;
        apply(next);
        void refresh();
      });
      es.onerror = (): void => {
        setStreamStatus('reconnecting');
        // A refused connection (HTTP error) closes the source for good; reopen with backoff.
        if (es.readyState === EventSource.CLOSED && !stopped) {
          es.close();
          retry = window.setTimeout(open, delay);
          delay = Math.min(delay * 2, STREAM_RETRY_MAX_MS);
        }
      };
    };
    open();
    return () => {
      stopped = true;
      window.clearTimeout(retry);
      source?.close();
    };
  }, [workspaceId, apply, refresh]);

  const mode = control?.mode ?? null;
  const paused = control !== null && control.mode === 'AGENT' && control.paused;
  const collaboration: CollaborationState = control === null ? 'unknown' : control.mode === 'AGENT' ? 'enabled' : 'disabled';
  const canControl = canOperate(actor?.role);
  const canMutate = canControl && control !== null && busy === null;

  // Announce every observed change, including ones made in another tab, once.
  const lastSaid = useRef<string | null>(null);
  useEffect(() => {
    if (policy.kind === 'loading') return;
    const key = control ? `${control.mode}/${control.paused}` : 'unknown';
    const before = lastSaid.current;
    lastSaid.current = key;
    if (before === null || before === key) return;
    if (!control) setAnnouncement(at('say.unknown'));
    else if (control.mode === 'NOVNC') setAnnouncement(at('say.modeOff'));
    else if (control.paused) setAnnouncement(at('say.paused'));
    else if (before === 'NOVNC/false') setAnnouncement(at('say.modeOn'));
    else if (before === 'AGENT/true') setAnnouncement(at('say.resumed'));
  }, [policy.kind, control, at]);

  const run = useCallback(
    async (action: AgentAction, call: () => Promise<AgentControl>): Promise<boolean> => {
      if (inFlight.current) return false;
      inFlight.current = true;
      setBusy(action);
      setError(null);
      try {
        const next = await call();
        if (mounted.current) apply(next);
        return true;
      } catch (err) {
        if (mounted.current) setError({ action, error: err });
        return false;
      } finally {
        inFlight.current = false;
        if (mounted.current) setBusy(null);
        // Success or not, the server's current answer wins.
        void refresh();
      }
    },
    [apply, refresh],
  );

  const guarded = canControl && control !== null;
  const setMode = useCallback(
    (next: AgentMode): Promise<boolean> => (guarded ? run('mode', () => putAgentMode(workspaceId, next)) : Promise.resolve(false)),
    [guarded, run, workspaceId],
  );
  const pause = useCallback(
    (): Promise<boolean> => (guarded ? run('pause', () => pauseAgent(workspaceId)) : Promise.resolve(false)),
    [guarded, run, workspaceId],
  );
  const resume = useCallback(
    (): Promise<boolean> => (guarded ? run('resume', () => resumeAgent(workspaceId)) : Promise.resolve(false)),
    [guarded, run, workspaceId],
  );
  const clearError = useCallback(() => setError(null), []);

  const value = useMemo<AgentCtx>(
    () => ({
      policy,
      mode,
      paused,
      collaboration,
      collaborationVisible: collaboration === 'enabled',
      canControl,
      canMutate,
      busy,
      error,
      clearError,
      streamStatus,
      setMode,
      pause,
      resume,
    }),
    [policy, mode, paused, collaboration, canControl, canMutate, busy, error, clearError, streamStatus, setMode, pause, resume],
  );

  return (
    <Ctx.Provider value={value}>
      {children}
      <div className="sr-only" role="status" aria-live="polite" aria-atomic="true">
        {announcement}
      </div>
    </Ctx.Provider>
  );
}

export function useAgentControl(): AgentCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error('useAgentControl outside AgentControlProvider');
  return v;
}
