/**
 * Base-image effects (blur, pixelate, spotlight dimming) as pure integer
 * pixel algorithms. The editing layer, the PNG rasterizer and the Gateway
 * (screenshot.redacted.png) must produce the same pixels, so everything here
 * is deterministic integer math on RGBA buffers that cover only the affected
 * rectangle; there are no per-radius inner loops (blur uses running sums).
 *
 * Blur and pixelate are cosmetic and NOT secure redaction. Solid redactions
 * are painted into the working base before any effect samples it and again
 * after everything else, so filtering can never carry a secret out of a
 * redacted area.
 */
import type { Annotation, NormRect } from '@/api/types';
import { EFFECT_SIZE_MAX, EFFECT_SIZE_MIN, effectSizeOf, type ImageSize } from './model';
import type { PathCmd } from './paths';

/** Integer pixel rectangle; the end coordinates are exclusive. */
export interface PixelRegion {
  x0: number;
  y0: number;
  x1: number;
  y1: number;
}

export interface EffectOp {
  id: string;
  kind: 'blur' | 'pixelate';
  region: PixelRegion;
  /** Blur radius or pixelate block edge in source pixels. */
  sizePx: number;
}

export function regionWidth(r: PixelRegion): number {
  return r.x1 - r.x0;
}

export function regionHeight(r: PixelRegion): number {
  return r.y1 - r.y0;
}

/**
 * Effect and spotlight rectangle in source pixels:
 * floor(x*w), floor(y*h), ceil((x+width)*w), ceil((y+height)*h), clamped to
 * the image. Null when nothing is left (zero-sized or off-image).
 */
export function effectRegion(g: NormRect, size: ImageSize): PixelRegion | null {
  if (!Number.isFinite(g.x + g.y + g.width + g.height)) return null;
  const x0 = Math.max(0, Math.min(size.width, Math.floor(g.x * size.width)));
  const y0 = Math.max(0, Math.min(size.height, Math.floor(g.y * size.height)));
  const x1 = Math.max(0, Math.min(size.width, Math.ceil((g.x + g.width) * size.width)));
  const y1 = Math.max(0, Math.min(size.height, Math.ceil((g.y + g.height) * size.height)));
  return x1 > x0 && y1 > y0 ? { x0, y0, x1, y1 } : null;
}

/**
 * Pixels a solid redaction covers: the exact floor of its origin plus the
 * ceiling of its extent and one spare pixel, so rounding can never leave a
 * sliver of the original uncovered. Clamped to the image.
 */
export function redactionRegion(g: NormRect, size: ImageSize): PixelRegion | null {
  if (!Number.isFinite(g.x + g.y + g.width + g.height)) return null;
  const x0 = Math.max(0, Math.min(size.width, Math.floor(g.x * size.width)));
  const y0 = Math.max(0, Math.min(size.height, Math.floor(g.y * size.height)));
  const x1 = Math.max(0, Math.min(size.width, x0 + Math.ceil(g.width * size.width) + 1));
  const y1 = Math.max(0, Math.min(size.height, y0 + Math.ceil(g.height * size.height) + 1));
  return x1 > x0 && y1 > y0 ? { x0, y0, x1, y1 } : null;
}

/** Blur/pixelate operations in annotation (list) order; each later op sees the result of the earlier ones. */
export function collectEffectOps(list: readonly Annotation[], size: ImageSize): EffectOp[] {
  const ops: EffectOp[] = [];
  for (const a of list) {
    if (a.kind !== 'blur' && a.kind !== 'pixelate') continue;
    const region = effectRegion(a.geometry, size);
    if (region) ops.push({ id: a.id, kind: a.kind, region, sizePx: effectSizeOf(a) });
  }
  return ops;
}

export function collectRedactionRegions(list: readonly Annotation[], size: ImageSize): PixelRegion[] {
  const out: PixelRegion[] = [];
  for (const a of list) {
    if (a.kind !== 'redact') continue;
    const region = redactionRegion(a.geometry, size);
    if (region) out.push(region);
  }
  return out;
}

