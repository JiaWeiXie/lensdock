/**
 * Annotation model and geometry. Pure functions only (no DOM) so the
 * coordinate contract can be unit-tested.
 *
 * Coordinate contract (SPEC 11.3): every geometry is normalized 0..1 with the
 * origin at the top-left of the *actual image content*; pixel positions come
 * from `ImageSize` (the capture's real width/height). Rectangles satisfy
 * x+width <= 1 and y+height <= 1 exactly, after rounding to COORD_DECIMALS.
 */
import type { Annotation, AnnotationKind, AnnotationStyle, ArrowAnnotation, ArrowStyle, NormRect, Point, TextStyle } from '@/api/types';
import { codePointLength, truncateToCodePoints } from '@/util/text';
import { flattenPath, flattenQuad, smoothPath, type PxPoint } from './paths';

export interface ImageSize {
  width: number;
  height: number;
}

export const MAX_ANNOTATIONS = 100;
export const MAX_TEXT_CP = 2000;
export const MAX_COMMENT_CP = 4000;
export const MAX_FREEHAND_POINTS = 10000;
export const COORD_DECIMALS = 6;
export const LINE_HEIGHT = 1.25;
/** First baseline offset from the text box top, in font-size units. */
export const BASELINE = 0.92;
export const REDACT_FILL = '#000000';

export const PALETTE: readonly string[] = ['#e5362b', '#f2a900', '#0b8a84', '#2f6fed', '#0b2540', '#ffffff'];

export const EFFECT_SIZE_MIN = 2;
export const EFFECT_SIZE_MAX = 64;
export const EFFECT_SIZE_DEFAULT_BLUR = 12;
export const EFFECT_SIZE_DEFAULT_PIXELATE = 16;
export const HIGHLIGHT_OPACITY_MIN = 0.1;
export const HIGHLIGHT_OPACITY_MAX = 0.8;
export const HIGHLIGHT_OPACITY_DEFAULT = 0.35;
export const HIGHLIGHT_COLOR = '#f2a900';
/** Fixed dim of the area outside all spotlights (there is no per-spotlight opacity). */
export const SPOTLIGHT_DIM = 0.6;
/** Quadratic-curve offset of the default arrow control, as a fraction of the chord (the apex sits half of it off the chord). */
const CURVE_OFFSET = 0.3;

export const ARROW_STYLES: readonly ArrowStyle[] = ['filled', 'open', 'double', 'curved'];
export const TEXT_STYLES: readonly TextStyle[] = ['plain', 'outline', 'label', 'shadow', 'mono', 'bold', 'underline'];

export type EffectKind = 'blur' | 'pixelate';

export function isEffectKind(kind: AnnotationKind): kind is EffectKind {
  return kind === 'blur' || kind === 'pixelate';
}

/** Kinds that need schema 0.3 whatever their styling. */
export const ADVANCED_KINDS: readonly AnnotationKind[] = ['highlight', 'spotlight', 'blur', 'pixelate'];

/** Paint tier: base effects first, then the spotlight dim, then every other annotation, and solid redactions last. */
function paintTier(a: Annotation): number {
  switch (a.kind) {
    case 'blur':
    case 'pixelate':
      return 0;
    case 'spotlight':
      return 1;
    case 'redact':
      return 3;
    default:
      return 2;
  }
}

/**
 * Paint order shared by hit testing, the editing view and every flattened export. Effects (blur,
 * pixelate) sample the base so they come first, the spotlight dim follows, other annotations keep
 * list order, and solid redactions are always last so nothing layered above them can cover one.
 * The relative order inside each tier is the list order (it decides which effect sees which).
 */
export function redactionsLast<T extends Annotation>(list: readonly T[]): T[] {
  const tiers: T[][] = [[], [], [], []];
  for (const a of list) tiers[paintTier(a)]?.push(a);
  return tiers.flat();
}

const SCALE = 10 ** COORD_DECIMALS;

export function clamp01(v: number): number {
  return Math.min(1, Math.max(0, v));
}

export function roundCoord(v: number): number {
  return Math.round(v * SCALE) / SCALE;
}

function floorCoord(v: number): number {
  return Math.floor(v * SCALE + 1e-9) / SCALE;
}

export function snapPoint(p: Point): Point {
  return { x: clamp01(roundCoord(p.x)), y: clamp01(roundCoord(p.y)) };
}

