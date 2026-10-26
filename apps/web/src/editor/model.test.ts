import { describe, expect, it } from 'vitest';
import type { Annotation, NormRect, Point } from '@/api/types';
import {
  COORD_DECIMALS,
  boundsOf,
  cropToPixels,
  handlePosition,
  hitTest,
  moveBy,
  pinRadiusPx,
  pointFromClient,
  rectFromPoints,
  resizeRect,
  simplifyPath,
  snapPoint,
  snapRect,
  toSourcePixel,
  toWire,
  validateAnnotations,
} from './model';

const SIZE = { width: 1000, height: 500 };

function rect(partial: Partial<NormRect> = {}): NormRect {
  return { x: 0.1, y: 0.2, width: 0.3, height: 0.4, ...partial };
}

describe('snapRect', () => {
  it('keeps x+width and y+height inside the image', () => {
    const r = snapRect(rect({ x: 0.7, y: 0.8, width: 0.4, height: 0.4 }));
    expect(r.x + r.width).toBeLessThanOrEqual(1);
    expect(r.y + r.height).toBeLessThanOrEqual(1);
  });

  it('rounds to the coordinate grid', () => {
    const r = snapRect({ x: 0.123456789, y: 0, width: 0.5, height: 0.5 });
    expect(r.x).toBe(Number(r.x.toFixed(COORD_DECIMALS)));
  });

  it('clamps negative origins', () => {
    const r = snapRect({ x: -0.5, y: -1, width: 0.2, height: 0.2 });
    expect(r.x).toBe(0);
    expect(r.y).toBe(0);
  });
});

describe('pointFromClient', () => {
  const box = { left: 100, top: 50, width: 500, height: 250 };

  it('maps the content rectangle corners exactly', () => {
    expect(pointFromClient(100, 50, box)).toEqual({ x: 0, y: 0 });
    expect(pointFromClient(600, 300, box)).toEqual({ x: 1, y: 1 });
  });

  it('uses the image rectangle, not the panel: letterboxing is excluded', () => {
    // A click 25px inside the image box is 5% in, regardless of the surrounding panel.
    expect(pointFromClient(125, 75, box)).toEqual({ x: 0.05, y: 0.1 });
  });

  it('clamps clicks outside the image', () => {
    expect(pointFromClient(-500, 9999, box)).toEqual({ x: 0, y: 1 });
  });
});

describe('source pixels', () => {
  it('converts normalized points to source pixels', () => {
    expect(toSourcePixel({ x: 0.5, y: 0.5 }, SIZE)).toEqual({ x: 500, y: 250 });
  });

  it('never returns a pixel outside the image', () => {
    expect(toSourcePixel({ x: 1, y: 1 }, SIZE)).toEqual({ x: 999, y: 499 });
  });

  it('crops to integer pixels inside the image', () => {
    expect(cropToPixels({ x: 0.5, y: 0.5, width: 0.5, height: 0.5 }, SIZE)).toEqual({ x: 500, y: 250, width: 500, height: 250 });
    expect(cropToPixels({ x: 0.99, y: 0.99, width: 0.5, height: 0.5 }, SIZE)).toEqual({ x: 990, y: 495, width: 10, height: 5 });
  });
});

describe('rectFromPoints', () => {
  it('normalizes a drag in any direction', () => {
    const a = rectFromPoints({ x: 0.8, y: 0.9 }, { x: 0.2, y: 0.1 });
    expect(a.x).toBe(0.2);
    expect(a.y).toBe(0.1);
    expect(a.width).toBeCloseTo(0.6, 12);
    expect(a.height).toBeCloseTo(0.8, 12);
  });
});

describe('hitTest', () => {
  const box: Annotation = { id: 'r1', kind: 'rectangle', geometry: rect() };
  const pin: Annotation = { id: 'p1', kind: 'pin', geometry: { position: { x: 0.5, y: 0.5 }, number: 1 }, style: { fontSizePx: 20 } };

  it('hits a rectangle edge but not its hollow interior', () => {
    expect(hitTest([box], { x: 0.1, y: 0.4 }, SIZE, 5)).toBe('r1');
    expect(hitTest([box], { x: 0.25, y: 0.4 }, SIZE, 5)).toBeNull();
  });

  it('hits the topmost annotation', () => {
    const top: Annotation = { id: 'p2', kind: 'pin', geometry: { position: { x: 0.25, y: 0.4 }, number: 2 }, style: { fontSizePx: 40 } };
    expect(hitTest([box, top], { x: 0.25, y: 0.4 }, SIZE, 5)).toBe('p2');
  });

  it('hits an opaque redaction anywhere inside it', () => {
    const red: Annotation = { id: 'x1', kind: 'redact', mode: 'solid', geometry: rect() };
    expect(hitTest([red], { x: 0.25, y: 0.4 }, SIZE, 5)).toBe('x1');
  });

  it('uses the pin radius from its style', () => {
    const r = pinRadiusPx(pin.style) / SIZE.width;
    expect(hitTest([pin], { x: 0.5 + r * 0.9, y: 0.5 }, SIZE, 0)).toBe('p1');
  });
});

describe('moveBy', () => {
  it('clamps a rectangle inside the image', () => {
    const moved = moveBy({ id: 'r', kind: 'rectangle', geometry: rect({ x: 0.9, y: 0.1, width: 0.2, height: 0.2 }) }, 0.5, 0.5);
    if (moved.kind !== 'rectangle') throw new Error('kind changed');
    expect(moved.geometry.x + moved.geometry.width).toBeLessThanOrEqual(1);
    expect(moved.geometry.y + moved.geometry.height).toBeLessThanOrEqual(1);
  });

  it('keeps a freehand path together', () => {
    const path: Point[] = [
      { x: 0.2, y: 0.2 },
      { x: 0.3, y: 0.3 },
      { x: 0.4, y: 0.2 },
    ];
    const moved = moveBy({ id: 'f', kind: 'freehand', geometry: { points: path } }, 0.01, 0.01);
    if (moved.kind !== 'freehand') throw new Error('kind changed');
    expect(moved.geometry.points[0]).toEqual({ x: 0.21, y: 0.21 });
    expect(moved.geometry.points).toHaveLength(3);
  });
});

