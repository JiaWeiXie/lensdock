import { newKey } from '@/util/async';

/**
 * Same-origin fetch wrapper for the Gateway API.
 *
 * - Authentication is the HttpOnly session cookie; nothing credential-like is
 *   ever put in a URL, localStorage or sessionStorage.
 * - Every state-changing call carries X-CSRF-Token and an Idempotency-Key.
 * - Errors normalise to ApiError. Both the flat {code,message,requestId} and
 *   the nested {error:{...}} envelopes are accepted.
 */

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId: string | undefined;

  constructor(status: number, code: string, message: string, requestId?: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

let csrfToken = '';
let unauthenticatedHandler: (() => void) | null = null;

export function setCsrfToken(token: string): void {
  csrfToken = token;
}

export function onUnauthenticated(handler: (() => void) | null): void {
  unauthenticatedHandler = handler;
}

const SAFE_METHODS: Record<string, true> = { GET: true, HEAD: true, OPTIONS: true };

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
  body?: unknown;
  contentType?: string;
  headers?: Record<string, string>;
  query?: Record<string, string | number | undefined>;
  signal?: AbortSignal | undefined;
  timeoutMs?: number;
  /** Sends an Idempotency-Key. Pass a string to reuse a key across retries. */
  idempotencyKey?: string | true;
  /** Do not treat 401 as an expired session (used by the login probe). */
  quiet401?: boolean;
}

export interface ApiResponse<T> {
  data: T;
  headers: Headers;
  status: number;
}

export function buildUrl(path: string, query?: Record<string, string | number | undefined>): string {
  if (!query) return path;
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(query)) {
    if (v !== undefined && v !== '') qs.set(k, String(v));
  }
  const s = qs.toString();
  return s ? `${path}?${s}` : path;
}

function isBodyInit(b: unknown): b is Blob | ArrayBuffer | string | FormData | URLSearchParams {
  return (
    typeof b === 'string' ||
    b instanceof Blob ||
    b instanceof ArrayBuffer ||
    b instanceof FormData ||
    b instanceof URLSearchParams
  );
}

async function parseError(res: Response): Promise<ApiError> {
  let code = `HTTP_${res.status}`;
  let message = res.statusText || `HTTP ${res.status}`;
  let requestId: string | undefined = res.headers.get('X-Request-Id') ?? undefined;
  try {
    const text = await res.text();
    if (text) {
      const parsed: unknown = JSON.parse(text);
      const flat = parsed as { code?: unknown; message?: unknown; requestId?: unknown; error?: unknown };
      const nested = (flat.error ?? null) as { code?: unknown; message?: unknown; requestId?: unknown } | null;
      const src = nested && typeof nested === 'object' ? nested : flat;
      if (typeof src.code === 'string') code = src.code;
      if (typeof src.message === 'string') message = src.message;
      if (typeof src.requestId === 'string') requestId = src.requestId;
    }
  } catch {
    // Non-JSON error body: keep the status line.
  }
  return new ApiError(res.status, code, message, requestId);
}