/** Snaps to the coordinate grid while guaranteeing x+width <= 1 and y+height <= 1. */
export function snapRect(r: NormRect): NormRect {
  const x = clamp01(roundCoord(r.x));
  const y = clamp01(roundCoord(r.y));
  let width = roundCoord(Math.max(0, r.width));
  let height = roundCoord(Math.max(0, r.height));
  while (x + width > 1 && width > 0) width = floorCoord(width - 1 / SCALE);
  while (y + height > 1 && height > 0) height = floorCoord(height - 1 / SCALE);
  return { x, y, width, height };
}

export function rectFromPoints(a: Point, b: Point): NormRect {
  const x = Math.min(a.x, b.x);
  const y = Math.min(a.y, b.y);
  return { x, y, width: Math.abs(a.x - b.x), height: Math.abs(a.y - b.y) };
}

/** Pointer position in the image content rectangle (screen rect of the stage) as normalized coordinates. */
export function pointFromClient(
  clientX: number,
  clientY: number,
  rect: { left: number; top: number; width: number; height: number },
): Point {
  if (rect.width <= 0 || rect.height <= 0) return { x: 0, y: 0 };
  return { x: clamp01((clientX - rect.left) / rect.width), y: clamp01((clientY - rect.top) / rect.height) };
}

export function toPx(p: Point, size: ImageSize): { x: number; y: number } {
  return { x: p.x * size.width, y: p.y * size.height };
}

/** Source pixel of a normalized point (what an Agent would be told about). */
export function toSourcePixel(p: Point, size: ImageSize): { x: number; y: number } {
  return { x: Math.min(size.width - 1, Math.floor(p.x * size.width)), y: Math.min(size.height - 1, Math.floor(p.y * size.height)) };
}

// ------------------------------------------------------------------ styles

export function defaultStyle(kind: Exclude<AnnotationKind, 'redact'>, size: ImageSize): AnnotationStyle {
  const base = Math.min(size.width, size.height);
  const stroke = PALETTE[0] ?? '#e5362b';
  const strokeWidthPx = Math.min(12, Math.max(2, Math.round(size.width / 400)));
  const fontSizePx = Math.min(72, Math.max(14, Math.round(base / 40)));
  switch (kind) {
    case 'text':
      return { stroke, fontSizePx };
    case 'pin':
      return { stroke, fontSizePx: Math.round(fontSizePx * 1.1) };
    case 'freehand':
      return { stroke, strokeWidthPx, smoothing: true };
    case 'highlight':
      return { stroke: HIGHLIGHT_COLOR, opacity: HIGHLIGHT_OPACITY_DEFAULT };
    case 'blur':
      return { effectSizePx: EFFECT_SIZE_DEFAULT_BLUR };
    case 'pixelate':
      return { effectSizePx: EFFECT_SIZE_DEFAULT_PIXELATE };
    case 'spotlight':
      return {};
    default:
      return { stroke, strokeWidthPx };
  }
}

export function pinRadiusPx(style: AnnotationStyle | undefined): number {
  return Math.max(10, (style?.fontSizePx ?? 20) * 0.8);
}

export function strokeOf(a: Annotation): string {
  if (a.kind === 'redact') return REDACT_FILL;
  return a.style?.stroke ?? (a.kind === 'highlight' ? HIGHLIGHT_COLOR : (PALETTE[0] ?? '#e5362b'));
}

const HEX_COLOR = /^#[0-9a-fA-F]{6}$/;

/** Shape fill for the closed shapes (rectangle, ellipse, diamond); `null` means transparent. Malformed values never paint. */
export function fillOf(a: Annotation): string | null {
  if (a.kind !== 'rectangle' && a.kind !== 'ellipse' && a.kind !== 'diamond') return null;
  const fill = a.style?.fill;
  return fill !== undefined && HEX_COLOR.test(fill) ? fill : null;
}

export function strokeWidthOf(a: Annotation): number {
  return a.style?.strokeWidthPx ?? 3;
}

export function fontSizeOf(a: Annotation): number {
  return a.style?.fontSizePx ?? 20;
}

/** Highlight fill alpha, clamped to 0.1..0.8 (default 0.35). */
export function highlightOpacityOf(a: Annotation): number {
  const v = a.style?.opacity;
  return typeof v === 'number' && Number.isFinite(v) ? Math.min(HIGHLIGHT_OPACITY_MAX, Math.max(HIGHLIGHT_OPACITY_MIN, v)) : HIGHLIGHT_OPACITY_DEFAULT;
}

