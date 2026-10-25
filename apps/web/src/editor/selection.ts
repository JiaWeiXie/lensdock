/**
 * Multi-object selection geometry. Pure functions over the annotation list
 * (no DOM, no React) so group move/resize/duplicate/align/reorder stay
 * testable and every result is normalized (0..1, 1e-6 grid).
 *
 * Every function returns a fresh array and leaves annotations that are not
 * part of the operation as the identical object, so callers can diff by
 * reference.
 */
import type { Annotation, NormRect, Point } from '@/api/types';
import {
  MAX_ANNOTATIONS,
  boundsOf,
  clamp01,
  clampTranslation,
  definingPoints,
  fontSizeOf,
  moveBy,
  newAnnotationId,
  roundCoord,
  snapPoint,
  snapRect,
  translateAnnotation,
  type ImageSize,
  type TextMeasure,
} from './model';

export type Alignment = 'left' | 'center' | 'right' | 'top' | 'middle' | 'bottom';
export type ReorderDirection = 'front' | 'back' | 'forward' | 'backward';

export const MIN_FONT_SIZE_PX = 8;
export const MAX_FONT_SIZE_PX = 128;
/** Offset of a duplicate from its original, in source pixels (applied to the whole group). */
export const DUPLICATE_OFFSET_PX = 16;

const MAX_PIN_NUMBER = 100;
/** Extents below this are treated as degenerate (a vertical or horizontal line has zero width or height). */
const EXTENT_EPS = 1e-9;

function pick(list: readonly Annotation[], ids: ReadonlySet<string>): Annotation[] {
  return list.filter((a) => ids.has(a.id));
}

function isUsable(r: NormRect): boolean {
  return Number.isFinite(r.x) && Number.isFinite(r.y) && Number.isFinite(r.width) && Number.isFinite(r.height) && r.width >= 0 && r.height >= 0;
}

/** Union of the visual bounds, not clamped to the image (pins and text may extend past the edge). */
function unionBounds(items: readonly Annotation[], size: ImageSize, measure?: TextMeasure): NormRect | null {
  let x0 = Infinity;
  let y0 = Infinity;
  let x1 = -Infinity;
  let y1 = -Infinity;
  for (const a of items) {
    const b = boundsOf(a, size, measure);
    if (!isUsable(b)) continue;
    x0 = Math.min(x0, b.x);
    y0 = Math.min(y0, b.y);
    x1 = Math.max(x1, b.x + b.width);
    y1 = Math.max(y1, b.y + b.height);
  }
  if (!Number.isFinite(x0) || !Number.isFinite(y0)) return null;
  return { x: x0, y: y0, width: x1 - x0, height: y1 - y0 };
}

/**
 * Bounding box of the given annotations (pass the selected ones), clamped to
 * the image so it is always a valid NormRect. Null for an empty list. A lone
 * vertical or horizontal line yields a zero-width or zero-height box.
 */
export function selectionBounds(annotations: readonly Annotation[], size: ImageSize, measure?: TextMeasure): NormRect | null {
  const u = unionBounds(annotations, size, measure);
  if (!u) return null;
  const x0 = clamp01(u.x);
  const y0 = clamp01(u.y);
  const x1 = clamp01(u.x + u.width);
  const y1 = clamp01(u.y + u.height);
  return { x: x0, y: y0, width: Math.max(0, x1 - x0), height: Math.max(0, y1 - y0) };
}

/**
 * Moves the selected annotations by one common delta. The delta is clamped
 * against all of their defining points together, so hitting the image edge
 * stops the whole group instead of squashing the shapes that arrive first.
 */
export function moveSelection(list: readonly Annotation[], ids: ReadonlySet<string>, dx: number, dy: number): Annotation[] {
  const sel = pick(list, ids);
  if (sel.length === 0) return list.slice();
  const c = clampTranslation(
    sel.flatMap((a) => definingPoints(a)),
    roundCoord(dx),
    roundCoord(dy),
  );
  if (c.dx === 0 && c.dy === 0) return list.slice();
  return list.map((a) => (ids.has(a.id) ? translateAnnotation(a, c.dx, c.dy) : a));
}

