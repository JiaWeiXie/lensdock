import { ApiError, request, uploadWithProgress, type RequestOptions } from './http';
import type {
  AgentControl,
  AgentMode,
  AgentSetup,
  FileArea,
  FileListing,
  Handoff,
  Lease,
  LeaseGrant,
  LeaseMode,
  ResolutionInfo,
  RuntimeHealth,
  TakeoverPending,
  TargetsResponse,
  Workspace,
} from './types';

const ws = (id: string): string => `/api/v1/workspaces/${encodeURIComponent(id)}`;

export async function listWorkspaces(signal?: AbortSignal): Promise<Workspace[]> {
  const res = await request<{ workspaces: Workspace[] }>('/api/v1/workspaces', { signal });
  return res.workspaces;
}

export function getWorkspace(id: string, signal?: AbortSignal): Promise<Workspace> {
  return request<Workspace>(ws(id), { signal });
}

export function getHealth(id: string, signal?: AbortSignal): Promise<RuntimeHealth> {
  return request<RuntimeHealth>(`${ws(id)}/health`, { signal });
}

export function listTargets(id: string, signal?: AbortSignal): Promise<TargetsResponse> {
  return request<TargetsResponse>(`${ws(id)}/targets`, { signal });
}

// ------------------------------------------------------------ agent control

/**
 * Strict reader for the usage-mode policy. Anything that is not exactly the documented shape is
 * `null`, so callers fail closed instead of assuming AGENT.
 */
export function parseAgentControl(value: unknown): AgentControl | null {
  if (typeof value !== 'object' || value === null) return null;
  const v = value as Record<string, unknown>;
  if (v.mode !== 'NOVNC' && v.mode !== 'AGENT') return null;
  if (typeof v.paused !== 'boolean') return null;
  if (typeof v.revision !== 'number' || !Number.isInteger(v.revision) || v.revision < 0) return null;
  if (typeof v.updatedAt !== 'string') return null;
  return { mode: v.mode, paused: v.paused, revision: v.revision, updatedAt: v.updatedAt };
}

async function agentRequest(id: string, path: string, init: RequestOptions): Promise<AgentControl> {
  const parsed = parseAgentControl(await request<unknown>(`${ws(id)}/agent${path}`, init));
  if (!parsed) throw new ApiError(502, 'UPSTREAM_UNAVAILABLE', 'malformed agent policy response');
  return parsed;
}

export function getAgentControl(id: string, signal?: AbortSignal): Promise<AgentControl> {
  return agentRequest(id, '', { signal });
}

/** Human/admin only. NOVNC disconnects the Agent; AGENT starts unpaused and never revives an old lease. */
export function putAgentMode(id: string, mode: AgentMode): Promise<AgentControl> {
  return agentRequest(id, '', { method: 'PUT', body: { mode }, idempotencyKey: true });
}

/** Human/admin only. Takes effect immediately; there is no confirmation step. */
export function pauseAgent(id: string): Promise<AgentControl> {
  return agentRequest(id, '/pause', { method: 'POST', body: {}, idempotencyKey: true });
}

/** Human/admin only. Allows a fresh Agent lease; it never revives an old one. */
export function resumeAgent(id: string): Promise<AgentControl> {
  return agentRequest(id, '/resume', { method: 'POST', body: {}, idempotencyKey: true });
}

export async function getAgentSetup(id: string, signal?: AbortSignal): Promise<AgentSetup> {
  const res = await request<unknown>(`${ws(id)}/agent/setup`, { signal });
  const prompt = typeof res === 'object' && res !== null ? (res as Record<string, unknown>).prompt : undefined;
  if (typeof prompt !== 'string' || prompt.trim() === '') {
    throw new ApiError(502, 'UPSTREAM_UNAVAILABLE', 'malformed agent setup response');
  }
  return { prompt };
}

/** Same-origin SSE URL for the policy notification stream (carries no feedback or private data). */
export function agentEventsUrl(id: string): string {
  return `${ws(id)}/agent/events`;
}

// ------------------------------------------------------------------ leases

export async function acquireLease(
  id: string,
  mode: LeaseMode,
  opts: { ttlSeconds?: number; takeoverId?: string; idempotencyKey: string },
): Promise<Lease> {
  const headers: Record<string, string> = {};
  if (opts.takeoverId) headers['X-LensDock-Takeover-ID'] = opts.takeoverId;
  const body: { mode: LeaseMode; ttlSeconds?: number } = { mode };
  if (opts.ttlSeconds !== undefined) body.ttlSeconds = opts.ttlSeconds;
  const res = await request<LeaseGrant>(`${ws(id)}/leases`, {
    method: 'POST',
    body,
    headers,
    idempotencyKey: opts.idempotencyKey,
  });
  return res.lease;
}

export function renewLease(id: string, lease: Lease, signal?: AbortSignal): Promise<Lease> {
  return request<Lease>(`${ws(id)}/leases/${encodeURIComponent(lease.leaseId)}/renew`, {
    method: 'POST',
    body: { epoch: lease.epoch, runtimeGeneration: lease.runtimeGeneration },
    signal,
    timeoutMs: 8000,
  });
}