/** Blur radius / pixelate block edge in source pixels, an integer in 2..64 (default 12 blur, 16 pixelate). */
export function effectSizeOf(a: Annotation): number {
  const v = a.style?.effectSizePx;
  if (typeof v === 'number' && Number.isFinite(v)) return Math.min(EFFECT_SIZE_MAX, Math.max(EFFECT_SIZE_MIN, Math.round(v)));
  return a.kind === 'pixelate' ? EFFECT_SIZE_DEFAULT_PIXELATE : EFFECT_SIZE_DEFAULT_BLUR;
}

export function arrowStyleOf(a: Annotation): ArrowStyle {
  const v = a.style?.arrowStyle;
  return v !== undefined && ARROW_STYLES.includes(v) ? v : 'filled';
}

export function textStyleOf(a: Annotation): TextStyle {
  const v = a.style?.textStyle;
  return v !== undefined && TEXT_STYLES.includes(v) ? v : 'outline';
}

/** Freehand smoothing is opt-in: legacy strokes (no flag) stay plain polylines. */
export function isSmoothed(a: Annotation): boolean {
  return a.kind === 'freehand' && a.style?.smoothing === true;
}

/** Padding of the colored backdrop of the `label` text style, in image pixels. */
export function labelPadding(fontSizePx: number): { x: number; y: number } {
  return { x: fontSizePx * 0.45, y: fontSizePx * 0.22 };
}

export function nextPinNumber(list: readonly Annotation[]): number | null {
  const used = new Set<number>();
  for (const a of list) if (a.kind === 'pin') used.add(a.geometry.number);
  for (let n = 1; n <= 100; n += 1) if (!used.has(n)) return n;
  return null;
}

export function newAnnotationId(): string {
  return crypto.randomUUID();
}

export function textLines(text: string): string[] {
  return text.split('\n');
}

export function textHeightPx(a: { geometry: { text: string } }, fontSizePx: number): number {
  return textLines(a.geometry.text).length * fontSizePx * LINE_HEIGHT;
}

// ---------------------------------------------------------------- geometry

/** Measures the widest line of a text annotation in image pixels. Injected so tests need no canvas. */
export type TextMeasure = (text: string, fontSizePx: number, textStyle?: TextStyle) => number;

const approxMeasure: TextMeasure = (text, fontSizePx, textStyle) =>
  Math.max(...textLines(text).map((l) => Array.from(l).length)) * fontSizePx * (textStyle === 'mono' ? 0.6 : textStyle === 'bold' ? 0.66 : 0.62);

/** Axis-aligned bounds in normalized coordinates (may extend past the image for pins near an edge). */
export function boundsOf(a: Annotation, size: ImageSize, measure: TextMeasure = approxMeasure): NormRect {
  // A zero-sized image must not turn text or pin extents into NaN/Infinity.
  const w = Math.max(1, size.width);
  const h = Math.max(1, size.height);
  switch (a.kind) {
    case 'rectangle':
    case 'ellipse':
    case 'diamond':
    case 'redact':
    case 'highlight':
    case 'spotlight':
    case 'blur':
    case 'pixelate':
      return a.geometry;
    case 'arrow': {
      // A curved arrow's box includes its control point so the control handle is inside the selection.
      const control = arrowControlPoint(a);
      return control ? pointsBounds([a.geometry.start, a.geometry.end, control]) : rectFromPoints(a.geometry.start, a.geometry.end);
    }
    case 'line':
      return rectFromPoints(a.geometry.start, a.geometry.end);
    case 'freehand':
      // A smoothed curve stays inside the hull of its raw points, so the raw box is exact for both modes.
      return a.geometry.points.length > 0 ? pointsBounds(a.geometry.points) : { x: 1, y: 1, width: -1, height: -1 };
    case 'text': {
      const fs = fontSizeOf(a);
      const measured = measure(a.geometry.text, fs, a.style?.textStyle);
      const pad = textStyleOf(a) === 'label' ? labelPadding(fs) : { x: 0, y: 0 };
      return {
        x: a.geometry.position.x,
        y: a.geometry.position.y,
        width: ((Number.isFinite(measured) ? Math.max(0, measured) : 0) + 2 * pad.x) / w,
        height: (textHeightPx(a, fs) + 2 * pad.y) / h,
      };
    }
    case 'pin': {
      const r = pinRadiusPx(a.style);
      return {
        x: a.geometry.position.x - r / w,
        y: a.geometry.position.y - r / h,
        width: (2 * r) / w,
        height: (2 * r) / h,
      };
    }
  }
}