export async function requestFull<T>(path: string, opts: RequestOptions = {}): Promise<ApiResponse<T>> {
  const method = opts.method ?? 'GET';
  const headers = new Headers(opts.headers);
  headers.set('Accept', 'application/json');
  if (!SAFE_METHODS[method] && csrfToken) headers.set('X-CSRF-Token', csrfToken);
  if (opts.idempotencyKey !== undefined) {
    headers.set('Idempotency-Key', opts.idempotencyKey === true ? newKey() : opts.idempotencyKey);
  }

  let body: BodyInit | null = null;
  if (opts.body !== undefined) {
    if (isBodyInit(opts.body)) {
      body = opts.body;
      if (opts.contentType) headers.set('Content-Type', opts.contentType);
    } else {
      body = JSON.stringify(opts.body);
      headers.set('Content-Type', 'application/json');
    }
  }

  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(new DOMException('Timed out', 'TimeoutError')), opts.timeoutMs ?? 30000);
  const outer = opts.signal;
  const onOuterAbort = (): void => controller.abort(outer?.reason);
  if (outer) {
    if (outer.aborted) controller.abort(outer.reason);
    else outer.addEventListener('abort', onOuterAbort, { once: true });
  }

  let res: Response;
  try {
    res = await fetch(buildUrl(path, opts.query), {
      method,
      headers,
      body,
      credentials: 'same-origin',
      cache: 'no-store',
      signal: controller.signal,
    });
  } catch (err) {
    if (outer?.aborted) throw err;
    if (controller.signal.aborted) throw new ApiError(0, 'TIMEOUT', 'request timed out');
    throw new ApiError(0, 'NETWORK', err instanceof Error ? err.message : 'network error');
  } finally {
    clearTimeout(timeout);
    outer?.removeEventListener('abort', onOuterAbort);
  }

  if (!res.ok) {
    const err = await parseError(res);
    if (res.status === 401 && !opts.quiet401) unauthenticatedHandler?.();
    throw err;
  }
  // Always read the response to its end so the browser sees every request complete; leaving a
  // body unread (204 or non-JSON) can be reported as an aborted request when the caller moves on.
  if (res.status === 204 || res.status === 205) {
    await res.arrayBuffer();
    return { data: undefined as T, headers: res.headers, status: res.status };
  }
  const type = res.headers.get('Content-Type') ?? '';
  if (type.includes('application/json')) {
    return { data: (await res.json()) as T, headers: res.headers, status: res.status };
  }
  await res.arrayBuffer();
  return { data: undefined as T, headers: res.headers, status: res.status };
}

export async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  return (await requestFull<T>(path, opts)).data;
}

/** Fetches a binary body (PNG, ...) with the session cookie. */
export async function requestBlob(path: string, opts: RequestOptions = {}): Promise<Blob> {
  const res = await fetch(buildUrl(path, opts.query), {
    credentials: 'same-origin',
    cache: 'no-store',
    signal: opts.signal ?? null,
  });
  if (!res.ok) {
    const err = await parseError(res);
    if (res.status === 401 && !opts.quiet401) unauthenticatedHandler?.();
    throw err;
  }
  return res.blob();
}

/** XHR upload with progress; fetch has no upload progress events. */
export function uploadWithProgress(
  path: string,
  body: Blob,
  opts: {
    method: 'PUT' | 'POST';
    contentType: string;
    headers?: Record<string, string>;
    query?: Record<string, string | number | undefined>;
    signal?: AbortSignal;
    onProgress?: (loaded: number, total: number) => void;
  },
): Promise<void> {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  const xhr = new XMLHttpRequest();
  xhr.open(opts.method, buildUrl(path, opts.query));
  xhr.withCredentials = true;
  xhr.setRequestHeader('Content-Type', opts.contentType);
  if (csrfToken) xhr.setRequestHeader('X-CSRF-Token', csrfToken);
  xhr.setRequestHeader('Idempotency-Key', newKey());
  for (const [k, v] of Object.entries(opts.headers ?? {})) xhr.setRequestHeader(k, v);
  xhr.upload.onprogress = (e): void => {
    if (e.lengthComputable) opts.onProgress?.(e.loaded, e.total);
  };
  xhr.onerror = (): void => reject(new ApiError(0, 'NETWORK', 'network error during upload'));
  xhr.onabort = (): void => reject(new DOMException('Aborted', 'AbortError'));
  xhr.onload = (): void => {
    if (xhr.status >= 200 && xhr.status < 300) {
      resolve();
      return;
    }
    let code = `HTTP_${xhr.status}`;
    let message = xhr.statusText || `HTTP ${xhr.status}`;
    let requestId: string | undefined;
    try {
      const parsed = JSON.parse(xhr.responseText) as { code?: string; message?: string; requestId?: string; error?: { code?: string; message?: string; requestId?: string } };
      const src = parsed.error ?? parsed;
      if (typeof src.code === 'string') code = src.code;
      if (typeof src.message === 'string') message = src.message;
      requestId = src.requestId;
    } catch {
      // keep status line
    }
    if (xhr.status === 401) unauthenticatedHandler?.();
    reject(new ApiError(xhr.status, code, message, requestId));
  };
  opts.signal?.addEventListener('abort', () => xhr.abort(), { once: true });
  xhr.send(body);
  return promise;
}