export function releaseLease(id: string, leaseId: string): Promise<void> {
  return request<void>(`${ws(id)}/leases/${encodeURIComponent(leaseId)}`, { method: 'DELETE' });
}

/** The lease the Gateway holds for this session (after capture restores a new epoch). */
export function currentLease(id: string, signal?: AbortSignal): Promise<Lease> {
  return request<Lease>(`${ws(id)}/leases/current`, { signal });
}

// ---------------------------------------------------------------- takeover

export function requestTakeover(
  id: string,
  reason: string,
  force: boolean,
  idempotencyKey: string,
): Promise<TakeoverPending> {
  return request<TakeoverPending>(`${ws(id)}/takeover`, {
    method: 'POST',
    body: { reason, force },
    idempotencyKey,
  });
}

export function getTakeover(id: string, signal?: AbortSignal): Promise<Handoff> {
  return request<Handoff>(`${ws(id)}/takeover`, { signal });
}

export function cancelTakeover(id: string): Promise<void> {
  return request<void>(`${ws(id)}/takeover`, { method: 'DELETE' });
}

// --------------------------------------------------------------- clipboard

/**
 * Lease-bound requests pin the exact epoch and runtime generation the UI
 * believes it holds. After a page capture restores control under a new epoch,
 * a stale value fails with 412 instead of acting on the wrong lease.
 */
export type LeaseFence = Pick<Lease, 'epoch' | 'runtimeGeneration'>;

export function leaseHeaders(lease: LeaseFence): Record<string, string> {
  return {
    'X-LensDock-Lease-Epoch': String(lease.epoch),
    'X-LensDock-Runtime-Generation': lease.runtimeGeneration,
  };
}

/** Writes UTF-8 text to the remote X11 clipboard. Requires the session's HUMAN lease. */
export function putClipboard(id: string, text: string, lease: LeaseFence): Promise<void> {
  return request<void>(`${ws(id)}/clipboard`, {
    method: 'PUT',
    body: text,
    contentType: 'text/plain; charset=utf-8',
    headers: leaseHeaders(lease),
    idempotencyKey: true,
    timeoutMs: 0,
  });
}

// ------------------------------------------------------------------- files

export function listFiles(id: string, area: FileArea, signal?: AbortSignal): Promise<FileListing> {
  return request<FileListing>(`${ws(id)}/files`, { query: { area }, signal });
}

export function deleteFile(id: string, area: FileArea, name: string): Promise<void> {
  return request<void>(`${ws(id)}/files/${area}/${encodeURIComponent(name)}`, { method: 'DELETE' });
}

/** Same-origin URL the browser follows on an explicit click; the response is Content-Disposition: attachment. */
export function fileDownloadUrl(id: string, area: FileArea, name: string): string {
  return `${ws(id)}/files/${area}/${encodeURIComponent(name)}`;
}

export function uploadFile(
  id: string,
  file: File,
  opts: { overwrite: boolean; signal?: AbortSignal; onProgress?: (loaded: number, total: number) => void },
): Promise<void> {
  return uploadWithProgress(`${ws(id)}/files/uploads/${encodeURIComponent(file.name)}`, file, {
    method: 'PUT',
    contentType: 'application/octet-stream',
    query: opts.overwrite ? { overwrite: 1 } : {},
    ...(opts.signal ? { signal: opts.signal } : {}),
    ...(opts.onProgress ? { onProgress: opts.onProgress } : {}),
  });
}

// -------------------------------------------------------------- resolution

export function getResolution(id: string, signal?: AbortSignal): Promise<ResolutionInfo> {
  return request<ResolutionInfo>(`${ws(id)}/resolution`, { signal });
}

/** Resizes the remote X screen (HUMAN lease). The response is the size read back from the server. */
export function putResolution(
  id: string,
  size: { width: number; height: number },
  lease: LeaseFence,
): Promise<{ width: number; height: number }> {
  return request<{ width: number; height: number }>(`${ws(id)}/resolution`, {
    method: 'PUT',
    body: size,
    headers: leaseHeaders(lease),
    idempotencyKey: true,
  });
}

// ------------------------------------------------------------------- vnc

/**
 * Same-origin WebSocket URL for the RFB stream. Auth is the session cookie;
 * no credential is in the URL. The control stream additionally pins the exact
 * lease epoch and runtime generation so a stale tab or a pre-capture
 * reconnect cannot attach to a newer epoch.
 */
export function vncUrl(id: string, mode: 'view'): string;
export function vncUrl(id: string, mode: 'control', lease: Pick<Lease, 'epoch' | 'runtimeGeneration'>): string;
export function vncUrl(id: string, mode: 'view' | 'control', lease?: Pick<Lease, 'epoch' | 'runtimeGeneration'>): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const base = `${proto}//${location.host}${ws(id)}/vnc/${mode}`;
  if (mode === 'control' && lease) {
    return `${base}?epoch=${lease.epoch}&generation=${encodeURIComponent(lease.runtimeGeneration)}`;
  }
  return base;
}
