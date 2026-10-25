import { request } from './http';
import type { AuditList, SystemInfo } from './types';

/** Gateway/storage/release/update information (human and admin roles). */
export function getSystem(signal?: AbortSignal): Promise<SystemInfo> {
  return request<SystemInfo>('/api/v1/system', { signal });
}

/** Audit trail (administrator only). `after` is the last seen sequence number. */
export function listAudit(
  workspaceId: string,
  opts: { after?: number; limit?: number; signal?: AbortSignal } = {},
): Promise<AuditList> {
  return request<AuditList>('/api/v1/audit', {
    query: { workspaceId, after: opts.after, limit: opts.limit },
    signal: opts.signal,
  });
}
