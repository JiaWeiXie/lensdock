/** Immutable undo/redo history with optional coalescing of rapid edits (typing a comment). */

export interface History<T> {
  past: readonly T[];
  present: T;
  future: readonly T[];
  coalesceKey: string | null;
  at: number;
}

export const HISTORY_LIMIT = 200;
const COALESCE_WINDOW_MS = 1500;

export function createHistory<T>(initial: T): History<T> {
  return { past: [], present: initial, future: [], coalesceKey: null, at: 0 };
}

export function commit<T>(
  h: History<T>,
  next: T,
  opts: { coalesceKey?: string; now?: number; limit?: number } = {},
): History<T> {
  if (Object.is(next, h.present)) return h;
  const now = opts.now ?? Date.now();
  const key = opts.coalesceKey ?? null;
  if (key !== null && h.coalesceKey === key && now - h.at < COALESCE_WINDOW_MS && h.past.length > 0) {
    return { ...h, present: next, future: [], at: now };
  }
  const limit = opts.limit ?? HISTORY_LIMIT;
  const past = [...h.past, h.present];
  return { past: past.length > limit ? past.slice(past.length - limit) : past, present: next, future: [], coalesceKey: key, at: now };
}

export function canUndo<T>(h: History<T>): boolean {
  return h.past.length > 0;
}

export function canRedo<T>(h: History<T>): boolean {
  return h.future.length > 0;
}

export function undo<T>(h: History<T>): History<T> {
  const prev = h.past[h.past.length - 1];
  if (prev === undefined) return h;
  return { past: h.past.slice(0, -1), present: prev, future: [h.present, ...h.future], coalesceKey: null, at: 0 };
}

export function redo<T>(h: History<T>): History<T> {
  const [next, ...rest] = h.future;
  if (next === undefined) return h;
  return { past: [...h.past, h.present], present: next, future: rest, coalesceKey: null, at: 0 };
}