describe('resizeRect', () => {
  it('honours the minimum size and stays inside the image', () => {
    const r = resizeRect(rect({ x: 0.1, y: 0.1, width: 0.2, height: 0.2 }), 'nw', { x: 0.5, y: 0.5 }, SIZE, 2);
    expect(r.width).toBeGreaterThan(0);
    expect(r.x + r.width).toBeLessThanOrEqual(1);
    expect(r.y + r.height).toBeLessThanOrEqual(1);
  });

  it('drags the se handle only', () => {
    const start = rect();
    const r = resizeRect(start, 'se', { x: 0.9, y: 0.9 }, SIZE);
    expect(r.x).toBe(start.x);
    expect(r.y).toBe(start.y);
    expect(r.width).toBeGreaterThan(start.width);
  });
});

describe('handlePosition', () => {
  it('places handles on the rectangle border', () => {
    const r = rect();
    expect(handlePosition(r, 'nw')).toEqual({ x: r.x, y: r.y });
    expect(handlePosition(r, 'se')).toEqual({ x: r.x + r.width, y: r.y + r.height });
  });
});

describe('simplifyPath', () => {
  it('drops collinear points and keeps endpoints', () => {
    const points: Point[] = [];
    for (let i = 0; i <= 100; i += 1) points.push({ x: i / 100, y: 0.5 });
    const simplified = simplifyPath(points, SIZE, 0.5);
    expect(simplified).toHaveLength(2);
    expect(simplified[0]).toEqual(points[0]);
    expect(simplified[1]).toEqual(points[points.length - 1]);
  });
});

describe('boundsOf', () => {
  it('measures text with the injected measurer', () => {
    const a: Annotation = { id: 't', kind: 'text', geometry: { position: { x: 0.1, y: 0.1 }, text: 'hello\nworld' } , style: { fontSizePx: 20 } };
    const b = boundsOf(a, SIZE, () => 100);
    expect(b.width).toBeCloseTo(0.1);
    expect(b.height).toBeCloseTo((2 * 20 * 1.25) / SIZE.height);
  });
});

describe('toWire', () => {
  it('keeps redactions solid and style-free', () => {
    const out = toWire([{ id: 'x', kind: 'redact', mode: 'solid', geometry: rect(), comment: '', style: { stroke: '#ff0000' } } as unknown as Annotation]);
    const first = out[0];
    expect(first?.kind).toBe('redact');
    if (first?.kind === 'redact') {
      expect(first.mode).toBe('solid');
      expect('style' in first).toBe(false);
    }
  });

  it('drops out-of-range style values', () => {
    const out = toWire([{ id: 'r', kind: 'rectangle', geometry: rect(), comment: '', style: { stroke: 'red', strokeWidthPx: 999 } }]);
    const first = out[0];
    if (first?.kind !== 'rectangle') throw new Error('kind changed');
    expect(first.style?.stroke).toBeUndefined();
    expect(first.style?.strokeWidthPx).toBe(32);
  });
});

describe('validateAnnotations', () => {
  it('rejects rectangles that leave the image', () => {
    const issues = validateAnnotations([{ id: 'r', kind: 'rectangle', geometry: { x: 0.9, y: 0.1, width: 0.5, height: 0.1 } }]);
    expect(issues.some((i) => i.code === 'bounds')).toBe(true);
  });

  it('rejects duplicate pin numbers and duplicate ids', () => {
    const issues = validateAnnotations([
      { id: 'a', kind: 'pin', geometry: { position: { x: 0.1, y: 0.1 }, number: 1 } },
      { id: 'a', kind: 'pin', geometry: { position: { x: 0.2, y: 0.2 }, number: 1 } },
    ]);
    expect(issues.some((i) => i.code === 'pinDuplicate')).toBe(true);
    expect(issues.some((i) => i.code === 'duplicateId')).toBe(true);
  });

  it('accepts a valid mixed set', () => {
    expect(
      validateAnnotations([
        { id: 'r', kind: 'rectangle', geometry: rect() },
        { id: 'p', kind: 'pin', geometry: { position: { x: 0.5, y: 0.5 }, number: 3 } },
        { id: 't', kind: 'text', geometry: { position: { x: 0.1, y: 0.1 }, text: 'ok' } },
      ]),
    ).toEqual([]);
  });

  it('has no annotation, freehand point or pin number caps', () => {
    const points = Array.from({ length: 10001 }, (_, i) => ({ x: (i % 100) / 100, y: 0.5 }));
    const many = Array.from({ length: 150 }, (_, i): Annotation => ({ id: `n${i}`, kind: 'pin', geometry: { position: { x: 0.5, y: 0.5 }, number: i + 1 } }));
    expect(validateAnnotations([{ id: 'f', kind: 'freehand', geometry: { points } }, ...many])).toEqual([]);
  });

  it('still rejects a one-point freehand stroke', () => {
    expect(validateAnnotations([{ id: 'f', kind: 'freehand', geometry: { points: [{ x: 0, y: 0 }] } }]).some((i) => i.code === 'freehandPoints')).toBe(true);
  });
});

describe('snapPoint', () => {
  it('clamps to the image', () => {
    expect(snapPoint({ x: 2, y: -1 })).toEqual({ x: 1, y: 0 });
  });
});
