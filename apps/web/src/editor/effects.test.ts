import { describe, expect, it } from 'vitest';
import type { Annotation } from '@/api/types';
import {
  boxBlurRgba,
  collectEffectOps,
  collectRedactionRegions,
  collectSpotlightRegions,
  dimChannel,
  dimRegions,
  effectRegion,
  effectSignature,
  pixelateRgba,
  redactionRegion,
  type EffectOp,
  type PixelRegion,
} from './effects';
import { applyBaseEffects } from './effectsCanvas';

const SIZE = { width: 1000, height: 500 };

/** RGBA buffer of `values` (one gray value per pixel). */
function gray(values: readonly number[]): Uint8ClampedArray {
  const data = new Uint8ClampedArray(values.length * 4);
  values.forEach((v, i) => data.set([v, v, v, 255], i * 4));
  return data;
}

function channel(data: Uint8ClampedArray, channelIndex = 0): number[] {
  return Array.from({ length: data.length / 4 }, (_, i) => data[i * 4 + channelIndex] ?? -1);
}

function box(id: string, kind: 'blur' | 'pixelate' | 'spotlight' | 'highlight' | 'rectangle', x: number, y: number, width: number, height: number, style?: Annotation['style']): Annotation {
  return { id, kind, geometry: { x, y, width, height }, ...(style ? { style } : {}) } as Annotation;
}

describe('effectRegion (shared pixel rectangle)', () => {
  it('floors the origin and ceils the far edge', () => {
    expect(effectRegion({ x: 0.25, y: 0.25, width: 0.5, height: 0.5 }, SIZE)).toEqual({ x0: 250, y0: 125, x1: 750, y1: 375 });
    expect(effectRegion({ x: 0.1234, y: 0.5, width: 0.2, height: 0.25 }, SIZE)).toEqual({ x0: 123, y0: 250, x1: 324, y1: 375 });
  });

  it('clamps to the image and drops empty or non-finite rectangles', () => {
    expect(effectRegion({ x: 0.9, y: 0.9, width: 0.5, height: 0.5 }, SIZE)).toEqual({ x0: 900, y0: 450, x1: 1000, y1: 500 });
    expect(effectRegion({ x: 0.5, y: 0.5, width: 0, height: 0.2 }, SIZE)).toBeNull();
    expect(effectRegion({ x: Number.NaN, y: 0, width: 0.2, height: 0.2 }, SIZE)).toBeNull();
  });
});

describe('redactionRegion', () => {
  it('covers one spare pixel beyond the ceiling so rounding never leaves a sliver', () => {
    expect(redactionRegion({ x: 0.25, y: 0.25, width: 0.5, height: 0.5 }, SIZE)).toEqual({ x0: 250, y0: 125, x1: 751, y1: 376 });
  });

  it('is clamped to the image', () => {
    expect(redactionRegion({ x: 0.5, y: 0, width: 0.5, height: 1 }, SIZE)).toEqual({ x0: 500, y0: 0, x1: 1000, y1: 500 });
  });
});

describe('pixelateRgba', () => {
  it('averages blocks anchored at the top-left, with partial blocks at the right and bottom', () => {
    const data = gray([0, 10, 20, 30, 40, 50, 60, 70, 80]);
    pixelateRgba(data, 3, 3, 2);
    expect(channel(data)).toEqual([20, 20, 35, 20, 20, 35, 65, 65, 80]);
    expect(channel(data, 3).every((a) => a === 255)).toBe(true);
  });

  it('rounds half up to the nearest integer and forces alpha 255', () => {
    const data = new Uint8ClampedArray([20, 20, 20, 7, 21, 21, 21, 7]);
    pixelateRgba(data, 2, 1, 2);
    expect(Array.from(data)).toEqual([21, 21, 21, 255, 21, 21, 21, 255]);
  });

  it('bounds the block edge to 2..64', () => {
    const small = gray([0, 100, 0, 100]);
    pixelateRgba(small, 4, 1, 1); // below the minimum: behaves as 2
    expect(channel(small)).toEqual([50, 50, 50, 50]);
  });
});