/** Smallest rectangle containing every point (callers pass at least one). */
function pointsBounds(points: readonly Point[]): NormRect {
  let minX = Infinity;
  let minY = Infinity;
  let maxX = -Infinity;
  let maxY = -Infinity;
  for (const p of points) {
    minX = Math.min(minX, p.x);
    minY = Math.min(minY, p.y);
    maxX = Math.max(maxX, p.x);
    maxY = Math.max(maxY, p.y);
  }
  return { x: minX, y: minY, width: maxX - minX, height: maxY - minY };
}

function distToSegment(p: { x: number; y: number }, a: { x: number; y: number }, b: { x: number; y: number }): number {
  const dx = b.x - a.x;
  const dy = b.y - a.y;
  const len2 = dx * dx + dy * dy;
  const t = len2 === 0 ? 0 : Math.max(0, Math.min(1, ((p.x - a.x) * dx + (p.y - a.y) * dy) / len2));
  return Math.hypot(p.x - (a.x + t * dx), p.y - (a.y + t * dy));
}

function insideRect(p: Point, r: NormRect): boolean {
  return p.x >= r.x && p.x <= r.x + r.width && p.y >= r.y && p.y <= r.y + r.height;
}

/** Four diamond vertices (top, right, bottom, left) in image pixels, inscribed in the rectangle. */
export function diamondVertices(r: NormRect, size: ImageSize): { x: number; y: number }[] {
  const left = r.x * size.width;
  const top = r.y * size.height;
  const right = (r.x + r.width) * size.width;
  const bottom = (r.y + r.height) * size.height;
  const cx = (left + right) / 2;
  const cy = (top + bottom) / 2;
  return [
    { x: cx, y: top },
    { x: right, y: cy },
    { x: cx, y: bottom },
    { x: left, y: cy },
  ];
}

/** First-order distance from `p` to an ellipse outline (exact on the axes, within a few percent elsewhere); degenerate radii fall back to the segment. */
function distToEllipse(p: { x: number; y: number }, c: { x: number; y: number }, rx: number, ry: number): number {
  if (rx <= 0 || ry <= 0) return distToSegment(p, { x: c.x - rx, y: c.y - ry }, { x: c.x + rx, y: c.y + ry });
  const dx = p.x - c.x;
  const dy = p.y - c.y;
  const rho = Math.hypot(dx / rx, dy / ry);
  if (rho < 1e-9) return Math.min(rx, ry);
  const grad = Math.hypot(dx / (rx * rx), dy / (ry * ry)) / rho;
  return Math.abs(rho - 1) / grad;
}

function hitClosedShape(a: Annotation & { kind: 'rectangle' | 'ellipse' | 'diamond' }, p: Point, pp: { x: number; y: number }, size: ImageSize, tolPx: number): boolean {
  const g = a.geometry;
  const filled = fillOf(a) !== null;
  switch (a.kind) {
    case 'rectangle': {
      const tx = tolPx / size.width;
      const ty = tolPx / size.height;
      const outer = insideRect(p, { x: g.x - tx, y: g.y - ty, width: g.width + 2 * tx, height: g.height + 2 * ty });
      const inner = insideRect(p, { x: g.x + tx, y: g.y + ty, width: g.width - 2 * tx, height: g.height - 2 * ty });
      return outer && (filled || !inner);
    }
    case 'ellipse': {
      const rx = (g.width * size.width) / 2;
      const ry = (g.height * size.height) / 2;
      const c = { x: g.x * size.width + rx, y: g.y * size.height + ry };
      if (filled && rx > 0 && ry > 0 && ((pp.x - c.x) / rx) ** 2 + ((pp.y - c.y) / ry) ** 2 <= 1) return true;
      return distToEllipse(pp, c, rx, ry) <= tolPx;
    }
    case 'diamond': {
      const v = diamondVertices(g, size);
      if (filled) {
        const rx = (g.width * size.width) / 2;
        const ry = (g.height * size.height) / 2;
        const cx = g.x * size.width + rx;
        const cy = g.y * size.height + ry;
        if (rx > 0 && ry > 0 && Math.abs(pp.x - cx) / rx + Math.abs(pp.y - cy) / ry <= 1) return true;
      }
      for (let k = 0; k < v.length; k += 1) {
        const s = v[k];
        const e = v[(k + 1) % v.length];
        if (s && e && distToSegment(pp, s, e) <= tolPx) return true;
      }
      return false;
    }
  }
}

