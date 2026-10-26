import { request, requestBlob } from './http';
import type {
  Annotation,
  DeliveryDetail,
  Draft,
  DraftDetail,
  DraftList,
  FeedbackList,
  PublishedFeedback,
  RevisionList,
  TaskFields,
  DeliveryState,
} from './types';

const fb = (id: string): string => `/api/v1/feedback/${encodeURIComponent(id)}`;

// ------------------------------------------------------------------ drafts

export function createDraft(
  workspaceId: string,
  captureId: string,
  task: TaskFields,
  idempotencyKey: string,
): Promise<Draft> {
  return request<Draft>('/api/v1/feedback', {
    method: 'POST',
    body: { workspaceId, captureId, task },
    idempotencyKey,
  });
}

export function listDrafts(workspaceId: string, signal?: AbortSignal): Promise<DraftList> {
  return request<DraftList>('/api/v1/feedback/drafts', { query: { workspaceId }, signal });
}

export function getDraft(feedbackId: string, signal?: AbortSignal): Promise<DraftDetail> {
  return request<DraftDetail>(`${fb(feedbackId)}/draft`, { signal });
}

export function deleteDraft(feedbackId: string): Promise<void> {
  return request<void>(`${fb(feedbackId)}/draft`, { method: 'DELETE' });
}

export interface DraftPatch {
  task?: TaskFields;
  annotations?: Annotation[];
}

/** Saves a draft under an ETag precondition. 412 PRECONDITION_FAILED means someone else changed it. */
export function patchDraft(feedbackId: string, etag: string, patch: DraftPatch, idempotencyKey: string): Promise<Draft> {
  return request<Draft>(fb(feedbackId), {
    method: 'PATCH',
    body: patch,
    headers: { 'If-Match': etag },
    idempotencyKey,
  });
}

/** Uploads the client-rendered annotated PNG bound to the exact draft ETag. */
export function uploadDraftPreview(
  feedbackId: string,
  etag: string,
  png: Blob,
  idempotencyKey: string,
): Promise<DraftDetail> {
  return request<DraftDetail>(`${fb(feedbackId)}/draft/preview`, {
    method: 'PUT',
    body: png,
    contentType: 'image/png',
    headers: { 'If-Match': etag },
    idempotencyKey,
    timeoutMs: 0,
  });
}


export function publishDraft(feedbackId: string, etag: string, idempotencyKey: string): Promise<PublishedFeedback> {
  return request<PublishedFeedback>(`${fb(feedbackId)}/publish`, {
    method: 'POST',
    body: { humanApproved: true, dispatch: true },
    headers: { 'If-Match': etag },
    idempotencyKey,
    timeoutMs: 0,
  });
}

// --------------------------------------------------------------- published

export function listFeedback(
  workspaceId: string,
  opts: { state?: DeliveryState; cursor?: string; limit?: number; signal?: AbortSignal } = {},
): Promise<FeedbackList> {
  return request<FeedbackList>('/api/v1/feedback', {
    query: { workspaceId, state: opts.state, cursor: opts.cursor, limit: opts.limit },
    signal: opts.signal,
  });
}

export function listRevisions(feedbackId: string, signal?: AbortSignal): Promise<RevisionList> {
  return request<RevisionList>(`${fb(feedbackId)}/revisions`, { signal });
}

export function getPublished(feedbackId: string, revision: number, signal?: AbortSignal): Promise<PublishedFeedback> {
  return request<PublishedFeedback>(fb(feedbackId), { query: { revision }, signal });
}

/** Starts revision N+1 as a new draft; published content is never edited in place. */
export function createRevision(feedbackId: string, idempotencyKey: string, captureId?: string): Promise<Draft> {
  return request<Draft>(`${fb(feedbackId)}/revisions`, {
    method: 'POST',
    body: captureId ? { captureId } : {},
    idempotencyKey,
  });
}

export function artifactUrl(feedbackId: string, name: string, revision: number): string {
  return `${fb(feedbackId)}/artifacts/${encodeURIComponent(name)}?revision=${revision}`;
}

export function bundleUrl(feedbackId: string, revision: number): string {
  return `${fb(feedbackId)}/bundle?revision=${revision}`;
}

export function fetchArtifactBlob(feedbackId: string, name: string, revision: number, signal?: AbortSignal): Promise<Blob> {
  return requestBlob(`${fb(feedbackId)}/artifacts/${encodeURIComponent(name)}`, { query: { revision }, signal });
}

// ---------------------------------------------------------------- delivery

export function getDelivery(feedbackId: string, revision: number, signal?: AbortSignal): Promise<DeliveryDetail> {
  return request<DeliveryDetail>(`${fb(feedbackId)}/delivery`, { query: { revision }, signal });
}

export function dispatchFeedback(
  feedbackId: string,
  revision: number,
  idempotencyKey: string,
): Promise<unknown> {
  return request<unknown>(`${fb(feedbackId)}/dispatch`, {
    method: 'POST',
    body: { revision },
    idempotencyKey,
  });
}

export function verifyFeedback(
  feedbackId: string,
  revision: number,
  outcome: 'verified' | 'rejected',
  reason: string | undefined,
  idempotencyKey: string,
): Promise<unknown> {
  const body: { revision: number; outcome: 'verified' | 'rejected'; reason?: string } = { revision, outcome };
  if (reason) body.reason = reason;
  return request<unknown>(`${fb(feedbackId)}/verify`, { method: 'POST', body, idempotencyKey });
}

export function cancelFeedback(
  feedbackId: string,
  revision: number,
  reason: string | undefined,
  idempotencyKey: string,
): Promise<unknown> {
  const body: { revision: number; reason?: string } = { revision };
  if (reason) body.reason = reason;
  return request<unknown>(`${fb(feedbackId)}/cancel`, { method: 'POST', body, idempotencyKey });
}