/** Stable key of everything that decides effect pixels; unchanged when only selection, UI or non-effect annotations change. */
export function effectSignature(list: readonly Annotation[], size: ImageSize): string {
  const ops = collectEffectOps(list, size);
  if (ops.length === 0) return '';
  const reds = collectRedactionRegions(list, size);
  const r = (g: PixelRegion): string => `${g.x0},${g.y0},${g.x1},${g.y1}`;
  return `${size.width}x${size.height}|${ops.map((o) => `${o.id}:${o.kind}:${o.sizePx}:${r(o.region)}`).join(';')}|${reds.map(r).join(';')}`;
}

// ------------------------------------------------------------------ pixels

const clampSize = (sizePx: number): number => Math.min(EFFECT_SIZE_MAX, Math.max(EFFECT_SIZE_MIN, Math.round(sizePx)));

/**
 * Pixelates a region-local RGBA buffer in place. Blocks are `sizePx` square
 * and anchored at the buffer's top-left; each block becomes the rounded mean
 * of its RGB values (alpha 255).
 */
export function pixelateRgba(data: Uint8ClampedArray, width: number, height: number, sizePx: number): void {
  const block = clampSize(sizePx);
  for (let by = 0; by < height; by += block) {
    const yEnd = Math.min(height, by + block);
    for (let bx = 0; bx < width; bx += block) {
      const xEnd = Math.min(width, bx + block);
      let r = 0;
      let g = 0;
      let b = 0;
      for (let y = by; y < yEnd; y += 1) {
        let i = (y * width + bx) * 4;
        for (let x = bx; x < xEnd; x += 1) {
          r += data[i] ?? 0;
          g += data[i + 1] ?? 0;
          b += data[i + 2] ?? 0;
          i += 4;
        }
      }
      const count = (yEnd - by) * (xEnd - bx);
      const mr = Math.floor((2 * r + count) / (2 * count));
      const mg = Math.floor((2 * g + count) / (2 * count));
      const mb = Math.floor((2 * b + count) / (2 * count));
      for (let y = by; y < yEnd; y += 1) {
        let i = (y * width + bx) * 4;
        for (let x = bx; x < xEnd; x += 1) {
          data[i] = mr;
          data[i + 1] = mg;
          data[i + 2] = mb;
          data[i + 3] = 255;
          i += 4;
        }
      }
    }
  }
}

/**
 * One separable box-blur pass with edge clamping inside the line, using a
 * running window sum (O(length) per line regardless of the radius). The
 * result of each line sample is rounded to the nearest integer.
 */
function blurPass(src: Uint8ClampedArray, dst: Uint8ClampedArray, lines: number, length: number, lineStep: number, step: number, radius: number): void {
  const n = 2 * radius + 1;
  const last = length - 1;
  for (let line = 0; line < lines; line += 1) {
    const base = line * lineStep;
    for (let c = 0; c < 3; c += 1) {
      let sum = 0;
      for (let k = -radius; k <= radius; k += 1) sum += src[base + Math.min(last, Math.max(0, k)) * step + c] ?? 0;
      for (let i = 0; i < length; i += 1) {
        dst[base + i * step + c] = Math.floor((2 * sum + n) / (2 * n));
        sum += (src[base + Math.min(last, i + radius + 1) * step + c] ?? 0) - (src[base + Math.max(0, i - radius) * step + c] ?? 0);
      }
    }
  }
}

/**
 * Box-blurs a region-local RGBA buffer in place: horizontal pass then vertical
 * pass, window 2r+1, edges clamped to the buffer (nothing outside the region
 * is ever sampled), alpha 255. `scratch` may be supplied to reuse memory.
 */