/** Topmost annotation under `p`, or null. `tolPx` is the pick tolerance in image pixels. */
export function hitTest(list: readonly Annotation[], p: Point, size: ImageSize, tolPx: number, measure?: TextMeasure): string | null {
  const pp = toPx(p, size);
  for (let i = list.length - 1; i >= 0; i -= 1) {
    const a = list[i];
    if (!a) continue;
    switch (a.kind) {
      case 'redact':
      case 'highlight':
      case 'spotlight':
      case 'blur':
      case 'pixelate':
        if (insideRect(p, a.geometry)) return a.id;
        break;
      case 'rectangle':
      case 'ellipse':
      case 'diamond':
        if (hitClosedShape(a, p, pp, size, tolPx)) return a.id;
        break;
      case 'arrow':
      case 'line': {
        if (polylineHit(pp, linePointsPx(a, size), tolPx + strokeWidthOf(a) / 2)) return a.id;
        break;
      }
      case 'freehand': {
        const raw = a.geometry.points.map((q) => toPx(q, size));
        if (polylineHit(pp, isSmoothed(a) ? flattenPath(smoothPath(raw)) : raw, tolPx + strokeWidthOf(a) / 2)) return a.id;
        break;
      }
      case 'text':
      case 'pin':
        if (insideRect(p, boundsOf(a, size, measure))) return a.id;
        break;
    }
  }
  return null;
}

/** The points that define an annotation's position; moves are clamped so all of them stay inside the image. */
export function definingPoints(a: Annotation): Point[] {
  switch (a.kind) {
    case 'rectangle':
    case 'ellipse':
    case 'diamond':
    case 'redact':
    case 'highlight':
    case 'spotlight':
    case 'blur':
    case 'pixelate':
      return [
        { x: a.geometry.x, y: a.geometry.y },
        { x: a.geometry.x + a.geometry.width, y: a.geometry.y + a.geometry.height },
      ];
    case 'arrow': {
      // The control only constrains moves while it is visible (curved); a retained one is clamped on its own.
      const control = arrowControlPoint(a);
      return control ? [a.geometry.start, a.geometry.end, control] : [a.geometry.start, a.geometry.end];
    }
    case 'line':
      return [a.geometry.start, a.geometry.end];
    case 'freehand':
      return a.geometry.points;
    case 'text':
    case 'pin':
      return [a.geometry.position];
  }
}

/**
 * Largest translation not exceeding the requested delta that keeps every given
 * point inside the image. Non-finite deltas count as zero. Passing the points
 * of a whole selection yields one common delta, so a group never shears.
 */
export function clampTranslation(points: readonly Point[], dx: number, dy: number): { dx: number; dy: number } {
  if (points.length === 0) return { dx: 0, dy: 0 };
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
  const rx = Number.isFinite(dx) ? dx : 0;
  const ry = Number.isFinite(dy) ? dy : 0;
  return { dx: Math.min(1 - maxX, Math.max(-minX, rx)), dy: Math.min(1 - maxY, Math.max(-minY, ry)) };
}

/** Translates by exactly (cx, cy) without clamping (callers clamp), snapping the result to the coordinate grid. */
export function translateAnnotation(a: Annotation, cx: number, cy: number): Annotation {
  const mv = (p: Point): Point => snapPoint({ x: p.x + cx, y: p.y + cy });
  switch (a.kind) {
    case 'rectangle':
    case 'ellipse':
    case 'diamond':
    case 'redact':
    case 'highlight':
    case 'spotlight':
    case 'blur':
    case 'pixelate':
      return { ...a, geometry: snapRect({ ...a.geometry, x: a.geometry.x + cx, y: a.geometry.y + cy }) };
    case 'arrow': {
      const { control } = a.geometry;
      return { ...a, geometry: { start: mv(a.geometry.start), end: mv(a.geometry.end), ...(control ? { control: mv(control) } : {}) } };
    }
    case 'line':
      return { ...a, geometry: { start: mv(a.geometry.start), end: mv(a.geometry.end) } };
    case 'freehand':
      return { ...a, geometry: { points: a.geometry.points.map(mv) } };
    case 'text':
      return { ...a, geometry: { ...a.geometry, position: mv(a.geometry.position) } };
    case 'pin':
      return { ...a, geometry: { ...a.geometry, position: mv(a.geometry.position) } };
  }
}

/** Translates by a normalized delta, clamped so every defining point stays inside the image. */
export function moveBy(a: Annotation, dx: number, dy: number): Annotation {
  const c = clampTranslation(definingPoints(a), dx, dy);
  return translateAnnotation(a, c.dx, c.dy);
}

export type Handle = 'nw' | 'n' | 'ne' | 'e' | 'se' | 's' | 'sw' | 'w';