describe('boxBlurRgba', () => {
  it('uses a clamped window of 2r+1 inside the region and rounds to nearest', () => {
    const data = gray([0, 100, 200]);
    boxBlurRgba(data, 3, 1, 2);
    expect(channel(data)).toEqual([60, 100, 140]);
  });

  it('spreads a single bright pixel without sampling outside the buffer', () => {
    const data = gray([0, 0, 255, 0, 0]);
    boxBlurRgba(data, 5, 1, 2);
    expect(channel(data)).toEqual([51, 51, 51, 51, 51]);
  });

  it('blurs along both axes (horizontal pass, then vertical pass)', () => {
    // 3x3 with a single bright center pixel: the box mean of the center column is the same both ways.
    const data = gray([0, 0, 0, 0, 255, 0, 0, 0, 0]);
    boxBlurRgba(data, 3, 3, 2);
    const out = channel(data);
    // Horizontal pass: row 1 becomes 51 everywhere, other rows stay 0. Vertical pass: every column sees 0,51,0 => floor((2*51+5)/10).
    expect(new Set(out).size).toBe(1);
    expect(out[0]).toBe(Math.floor((2 * 51 + 5) / 10));
  });

  it('treats radii below 2 as 2 and keeps alpha opaque', () => {
    const a = gray([0, 100, 200]);
    const b = gray([0, 100, 200]);
    boxBlurRgba(a, 3, 1, 1);
    boxBlurRgba(b, 3, 1, 2);
    expect(Array.from(a)).toEqual(Array.from(b));
    expect(channel(a, 3).every((v) => v === 255)).toBe(true);
  });

  it('is a no-op on flat color', () => {
    const data = gray(new Array<number>(6 * 4).fill(77));
    boxBlurRgba(data, 6, 4, 64);
    expect(new Set(channel(data))).toEqual(new Set([77]));
  });
});

describe('collectEffectOps', () => {
  it('keeps annotation order, snaps sizes to 2..64 integers, applies per-kind defaults and skips other kinds', () => {
    const list: Annotation[] = [
      box('a', 'blur', 0.1, 0.1, 0.2, 0.2),
      box('r', 'rectangle', 0.1, 0.1, 0.2, 0.2),
      box('b', 'pixelate', 0.5, 0.5, 0.2, 0.2),
      box('c', 'blur', 0.3, 0.3, 0.1, 0.1, { effectSizePx: 100 }),
      box('d', 'blur', 0.3, 0.3, 0.1, 0.1, { effectSizePx: 1 }),
      box('e', 'blur', 0.3, 0.3, 0.1, 0.1, { effectSizePx: 7.6 }),
      box('z', 'blur', 0.3, 0.3, 0, 0.1),
    ];
    const ops = collectEffectOps(list, SIZE);
    expect(ops.map((o) => [o.id, o.kind, o.sizePx])).toEqual([
      ['a', 'blur', 12],
      ['b', 'pixelate', 16],
      ['c', 'blur', 64],
      ['d', 'blur', 2],
      ['e', 'blur', 8],
    ]);
  });
});

describe('effectSignature', () => {
  const base: Annotation[] = [box('b', 'blur', 0.25, 0.25, 0.5, 0.5, { effectSizePx: 10 })];

  it('is empty without effects so the image is never re-filtered', () => {
    expect(effectSignature([box('s', 'spotlight', 0.1, 0.1, 0.2, 0.2), box('h', 'highlight', 0.1, 0.1, 0.2, 0.2)], SIZE)).toBe('');
  });

  it('ignores non-effect annotations and sub-pixel moves that keep the same pixels', () => {
    const sig = effectSignature(base, SIZE);
    const extra: Annotation = { id: 'arrow', kind: 'arrow', geometry: { start: { x: 0, y: 0 }, end: { x: 1, y: 1 } } };
    expect(effectSignature([...base, extra], SIZE)).toBe(sig);
    // x 250.4 vs 250.6 and far edge 650.4 vs 650.6: both snap to the same pixels 250..651.
    const a = effectSignature([box('b', 'blur', 0.2504, 0.25, 0.4, 0.5, { effectSizePx: 10 })], SIZE);
    const b = effectSignature([box('b', 'blur', 0.2506, 0.25, 0.4, 0.5, { effectSizePx: 10 })], SIZE);
    expect(a).toBe(b);
  });

  it('changes with the effect rectangle, size, image size and redactions', () => {
    const sig = effectSignature(base, SIZE);
    expect(effectSignature([box('b', 'blur', 0.26, 0.25, 0.5, 0.5, { effectSizePx: 10 })], SIZE)).not.toBe(sig);
    expect(effectSignature([box('b', 'blur', 0.25, 0.25, 0.5, 0.5, { effectSizePx: 11 })], SIZE)).not.toBe(sig);
    expect(effectSignature(base, { width: 1001, height: 500 })).not.toBe(sig);
    const redact: Annotation = { id: 'x', kind: 'redact', mode: 'solid', geometry: { x: 0.3, y: 0.3, width: 0.1, height: 0.1 } };
    expect(effectSignature([...base, redact], SIZE)).not.toBe(sig);
  });
});

