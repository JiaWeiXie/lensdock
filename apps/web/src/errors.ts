import { ApiError } from '@/api/http';
import type { MessageKey, MessageParams } from '@/i18n';

/**
 * Codes the UI explains in the user's language. Anything else falls back to
 * the server's own message so nothing is hidden or reworded into a guess.
 */
export const ERROR_CODES = [
  'NETWORK',
  'TIMEOUT',
  'UNAUTHENTICATED',
  'FORBIDDEN',
  'LEASE_REQUIRED',
  'LEASE_CONFLICT',
  'LEASE_ENDED',
  'LEASE_MISMATCH',
  'EPOCH_MISMATCH',
  'GENERATION_MISMATCH',
  'IDEMPOTENCY_REPLAY',
  'MAINTENANCE',
  'RUNTIME_NOT_READY',
  'UPSTREAM_UNAVAILABLE',
  'TOO_MANY_CONNECTIONS',
  'WRONG_LEASE_MODE',
  'TARGET_NOT_FOUND',
  'CAPTURE_FAILED',
  'STALE_CAPTURE',
  'PRECONDITION_FAILED',
  'PRECONDITION_REQUIRED',
  'PREVIEW_REQUIRED',
  'ANNOTATION_INVALID',
  'RESIZE_UNSUPPORTED',
  'BODY_TOO_LARGE',
  'FILE_EXISTS',
  'INVALID_NAME',
  'INVALID_UTF8',
  'FEEDBACK_CONFLICT',
  'AGENT_NOT_CONFIGURED',
  'AGENT_AMBIGUOUS',
  'RESOURCE_LIMIT',
  'NOT_FOUND',
  'TAKEOVER_TIMEOUT',
] as const;

export type KnownErrorCode = (typeof ERROR_CODES)[number];

function isKnown(code: string): code is KnownErrorCode {
  return (ERROR_CODES as readonly string[]).includes(code);
}

type T = (key: MessageKey, params?: MessageParams) => string;

/** One user-facing sentence for any thrown value, with the request id for support. */
export function describeError(err: unknown, t: T): string {
  if (err instanceof ApiError) {
    const base = isKnown(err.code) ? t(`err.${err.code}`) : `${err.message} (${err.code})`;
    return err.requestId ? `${base} [${err.requestId}]` : base;
  }
  if (err instanceof Error) return err.message;
  return t('err.UNKNOWN');
}