export const HANDLES: readonly Handle[] = ['nw', 'n', 'ne', 'e', 'se', 's', 'sw', 'w'];

export function handlePosition(r: NormRect, h: Handle): Point {
  const cx = r.x + r.width / 2;
  const cy = r.y + r.height / 2;
  const right = r.x + r.width;
  const bottom = r.y + r.height;
  switch (h) {
    case 'nw':
      return { x: r.x, y: r.y };
    case 'n':
      return { x: cx, y: r.y };
    case 'ne':
      return { x: right, y: r.y };
    case 'e':
      return { x: right, y: cy };
    case 'se':
      return { x: right, y: bottom };
    case 's':
      return { x: cx, y: bottom };
    case 'sw':
      return { x: r.x, y: bottom };
    case 'w':
      return { x: r.x, y: cy };
  }
}

/** Drags one handle of a rectangle to `p`, keeping a minimum size of `minPx` source pixels. */
export function resizeRect(r: NormRect, h: Handle, p: Point, size: ImageSize, minPx = 2): NormRect {
  let left = r.x;
  let top = r.y;
  let right = r.x + r.width;
  let bottom = r.y + r.height;
  const minW = minPx / size.width;
  const minH = minPx / size.height;
  if (h.includes('w')) left = Math.min(p.x, right - minW);
  if (h.includes('e')) right = Math.max(p.x, left + minW);
  if (h.includes('n')) top = Math.min(p.y, bottom - minH);
  if (h.includes('s')) bottom = Math.max(p.y, top + minH);
  return snapRect({ x: clamp01(left), y: clamp01(top), width: clamp01(right) - clamp01(left), height: clamp01(bottom) - clamp01(top) });
}

/** Ramer-Douglas-Peucker in image pixels; keeps the stroke shape while bounding the point count. */
export function simplifyPath(points: readonly Point[], size: ImageSize, tolerancePx: number): Point[] {
  if (points.length <= 2) return [...points];
  const px = points.map((p) => toPx(p, size));
  const keep = new Array<boolean>(points.length).fill(false);
  keep[0] = true;
  keep[points.length - 1] = true;
  const stack: [number, number][] = [[0, points.length - 1]];
  while (stack.length > 0) {
    const range = stack.pop();
    if (!range) break;
    const [s, e] = range;
    const a = px[s];
    const b = px[e];
    if (!a || !b) continue;
    let maxD = 0;
    let idx = -1;
    for (let i = s + 1; i < e; i += 1) {
      const q = px[i];
      if (!q) continue;
      const d = distToSegment(q, a, b);
      if (d > maxD) {
        maxD = d;
        idx = i;
      }
    }
    if (idx !== -1 && maxD > tolerancePx) {
      keep[idx] = true;
      stack.push([s, idx], [idx, e]);
    }
  }
  return points.filter((_, i) => keep[i]);
}

/** Arrow head triangle (tip, left, right) for a head whose tip travels in direction `angle` (radians), in image pixels. */
export function headTriangle(tip: PxPoint, angle: number, strokeWidthPx: number): [PxPoint, PxPoint, PxPoint] {
  const len = Math.max(12, strokeWidthPx * 4.5);
  const spread = Math.PI / 7;
  return [
    tip,
    { x: tip.x - len * Math.cos(angle - spread), y: tip.y - len * Math.sin(angle - spread) },
    { x: tip.x - len * Math.cos(angle + spread), y: tip.y - len * Math.sin(angle + spread) },
  ];
}

/**
 * Default control point of a curved arrow: the chord midpoint pushed sideways by
 * 30% of the chord (perpendicular in normalized coordinates). The side that
 * stays inside the image wins, and the result is clamped and snapped to the
 * coordinate grid, so it is always a valid wire Point.
 */
export function defaultArrowControl(start: Point, end: Point): Point {
  const mx = (start.x + end.x) / 2;
  const my = (start.y + end.y) / 2;
  const ox = -(end.y - start.y) * CURVE_OFFSET;
  const oy = (end.x - start.x) * CURVE_OFFSET;
  const inside = (p: Point): boolean => p.x >= 0 && p.x <= 1 && p.y >= 0 && p.y <= 1;
  const first = { x: mx + ox, y: my + oy };
  const second = { x: mx - ox, y: my - oy };
  return snapPoint(inside(first) || !inside(second) ? first : second);
}

/**
 * The quadratic control point of an arrow that is drawn curved (stored one, or
 * the default), else null. Handles, bounds and hit tests use this; a control
 * retained on a straight arrow is ignored.
 */