describe('dimRegions (spotlight union)', () => {
  const size = { width: 10, height: 10 };

  function coverage(regions: readonly PixelRegion[]): number[] {
    const counts = new Array<number>(size.width * size.height).fill(0);
    for (const r of regions) {
      expect(Number.isInteger(r.x0 + r.y0 + r.x1 + r.y1)).toBe(true);
      for (let y = r.y0; y < r.y1; y += 1) for (let x = r.x0; x < r.x1; x += 1) counts[y * size.width + x] = (counts[y * size.width + x] ?? 0) + 1;
    }
    return counts;
  }

  it('dims every pixel outside the union exactly once, including where spotlights overlap', () => {
    const a: PixelRegion = { x0: 1, y0: 1, x1: 5, y1: 5 };
    const b: PixelRegion = { x0: 3, y0: 3, x1: 7, y1: 7 };
    const counts = coverage(dimRegions([a, b], size));
    for (let y = 0; y < size.height; y += 1) {
      for (let x = 0; x < size.width; x += 1) {
        const inside = [a, b].some((r) => x >= r.x0 && x < r.x1 && y >= r.y0 && y < r.y1);
        expect(counts[y * size.width + x]).toBe(inside ? 0 : 1);
      }
    }
  });

  it('dims nothing without spotlights or when they cover the whole image', () => {
    expect(dimRegions([], size)).toEqual([]);
    expect(dimRegions([{ x0: 0, y0: 0, x1: 10, y1: 10 }], size)).toEqual([]);
    expect(dimRegions([{ x0: 0, y0: 0, x1: 10, y1: 4 }, { x0: 0, y0: 4, x1: 10, y1: 10 }], size)).toEqual([]);
  });

  it('handles spotlights touching the image edges', () => {
    const counts = coverage(dimRegions([{ x0: 0, y0: 0, x1: 3, y1: 10 }], size));
    expect(counts.filter((c) => c === 1).length).toBe(70);
    expect(counts.filter((c) => c === 0).length).toBe(30);
  });

  it('takes spotlight regions from annotations in list order', () => {
    const list: Annotation[] = [box('s1', 'spotlight', 0.25, 0.25, 0.5, 0.5), box('b', 'blur', 0, 0, 0.1, 0.1), box('s2', 'spotlight', 0, 0, 0.5, 0.5)];
    expect(collectSpotlightRegions(list, SIZE)).toEqual([
      { x0: 250, y0: 125, x1: 750, y1: 375 },
      { x0: 0, y0: 0, x1: 500, y1: 250 },
    ]);
  });
});

describe('dimChannel', () => {
  it('is round(c * 0.4) in integers', () => {
    expect([0, 1, 128, 255].map(dimChannel)).toEqual([0, 0, 51, 102]);
  });
});

interface FakeImage {
  data: Uint8ClampedArray;
  width: number;
  height: number;
}

/** Minimal in-memory 2D context: only what applyBaseEffects touches. */
class FakeContext {
  fillStyle = '';
  readonly fills: string[] = [];
  readonly data: Uint8ClampedArray;

  constructor(
    readonly width: number,
    readonly height: number,
    fill: [number, number, number],
  ) {
    this.data = new Uint8ClampedArray(width * height * 4);
    for (let i = 0; i < width * height; i += 1) this.data.set([...fill, 255], i * 4);
  }

  setPixel(x: number, y: number, rgb: [number, number, number]): void {
    this.data.set([...rgb, 255], (y * this.width + x) * 4);
  }

  pixel(x: number, y: number): number[] {
    const i = (y * this.width + x) * 4;
    return [this.data[i] ?? -1, this.data[i + 1] ?? -1, this.data[i + 2] ?? -1];
  }

  fillRect(x: number, y: number, w: number, h: number): void {
    this.fills.push(this.fillStyle);
    for (let yy = y; yy < y + h; yy += 1) for (let xx = x; xx < x + w; xx += 1) this.setPixel(xx, yy, [0, 0, 0]);
  }