interface AxisMap {
  /** Maps a coordinate of the old box into the new box. */
  apply: (v: number) => number;
  /** New length over old length; null when the old box has no extent on this axis. */
  scale: number | null;
}

/** Affine map of one axis. A zero-extent source axis collapses onto the target's near edge (the line stays a line). */
function axisMap(b0: number, bLen: number, t0: number, tLen: number): AxisMap {
  if (bLen <= EXTENT_EPS) return { apply: () => t0, scale: null };
  const scale = tLen / bLen;
  return { apply: (v) => t0 + (v - b0) * scale, scale };
}

/** Clamps a target box to the image and guarantees at least one source pixel on each axis. */
function normalizeTarget(r: NormRect, size: ImageSize): NormRect {
  const fit = (lo: number, hi: number, minLen: number): [number, number] => {
    let a = clamp01(Math.min(lo, hi));
    let b = clamp01(Math.max(lo, hi));
    if (b - a < minLen) {
      if (a + minLen <= 1) b = a + minLen;
      else {
        b = 1;
        a = Math.max(0, 1 - minLen);
      }
    }
    return [a, b];
  };
  const [x0, x1] = fit(r.x, r.x + r.width, 1 / Math.max(1, size.width));
  const [y0, y1] = fit(r.y, r.y + r.height, 1 / Math.max(1, size.height));
  return { x: x0, y: y0, width: x1 - x0, height: y1 - y0 };
}

function scaledFont(a: Annotation, factor: number): number {
  return Math.min(MAX_FONT_SIZE_PX, Math.max(MIN_FONT_SIZE_PX, Math.round(fontSizeOf(a) * factor * 100) / 100));
}

/**
 * Resizes the selected annotations as one group: every defining point goes
 * through the affine map that takes `before` onto `after`, so relative
 * positions are preserved. Text and pin font sizes follow the geometric mean
 * of the two axis scales, bounded to 8..128. The target is clamped to the
 * image and at least one pixel in each direction, so no shape degenerates and
 * every coordinate stays normalized.
 */
export function resizeSelection(
  list: readonly Annotation[],
  ids: ReadonlySet<string>,
  before: NormRect,
  after: NormRect,
  size: ImageSize,
): Annotation[] {
  if (!pick(list, ids).length || !isUsable(before) || !Number.isFinite(after.x + after.y + after.width + after.height)) return list.slice();
  const t = normalizeTarget(after, size);
  const mx = axisMap(before.x, before.width, t.x, t.width);
  const my = axisMap(before.y, before.height, t.y, t.height);
  const minW = 1 / Math.max(1, size.width);
  const minH = 1 / Math.max(1, size.height);
  const factor = mx.scale !== null && my.scale !== null ? Math.sqrt(mx.scale * my.scale) : (mx.scale ?? my.scale ?? 1);
  const fontFactor = Number.isFinite(factor) && factor > 0 ? factor : 1;
  const mp = (p: Point): Point => snapPoint({ x: mx.apply(p.x), y: my.apply(p.y) });
  // A box pushed against the far edge slides back instead of losing its one-pixel minimum.
  const mapSpan = (m: AxisMap, lo: number, len: number, minLen: number): [number, number] => {
    const start = Math.min(clamp01(m.apply(lo)), 1 - minLen);
    const end = Math.max(start + minLen, clamp01(m.apply(lo + len)));
    return [start, end - start];
  };
  const mapRect = (g: NormRect): NormRect => {
    const [x, width] = mapSpan(mx, g.x, g.width, minW);
    const [y, height] = mapSpan(my, g.y, g.height, minH);
    return snapRect({ x, y, width, height });
  };
  return list.map((a): Annotation => {
    if (!ids.has(a.id)) return a;
    switch (a.kind) {
      case 'rectangle':
      case 'ellipse':
      case 'diamond':
      case 'redact':
      case 'highlight':
      case 'spotlight':
      case 'blur':
      case 'pixelate':
        return { ...a, geometry: mapRect(a.geometry) };
      case 'arrow': {
        const { control } = a.geometry;
        return { ...a, geometry: { start: mp(a.geometry.start), end: mp(a.geometry.end), ...(control ? { control: mp(control) } : {}) } };
      }
      case 'line':
        return { ...a, geometry: { start: mp(a.geometry.start), end: mp(a.geometry.end) } };
      case 'freehand':
        return { ...a, geometry: { points: a.geometry.points.map(mp) } };
      case 'text': {
        const moved = { ...a, geometry: { ...a.geometry, position: mp(a.geometry.position) } };
        const fontSizePx = scaledFont(a, fontFactor);
        return fontSizePx === fontSizeOf(a) ? moved : { ...moved, style: { ...a.style, fontSizePx } };
      }
      case 'pin': {
        const moved = { ...a, geometry: { ...a.geometry, position: mp(a.geometry.position) } };
        const fontSizePx = scaledFont(a, fontFactor);
        return fontSizePx === fontSizeOf(a) ? moved : { ...moved, style: { ...a.style, fontSizePx } };
      }
    }
  });
}

