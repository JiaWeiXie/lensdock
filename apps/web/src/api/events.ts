import type { AuditEvent } from './types';

/**
 * Server-sent events feed. SSE is a change notification, not a data source:
 * subscribers re-read the authoritative state over REST. The browser resends
 * Last-Event-ID on reconnect; bounded polling in the stores covers gaps.
 */

export type FeedStatus = 'connecting' | 'open' | 'reconnecting';

export type LiveEvent = Pick<AuditEvent, 'seq' | 'id' | 'time' | 'category' | 'action' | 'result' | 'actorId'> &
  Partial<Pick<AuditEvent, 'subjectType' | 'subjectId' | 'revision' | 'agentId' | 'workspaceId'>>;

export class EventFeed {
  private source: EventSource | null = null;
  private readonly listeners = new Set<(e: LiveEvent) => void>();
  private readonly statusListeners = new Set<(s: FeedStatus) => void>();
  private status: FeedStatus = 'connecting';

  constructor(private readonly workspaceId: string) {}

  start(): void {
    if (this.source) return;
    this.setStatus('connecting');
    const url = `/api/v1/events?workspaceId=${encodeURIComponent(this.workspaceId)}`;
    const es = new EventSource(url, { withCredentials: true });
    this.source = es;
    es.onopen = (): void => this.setStatus('open');
    es.onerror = (): void => this.setStatus('reconnecting');
    es.onmessage = (m: MessageEvent<string>): void => this.dispatch(m.data);
  }

  stop(): void {
    this.source?.close();
    this.source = null;
    this.listeners.clear();
    this.statusListeners.clear();
  }

  onEvent(fn: (e: LiveEvent) => void): () => void {
    this.listeners.add(fn);
    return () => {
      this.listeners.delete(fn);
    };
  }

  onStatus(fn: (s: FeedStatus) => void): () => void {
    this.statusListeners.add(fn);
    fn(this.status);
    return () => {
      this.statusListeners.delete(fn);
    };
  }

  private setStatus(s: FeedStatus): void {
    this.status = s;
    for (const l of this.statusListeners) l(s);
  }

  private dispatch(data: string): void {
    let parsed: unknown;
    try {
      parsed = JSON.parse(data);
    } catch {
      return;
    }
    if (typeof parsed !== 'object' || parsed === null) return;
    const e = parsed as Partial<LiveEvent>;
    if (typeof e.category !== 'string' || typeof e.action !== 'string') return;
    const ev: LiveEvent = {
      seq: typeof e.seq === 'number' ? e.seq : 0,
      id: typeof e.id === 'string' ? e.id : '',
      time: typeof e.time === 'string' ? e.time : new Date().toISOString(),
      category: e.category,
      action: e.action,
      result: typeof e.result === 'string' ? e.result : '',
      actorId: typeof e.actorId === 'string' ? e.actorId : '',
    };
    if (typeof e.subjectType === 'string') ev.subjectType = e.subjectType;
    if (typeof e.subjectId === 'string') ev.subjectId = e.subjectId;
    if (typeof e.revision === 'number') ev.revision = e.revision;
    if (typeof e.agentId === 'string') ev.agentId = e.agentId;
    for (const l of this.listeners) l(ev);
  }
}