export function arrowControlPoint(a: ArrowAnnotation): Point | null {
  if (a.style?.arrowStyle !== 'curved') return null;
  return a.geometry.control ?? defaultArrowControl(a.geometry.start, a.geometry.end);
}

/** Visible centerline of a line or arrow in image pixels (curves are flattened). */
function linePointsPx(a: Annotation & { kind: 'arrow' | 'line' }, size: ImageSize): PxPoint[] {
  const s = toPx(a.geometry.start, size);
  const e = toPx(a.geometry.end, size);
  const control = a.kind === 'arrow' ? arrowControlPoint(a) : null;
  return control ? flattenQuad(s, toPx(control, size), e, 32) : [s, e];
}

function polylineHit(p: PxPoint, points: readonly PxPoint[], tol: number): boolean {
  for (let k = 1; k < points.length; k += 1) {
    const s = points[k - 1];
    const e = points[k];
    if (s && e && distToSegment(p, s, e) <= tol) return true;
  }
  return false;
}

// -------------------------------------------------------------- validation

export type AnnotationIssue =
  | { code: 'tooMany' }
  | { code: 'duplicateId'; id: string }
  | { code: 'pinNumber'; id: string }
  | { code: 'pinDuplicate'; id: string }
  | { code: 'bounds'; id: string }
  | { code: 'textLength'; id: string }
  | { code: 'commentLength'; id: string }
  | { code: 'freehandPoints'; id: string };

const EPS = 0;

/** Client-side mirror of the server's cross-field checks. The server remains authoritative. */
export function validateAnnotations(list: readonly Annotation[]): AnnotationIssue[] {
  const issues: AnnotationIssue[] = [];
  if (list.length > MAX_ANNOTATIONS) issues.push({ code: 'tooMany' });
  const ids = new Set<string>();
  const pins = new Set<number>();
  for (const a of list) {
    if (ids.has(a.id)) issues.push({ code: 'duplicateId', id: a.id });
    ids.add(a.id);
    if ((a.comment ?? '').length > 0 && codePointLength(a.comment ?? '') > MAX_COMMENT_CP) issues.push({ code: 'commentLength', id: a.id });
    switch (a.kind) {
      case 'rectangle':
      case 'ellipse':
      case 'diamond':
      case 'redact':
      case 'highlight':
      case 'spotlight':
      case 'blur':
      case 'pixelate': {
        const g = a.geometry;
        if (g.width <= 0 || g.height <= 0 || g.x + g.width > 1 + EPS || g.y + g.height > 1 + EPS || g.x < 0 || g.y < 0) issues.push({ code: 'bounds', id: a.id });
        break;
      }
      case 'pin': {
        const n = a.geometry.number;
        if (!Number.isInteger(n) || n < 1 || n > 100) issues.push({ code: 'pinNumber', id: a.id });
        if (pins.has(n)) issues.push({ code: 'pinDuplicate', id: a.id });
        pins.add(n);
        break;
      }
      case 'text': {
        const len = codePointLength(a.geometry.text);
        if (len < 1 || len > MAX_TEXT_CP) issues.push({ code: 'textLength', id: a.id });
        break;
      }
      case 'freehand':
        if (a.geometry.points.length < 2 || a.geometry.points.length > MAX_FREEHAND_POINTS) issues.push({ code: 'freehandPoints', id: a.id });
        break;
      case 'arrow': {
        const c = a.geometry.control;
        if (c && !(c.x >= 0 && c.x <= 1 && c.y >= 0 && c.y <= 1)) issues.push({ code: 'bounds', id: a.id });
        break;
      }
      case 'line':
        break;
    }
  }
  return issues;
}

/**
 * Wire form of the annotations. Redactions carry `mode: "solid"` and no style
 * key at all (the schema forbids one); comments are cut at the limit on a
 * grapheme boundary; coordinates are snapped to the grid.
 */