export function boxBlurRgba(data: Uint8ClampedArray, width: number, height: number, radius: number, scratch?: Uint8ClampedArray): void {
  const r = clampSize(radius);
  const tmp = scratch && scratch.length >= data.length ? scratch : new Uint8ClampedArray(data.length);
  blurPass(data, tmp, height, width, width * 4, 4, r);
  blurPass(tmp, data, width, height, 4, width * 4, r);
  for (let i = 3; i < data.length; i += 4) data[i] = 255;
}

/** Runs one op on a region-local buffer (already cut out of the working base). */
export function applyEffectToBuffer(data: Uint8ClampedArray, op: EffectOp, scratch?: Uint8ClampedArray): void {
  const w = regionWidth(op.region);
  const h = regionHeight(op.region);
  if (op.kind === 'pixelate') pixelateRgba(data, w, h, op.sizePx);
  else boxBlurRgba(data, w, h, op.sizePx, scratch);
}

/** Dimmed channel value of the spotlight (round(c * (1 - 0.6)) in integers). */
export function dimChannel(c: number): number {
  return Math.floor((4 * c + 5) / 10);
}

// ---------------------------------------------------------------- spotlight

/** Regions of all spotlights, in list order. */
export function collectSpotlightRegions(list: readonly Annotation[], size: ImageSize): PixelRegion[] {
  const out: PixelRegion[] = [];
  for (const a of list) {
    if (a.kind !== 'spotlight') continue;
    const region = effectRegion(a.geometry, size);
    if (region) out.push(region);
  }
  return out;
}

/**
 * Disjoint rectangles that cover every pixel OUTSIDE the union of the given
 * spotlight regions. Empty when there is no spotlight (nothing is dimmed) or
 * when the union covers the whole image. Overlapping spotlights are a union,
 * so no pixel is ever dimmed twice.
 */
export function dimRegions(spots: readonly PixelRegion[], size: ImageSize): PixelRegion[] {
  if (spots.length === 0 || size.width <= 0 || size.height <= 0) return [];
  const ys = [...new Set([0, size.height, ...spots.flatMap((s) => [s.y0, s.y1])])].sort((a, b) => a - b);
  const out: PixelRegion[] = [];
  // Rectangles still growing downward, keyed by their horizontal extent.
  let open = new Map<string, PixelRegion>();
  for (let j = 0; j + 1 < ys.length; j += 1) {
    const top = ys[j] ?? 0;
    const bottom = ys[j + 1] ?? 0;
    const covered = spots
      .filter((s) => s.y0 <= top && s.y1 >= bottom)
      .map((s): [number, number] => [s.x0, s.x1])
      .sort((a, b) => a[0] - b[0]);
    const free: [number, number][] = [];
    let cursor = 0;
    for (const [x0, x1] of covered) {
      if (x0 > cursor) free.push([cursor, x0]);
      cursor = Math.max(cursor, x1);
    }
    if (cursor < size.width) free.push([cursor, size.width]);
    const next = new Map<string, PixelRegion>();
    for (const [x0, x1] of free) {
      const key = `${x0},${x1}`;
      const grown = open.get(key);
      if (grown) {
        grown.y1 = bottom;
        next.set(key, grown);
        open.delete(key);
      } else {
        const fresh = { x0, y0: top, x1, y1: bottom };
        out.push(fresh);
        next.set(key, fresh);
      }
    }
    open = next;
  }
  return out;
}

/** Closed rectangles as one path (a single fill, so abutting rectangles never show a seam). */
export function regionsPath(regions: readonly PixelRegion[]): PathCmd[] {
  return regions.flatMap((r): PathCmd[] => [
    { op: 'M', x: r.x0, y: r.y0 },
    { op: 'L', x: r.x1, y: r.y0 },
    { op: 'L', x: r.x1, y: r.y1 },
    { op: 'L', x: r.x0, y: r.y1 },
    { op: 'Z' },
  ]);
}

/** Dim path for a paint-ordered or raw annotation list (spotlights only). */
export function spotlightDimPath(list: readonly Annotation[], size: ImageSize): PathCmd[] {
  return regionsPath(dimRegions(collectSpotlightRegions(list, size), size));
}
