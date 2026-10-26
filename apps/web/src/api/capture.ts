import { request, requestBlob } from './http';
import type { CaptureState, CropRequest } from './types';

const wsPath = (id: string): string => `/api/v1/workspaces/${encodeURIComponent(id)}`;

/** Reserves a desktop capture. No lease is needed and control state is untouched. */
export function reserveDesktopCapture(workspaceId: string, idempotencyKey: string): Promise<CaptureState> {
  return request<CaptureState>(`${wsPath(workspaceId)}/captures`, {
    method: 'POST',
    body: { source: 'desktop' },
    idempotencyKey,
  });
}

/** Creates a private blank canvas without accessing the browser or acquiring a lease. */
export function createDrawingCapture(
  workspaceId: string,
  imageWidthPx: number,
  imageHeightPx: number,
  idempotencyKey: string,
): Promise<CaptureState> {
  return request<CaptureState>(`${wsPath(workspaceId)}/captures`, {
    method: 'POST',
    body: { source: 'drawing', imageWidthPx, imageHeightPx },
    idempotencyKey,
  });
}

/**
 * Uploads the PNG taken from the client's RFB framebuffer. `capturedAt` is the
 * client capture time; the server records its own receipt time separately.
 */
export function uploadDesktopCapture(
  captureId: string,
  png: Blob,
  capturedAt: string,
  idempotencyKey: string,
): Promise<CaptureState> {
  return request<CaptureState>(`/api/v1/captures/${encodeURIComponent(captureId)}/image`, {
    method: 'PUT',
    body: png,
    contentType: 'image/png',
    headers: { 'X-LensDock-Captured-At': capturedAt },
    idempotencyKey,
    timeoutMs: 0,
  });
}

/**
 * Captures one CDP target's viewport. `leaseId` is the lease that currently
 * holds control (the session's own HUMAN lease, or the AGENT lease's id when
 * the user explicitly accepted interrupting the Agent via `allowInterrupt`).
 * The restored lease keeps its token but gets a new leaseId and epoch.
 */
export function createViewportCapture(
  workspaceId: string,
  req: { targetId: string; leaseId: string; allowInterrupt?: boolean },
  idempotencyKey: string,
): Promise<CaptureState> {
  const body: { source: 'page-viewport'; targetId: string; leaseId: string; allowInterrupt?: boolean } = {
    source: 'page-viewport',
    targetId: req.targetId,
    leaseId: req.leaseId,
  };
  if (req.allowInterrupt) body.allowInterrupt = true;
  return request<CaptureState>(`${wsPath(workspaceId)}/captures`, {
    method: 'POST',
    body,
    idempotencyKey,
    timeoutMs: 0,
  });
}

export function getCapture(captureId: string, signal?: AbortSignal): Promise<CaptureState> {
  return request<CaptureState>(`/api/v1/captures/${encodeURIComponent(captureId)}`, { signal });
}

/** Private raw PNG of a capture (owner only). Fetched as a blob so the editor can decode it. */
export function fetchCaptureImage(captureId: string, signal?: AbortSignal): Promise<Blob> {
  return requestBlob(`/api/v1/captures/${encodeURIComponent(captureId)}/image`, { signal });
}

/** Crop creates a new capture; the parent and its annotations are untouched. */
export function cropCapture(captureId: string, rect: CropRequest, idempotencyKey: string): Promise<CaptureState> {
  return request<CaptureState>(`/api/v1/captures/${encodeURIComponent(captureId)}/crop`, {
    method: 'POST',
    body: rect,
    idempotencyKey,
  });
}