export function toWire(list: readonly Annotation[]): Annotation[] {
  return list.map((a): Annotation => {
    const comment = truncateToCodePoints(a.comment ?? '', MAX_COMMENT_CP);
    switch (a.kind) {
      case 'redact':
        return { id: a.id, kind: 'redact', mode: 'solid', geometry: snapRect(a.geometry), comment };
      case 'spotlight':
        return { id: a.id, kind: 'spotlight', geometry: snapRect(a.geometry), comment };
      case 'highlight':
        return {
          id: a.id,
          kind: 'highlight',
          geometry: snapRect(a.geometry),
          comment,
          style: { stroke: hexOr(a.style?.stroke, HIGHLIGHT_COLOR), opacity: Math.round(highlightOpacityOf(a) * 100) / 100 },
        };
      case 'blur':
      case 'pixelate':
        return { id: a.id, kind: a.kind, geometry: snapRect(a.geometry), comment, style: { effectSizePx: effectSizeOf(a) } };
      case 'rectangle':
      case 'ellipse':
      case 'diamond':
        return { id: a.id, kind: a.kind, geometry: snapRect(a.geometry), comment, style: cleanStyle(a.style) };
      case 'line':
        return {
          id: a.id,
          kind: 'line',
          geometry: { start: snapPoint(a.geometry.start), end: snapPoint(a.geometry.end) },
          comment,
          style: cleanStyle(a.style),
        };
      case 'arrow': {
        const style = cleanStyle(a.style);
        const arrowStyle = arrowStyleOf(a);
        // "filled" is the absent default, so it is not written (a document stays on its old schema version).
        if (arrowStyle !== 'filled') style.arrowStyle = arrowStyle;
        const control = arrowStyle === 'curved' ? (a.geometry.control ?? defaultArrowControl(a.geometry.start, a.geometry.end)) : a.geometry.control;
        return {
          id: a.id,
          kind: 'arrow',
          geometry: { start: snapPoint(a.geometry.start), end: snapPoint(a.geometry.end), ...(control ? { control: snapPoint(control) } : {}) },
          comment,
          style,
        };
      }
      case 'freehand': {
        const style = cleanStyle(a.style);
        if (isSmoothed(a)) style.smoothing = true;
        return { id: a.id, kind: 'freehand', geometry: { points: a.geometry.points.map(snapPoint) }, comment, style };
      }
      case 'text': {
        const style = cleanStyle(a.style);
        const textStyle = textStyleOf(a);
        if (textStyle !== 'outline') style.textStyle = textStyle;
        return {
          id: a.id,
          kind: 'text',
          geometry: { position: snapPoint(a.geometry.position), text: truncateToCodePoints(a.geometry.text, MAX_TEXT_CP) },
          comment,
          style,
        };
      }
      case 'pin':
        return {
          id: a.id,
          kind: 'pin',
          geometry: { position: snapPoint(a.geometry.position), number: a.geometry.number },
          comment,
          style: cleanStyle(a.style),
        };
    }
  });
}

function hexOr(value: string | undefined, fallback: string): string {
  return value !== undefined && HEX_COLOR.test(value) ? value : fallback;
}

/** Legacy style fields (schema 0.1/0.2); kind-specific newer fields are added by toWire. */
function cleanStyle(style: AnnotationStyle | undefined): AnnotationStyle {
  const out: AnnotationStyle = {};
  if (style?.stroke !== undefined && HEX_COLOR.test(style.stroke)) out.stroke = style.stroke;
  if (style?.fill !== undefined && HEX_COLOR.test(style.fill)) out.fill = style.fill;
  if (style?.strokeWidthPx !== undefined) out.strokeWidthPx = Math.min(32, Math.max(0.5, style.strokeWidthPx));
  if (style?.fontSizePx !== undefined) out.fontSizePx = Math.min(128, Math.max(8, style.fontSizePx));
  return out;
}

/** True when the wire form of the list uses anything that needs schema 0.3 (new kinds, curve control, new style fields). */
export function requiresAdvancedSchema(list: readonly Annotation[]): boolean {
  return toWire(list).some((a) => {
    if (ADVANCED_KINDS.includes(a.kind)) return true;
    if (a.kind === 'arrow' && a.geometry.control !== undefined) return true;
    const s = a.style;
    return s !== undefined && (s.opacity !== undefined || s.effectSizePx !== undefined || s.arrowStyle !== undefined || s.textStyle !== undefined || s.smoothing !== undefined);
  });
}

/** Normalized crop rectangle to integer source pixels (at least 1x1, inside the image). */
export function cropToPixels(r: NormRect, size: ImageSize): { x: number; y: number; width: number; height: number } {
  const x = Math.min(size.width - 1, Math.max(0, Math.round(r.x * size.width)));
  const y = Math.min(size.height - 1, Math.max(0, Math.round(r.y * size.height)));
  const width = Math.max(1, Math.min(size.width - x, Math.round(r.width * size.width)));
  const height = Math.max(1, Math.min(size.height - y, Math.round(r.height * size.height)));
  return { x, y, width, height };
}
