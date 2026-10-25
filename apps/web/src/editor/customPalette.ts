/**
 * The user's own colours, remembered in this browser (localStorage). Everything read back is validated:
 * only `#RRGGBB` strings survive, lower-cased, de-duplicated and capped, so a hand-edited or corrupted
 * value can never reach the annotation model.
 */
import { useSyncExternalStore } from 'react';
import { PALETTE } from './model';

export const PALETTE_STORAGE_KEY = 'lensdock.editor.palette.v1';
export const MAX_CUSTOM_COLORS = 12;
/** Anything longer than this is not a palette we wrote; it is ignored instead of parsed. */
const MAX_STORED_CHARS = 4096;

const HEX = /^#[0-9a-fA-F]{6}$/;

export function normalizeHex(value: unknown): string | null {
  return typeof value === 'string' && HEX.test(value) ? value.toLowerCase() : null;
}

/** True for the built-in swatches, which are always offered and never stored. */
export function isBuiltInColor(color: string): boolean {
  const c = color.toLowerCase();
  return PALETTE.some((p) => p.toLowerCase() === c);
}

/** Parses a stored palette. Anything malformed yields the valid subset, never an exception. */
export function parsePalette(raw: string | null): string[] {
  if (!raw || raw.length > MAX_STORED_CHARS) return [];
  let data: unknown;
  try {
    data = JSON.parse(raw);
  } catch {
    return [];
  }
  if (!Array.isArray(data)) return [];
  const out: string[] = [];
  for (const item of data) {
    const hex = normalizeHex(item);
    if (hex && !isBuiltInColor(hex) && !out.includes(hex)) out.push(hex);
    if (out.length >= MAX_CUSTOM_COLORS) break;
  }
  return out;
}

/** Adds a colour at the front. Invalid, built-in, duplicate or over-limit additions leave the list unchanged. */
export function addColor(list: readonly string[], color: string): readonly string[] {
  const hex = normalizeHex(color);
  if (!hex || isBuiltInColor(hex) || list.includes(hex) || list.length >= MAX_CUSTOM_COLORS) return list;
  return [hex, ...list];
}

export function removeColor(list: readonly string[], color: string): readonly string[] {
  const hex = normalizeHex(color);
  return hex && list.includes(hex) ? list.filter((c) => c !== hex) : list;
}

// ------------------------------------------------------------------- store

const listeners = new Set<() => void>();
let cache: readonly string[] | null = null;

function load(): readonly string[] {
  if (cache) return cache;
  try {
    cache = parsePalette(localStorage.getItem(PALETTE_STORAGE_KEY));
  } catch {
    cache = [];
  }
  return cache;
}

function store(next: readonly string[]): void {
  cache = next;
  try {
    localStorage.setItem(PALETTE_STORAGE_KEY, JSON.stringify(next));
  } catch {
    // Storage may be unavailable or full; the palette still works for this page view.
  }
  for (const l of listeners) l();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  const onStorage = (e: StorageEvent): void => {
    if (e.key !== PALETTE_STORAGE_KEY && e.key !== null) return;
    cache = null;
    listener();
  };
  window.addEventListener('storage', onStorage);
  return () => {
    listeners.delete(listener);
    window.removeEventListener('storage', onStorage);
  };
}

export interface CustomPalette {
  colors: readonly string[];
  /** False when the list is full or the colour is built in / already saved. */
  canAdd: (color: string) => boolean;
  add: (color: string) => void;
  remove: (color: string) => void;
}

const EMPTY: readonly string[] = [];

const api: Omit<CustomPalette, 'colors'> = {
  canAdd: (color) => addColor(load(), color) !== load(),
  add: (color) => {
    const next = addColor(load(), color);
    if (next !== load()) store(next);
  },
  remove: (color) => {
    const next = removeColor(load(), color);
    if (next !== load()) store(next);
  },
};

export function useCustomPalette(): CustomPalette {
  const colors = useSyncExternalStore(subscribe, load, () => EMPTY);
  return { colors, ...api };
}