/** Chooses a per-axis offset of at most `d`: forward if there is room, else backward, else whatever room remains. */
function offsetAxis(lo: number, hi: number, d: number): number {
  const forward = roundCoord(1 - hi);
  const backward = roundCoord(lo);
  if (forward >= d) return d;
  if (backward >= d) return -d;
  return forward >= backward ? Math.max(0, forward) : -Math.max(0, backward);
}

function freePinNumber(used: ReadonlySet<number>): number | null {
  for (let n = 1; n <= MAX_PIN_NUMBER; n += 1) if (!used.has(n)) return n;
  return null;
}

/**
 * Appends copies of the selected annotations (in their current stacking
 * order) shifted by one common offset of at most 16 source pixels, and
 * returns the copies' ids as the new selection. Copies get fresh ids and the
 * lowest unused pin numbers. If the copies would exceed the 100-annotation
 * limit (or no pin number is free) nothing is duplicated and the original
 * selection is returned, so a group is never half-copied.
 */
export function duplicateSelection(
  list: readonly Annotation[],
  ids: ReadonlySet<string>,
  size: ImageSize,
): { annotations: Annotation[]; selectedIds: string[] } {
  const sel = pick(list, ids);
  const unchanged = { annotations: list.slice(), selectedIds: sel.map((a) => a.id) };
  if (sel.length === 0 || list.length + sel.length > MAX_ANNOTATIONS) return unchanged;

  const points = sel.flatMap((a) => definingPoints(a));
  let minX = 1;
  let minY = 1;
  let maxX = 0;
  let maxY = 0;
  for (const p of points) {
    minX = Math.min(minX, p.x);
    minY = Math.min(minY, p.y);
    maxX = Math.max(maxX, p.x);
    maxY = Math.max(maxY, p.y);
  }
  const dx = offsetAxis(minX, maxX, roundCoord(DUPLICATE_OFFSET_PX / Math.max(1, size.width)));
  const dy = offsetAxis(minY, maxY, roundCoord(DUPLICATE_OFFSET_PX / Math.max(1, size.height)));

  const usedIds = new Set(list.map((a) => a.id));
  const usedPins = new Set<number>();
  for (const a of list) if (a.kind === 'pin') usedPins.add(a.geometry.number);

  const copies: Annotation[] = [];
  for (const a of sel) {
    let id = newAnnotationId();
    while (usedIds.has(id)) id = newAnnotationId();
    usedIds.add(id);
    let copy: Annotation = { ...translateAnnotation(a, dx, dy), id };
    if (copy.kind === 'pin') {
      const n = freePinNumber(usedPins);
      if (n === null) return unchanged;
      usedPins.add(n);
      copy = { ...copy, geometry: { ...copy.geometry, number: n } };
    }
    if (copy.style) copy = { ...copy, style: { ...copy.style } };
    copies.push(copy);
  }
  return { annotations: [...list, ...copies], selectedIds: copies.map((c) => c.id) };
}