  getImageData(x: number, y: number, w: number, h: number): FakeImage {
    const data = new Uint8ClampedArray(w * h * 4);
    for (let yy = 0; yy < h; yy += 1) {
      const from = ((y + yy) * this.width + x) * 4;
      data.set(this.data.subarray(from, from + w * 4), yy * w * 4);
    }
    return { data, width: w, height: h };
  }

  putImageData(image: FakeImage, x: number, y: number): void {
    for (let yy = 0; yy < image.height; yy += 1) {
      this.data.set(image.data.subarray(yy * image.width * 4, (yy + 1) * image.width * 4), ((y + yy) * this.width + x) * 4);
    }
  }
}

function run(ctx: FakeContext, origin: { x: number; y: number }, ops: EffectOp[], reds: PixelRegion[]): void {
  applyBaseEffects(ctx as unknown as CanvasRenderingContext2D, origin, ops, reds);
}

describe('applyBaseEffects (secure order)', () => {
  it('blackens redacted pixels before blurring so a secret cannot bleed out', () => {
    const ctx = new FakeContext(12, 4, [255, 255, 255]);
    for (let y = 0; y < 4; y += 1) {
      ctx.setPixel(4, y, [255, 0, 0]);
      ctx.setPixel(5, y, [255, 0, 0]);
    }
    const blur = box('b', 'blur', 0, 0, 1, 1, { effectSizePx: 2 });
    run(ctx, { x: 0, y: 0 }, collectEffectOps([blur], { width: 12, height: 4 }), [{ x0: 4, y0: 0, x1: 6, y1: 4 }]);
    for (let y = 0; y < 4; y += 1) {
      for (let x = 0; x < 12; x += 1) {
        const [r, g, b] = ctx.pixel(x, y);
        // Without the pre-redaction the red secret would leave r > g near the redaction.
        expect(r).toBe(g);
        expect(g).toBe(b);
      }
    }
    // The blur really ran (the black bled into its neighbors) and far pixels are untouched.
    expect(ctx.pixel(3, 1)[0]).toBeLessThan(255);
    expect(ctx.pixel(0, 1)[0]).toBe(255);
  });

  it('does nothing, not even a redaction fill, without effect ops', () => {
    const ctx = new FakeContext(4, 4, [9, 9, 9]);
    run(ctx, { x: 0, y: 0 }, [], [{ x0: 0, y0: 0, x1: 4, y1: 4 }]);
    expect(ctx.fills).toEqual([]);
    expect(ctx.pixel(1, 1)).toEqual([9, 9, 9]);
  });

  it('maps image pixels onto a working canvas that starts at an offset', () => {
    // Working canvas shows image pixels (10..13, 20..21).
    const ctx = new FakeContext(4, 2, [0, 0, 0]);
    ctx.setPixel(1, 0, [100, 100, 100]);
    ctx.setPixel(2, 0, [200, 200, 200]);
    // Binary-exact fractions keep the floor/ceil boundaries free of float noise.
    const pixelate = box('p', 'pixelate', 11 / 128, 20 / 128, 2 / 128, 2 / 128, { effectSizePx: 2 });
    const ops = collectEffectOps([pixelate], { width: 128, height: 128 });
    expect(ops[0]?.region).toEqual({ x0: 11, y0: 20, x1: 13, y1: 22 });
    run(ctx, { x: 10, y: 20 }, ops, []);
    // 2x2 block at canvas (1..2, 0..1): mean of (100, 200, 0, 0) = 75; outside pixels untouched.
    expect(ctx.pixel(1, 0)).toEqual([75, 75, 75]);
    expect(ctx.pixel(2, 1)).toEqual([75, 75, 75]);
    expect(ctx.pixel(0, 0)).toEqual([0, 0, 0]);
    expect(ctx.pixel(3, 0)).toEqual([0, 0, 0]);
  });

  it('collects every redaction region of a list', () => {
    const redact: Annotation = { id: 'x', kind: 'redact', mode: 'solid', geometry: { x: 0.25, y: 0.25, width: 0.5, height: 0.5 } };
    expect(collectRedactionRegions([redact, box('r', 'rectangle', 0, 0, 1, 1)], SIZE)).toEqual([{ x0: 250, y0: 125, x1: 751, y1: 376 }]);
  });
});
