/** Resolves after `ms`, or rejects with an AbortError when `signal` aborts. */
export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  if (signal?.aborted) {
    reject(new DOMException('Aborted', 'AbortError'));
    return promise;
  }
  const onAbort = (): void => {
    clearTimeout(timer);
    reject(new DOMException('Aborted', 'AbortError'));
  };
  const timer = setTimeout(() => {
    signal?.removeEventListener('abort', onAbort);
    resolve();
  }, ms);
  signal?.addEventListener('abort', onAbort, { once: true });
  return promise;
}

export function isAbortError(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'AbortError';
}

/**
 * Bounded exponential backoff. Returns the delay for 0-based `attempt`,
 * capped at `maxMs`, with up to 20% jitter.
 */
export function backoffDelay(attempt: number, baseMs = 1000, maxMs = 15000, random: () => number = Math.random): number {
  const raw = Math.min(maxMs, baseMs * 2 ** attempt);
  return Math.round(raw * (1 + 0.2 * random()));
}

export function newKey(): string {
  return crypto.randomUUID();
}

export interface SingleFlight<T> {
  /** Starts `fn`, or joins the run that is already pending. Every caller gets the same outcome. */
  run: () => Promise<T>;
  readonly pending: boolean;
}

/** Collapses concurrent callers onto one in-flight call; a settled call lets the next `run` start afresh. */
export function createSingleFlight<T>(fn: () => Promise<T>): SingleFlight<T> {
  let current: Promise<T> | null = null;
  return {
    run: () => {
      if (!current) {
        current = (async () => fn())().finally(() => {
          current = null;
        });
      }
      return current;
    },
    get pending() {
      return current !== null;
    },
  };
}