/**
 * Aligns each selected annotation's visual bounds to the edge or center line
 * of the selection's overall bounds. Each shape moves independently (clamped
 * to the image); unselected shapes are untouched. A single selection is
 * already aligned with itself and is returned unchanged.
 */
export function alignSelection(
  list: readonly Annotation[],
  ids: ReadonlySet<string>,
  alignment: Alignment,
  size: ImageSize,
  measure?: TextMeasure,
): Annotation[] {
  const sel = pick(list, ids);
  const box = unionBounds(sel, size, measure);
  if (!box || sel.length < 2) return list.slice();
  return list.map((a) => {
    if (!ids.has(a.id)) return a;
    const b = boundsOf(a, size, measure);
    if (!isUsable(b)) return a;
    let dx = 0;
    let dy = 0;
    switch (alignment) {
      case 'left':
        dx = box.x - b.x;
        break;
      case 'center':
        dx = box.x + box.width / 2 - (b.x + b.width / 2);
        break;
      case 'right':
        dx = box.x + box.width - (b.x + b.width);
        break;
      case 'top':
        dy = box.y - b.y;
        break;
      case 'middle':
        dy = box.y + box.height / 2 - (b.y + b.height / 2);
        break;
      case 'bottom':
        dy = box.y + box.height - (b.y + b.height);
        break;
    }
    if (Math.abs(dx) < EXTENT_EPS && Math.abs(dy) < EXTENT_EPS) return a;
    return moveBy(a, dx, dy);
  });
}

/**
 * Changes the stacking order (list order is paint order, last is on top)
 * of the selected annotations. Relative order inside the selection and inside
 * the rest is preserved; `forward`/`backward` move the selection one step past
 * the nearest unselected neighbour, and a group already at the limit stays put.
 */
export function reorderSelection(list: readonly Annotation[], ids: ReadonlySet<string>, direction: ReorderDirection): Annotation[] {
  const out = list.slice();
  switch (direction) {
    case 'front':
      return [...out.filter((a) => !ids.has(a.id)), ...out.filter((a) => ids.has(a.id))];
    case 'back':
      return [...out.filter((a) => ids.has(a.id)), ...out.filter((a) => !ids.has(a.id))];
    case 'forward':
      for (let i = out.length - 2; i >= 0; i -= 1) {
        const cur = out[i];
        const next = out[i + 1];
        if (cur && next && ids.has(cur.id) && !ids.has(next.id)) {
          out[i] = next;
          out[i + 1] = cur;
        }
      }
      return out;
    case 'backward':
      for (let i = 1; i < out.length; i += 1) {
        const cur = out[i];
        const prev = out[i - 1];
        if (cur && prev && ids.has(cur.id) && !ids.has(prev.id)) {
          out[i] = prev;
          out[i - 1] = cur;
        }
      }
      return out;
  }
}

/**
 * Ids (in list order) of the annotations whose bounds touch the marquee.
 * The marquee may be dragged in any direction; a zero-area marquee (a plain
 * click) selects nothing.
 */
export function marqueeSelection(list: readonly Annotation[], rect: NormRect, size: ImageSize, measure?: TextMeasure): string[] {
  if (!Number.isFinite(rect.x + rect.y + rect.width + rect.height)) return [];
  const x0 = Math.min(rect.x, rect.x + rect.width);
  const x1 = Math.max(rect.x, rect.x + rect.width);
  const y0 = Math.min(rect.y, rect.y + rect.height);
  const y1 = Math.max(rect.y, rect.y + rect.height);
  if (x1 - x0 <= 0 || y1 - y0 <= 0) return [];
  const ids: string[] = [];
  for (const a of list) {
    const b = boundsOf(a, size, measure);
    if (!isUsable(b)) continue;
    if (b.x <= x1 && b.x + b.width >= x0 && b.y <= y1 && b.y + b.height >= y0) ids.push(a.id);
  }
  return ids;
}
