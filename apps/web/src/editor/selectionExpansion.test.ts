import { describe, expect, it } from 'vitest';
import type { Annotation, ArrowAnnotation, NormRect, Point } from '@/api/types';
import { alignSelection, duplicateSelection, marqueeSelection, moveSelection, resizeSelection, selectionBounds } from './selection';
import { boundsOf } from './model';

const SIZE = { width: 1000, height: 500 };
const ids = (...v: string[]): Set<string> => new Set(v);

function box(id: string, kind: 'highlight' | 'spotlight' | 'blur' | 'pixelate', g: NormRect): Annotation {
  return { id, kind, geometry: g };
}

function curved(id: string, start: Point, end: Point, control: Point): ArrowAnnotation {
  return { id, kind: 'arrow', geometry: { start, end, control }, style: { arrowStyle: 'curved' } };
}

function inUnit(p: Point | undefined): boolean {
  return p !== undefined && p.x >= 0 && p.x <= 1 && p.y >= 0 && p.y <= 1;
}

describe('resizeSelection with the expanded kinds', () => {
  it('maps a curved arrow control through the same affine map as its endpoints', () => {
    const a = curved('a', { x: 0.2, y: 0.2 }, { x: 0.4, y: 0.2 }, { x: 0.3, y: 0.4 });
    const before = selectionBounds([a], SIZE);
    if (!before) throw new Error('bounds expected');
    // The box includes the control: 0.2..0.4 x 0.2..0.4. Doubling it about its corner scales every point.
    const [out] = resizeSelection([a], ids('a'), before, { x: 0.2, y: 0.2, width: 0.4, height: 0.4 }, SIZE);
    if (out?.kind !== 'arrow') throw new Error('arrow expected');
    expect(out.geometry.start.x).toBeCloseTo(0.2, 6);
    expect(out.geometry.end.x).toBeCloseTo(0.6, 6);
    expect(out.geometry.control?.x).toBeCloseTo(0.4, 6);
    expect(out.geometry.control?.y).toBeCloseTo(0.6, 6);
    expect(out.style).toEqual({ arrowStyle: 'curved' });
  });

  it.each(['highlight', 'spotlight', 'blur', 'pixelate'] as const)('resizes a %s like any rectangle and keeps it inside the image', (kind) => {
    const a = box('b', kind, { x: 0.2, y: 0.2, width: 0.2, height: 0.2 });
    const [out] = resizeSelection([a], ids('b'), { x: 0.2, y: 0.2, width: 0.2, height: 0.2 }, { x: 0.1, y: 0.1, width: 1.5, height: 0.6 }, SIZE);
    if (!out || out.kind !== kind) throw new Error('kind changed');
    expect(out.geometry.x).toBeCloseTo(0.1, 6);
    expect(out.geometry.x + out.geometry.width).toBeLessThanOrEqual(1);
    expect(out.geometry.y + out.geometry.height).toBeLessThanOrEqual(1);
  });

  it('never collapses an effect rectangle below one source pixel', () => {
    const a = box('b', 'blur', { x: 0.2, y: 0.2, width: 0.2, height: 0.2 });
    const [out] = resizeSelection([a], ids('b'), { x: 0.2, y: 0.2, width: 0.2, height: 0.2 }, { x: 0.5, y: 0.5, width: 0, height: 0 }, SIZE);
    if (out?.kind !== 'blur') throw new Error('blur expected');
    expect(out.geometry.width).toBeGreaterThanOrEqual(1 / SIZE.width - 1e-9);
    expect(out.geometry.height).toBeGreaterThanOrEqual(1 / SIZE.height - 1e-9);
  });
});

describe('moveSelection with the expanded kinds', () => {
  it('stops the whole group at the first edge, counting the curve control', () => {
    const arrow = curved('a', { x: 0.2, y: 0.5 }, { x: 0.4, y: 0.5 }, { x: 0.95, y: 0.7 });
    const hl = box('h', 'highlight', { x: 0.1, y: 0.1, width: 0.2, height: 0.2 });
    const out = moveSelection([arrow, hl], ids('a', 'h'), 0.2, 0);
    const [a, h] = out;
    if (a?.kind !== 'arrow' || h?.kind !== 'highlight') throw new Error('kinds changed');
    // The control is at x=0.95, so the shared delta is 0.05 and nothing shears.
    expect(a.geometry.start.x).toBeCloseTo(0.25, 6);
    expect(a.geometry.control?.x).toBeCloseTo(1, 6);
    expect(h.geometry.x).toBeCloseTo(0.15, 6);
  });
});

describe('duplicateSelection with the expanded kinds', () => {
  it('copies a curved arrow near the image edge with its control and keeps everything normalized', () => {
    const arrow = curved('a', { x: 0.9, y: 0.9 }, { x: 0.98, y: 0.98 }, { x: 0.95, y: 0.99 });
    const hl = box('h', 'highlight', { x: 0.85, y: 0.85, width: 0.15, height: 0.15 });
    const { annotations, selectedIds } = duplicateSelection([arrow, hl], ids('a', 'h'), SIZE);
    expect(annotations).toHaveLength(4);
    expect(selectedIds).toHaveLength(2);
    expect(new Set(annotations.map((a) => a.id)).size).toBe(4);
    const copy = annotations[2];
    if (copy?.kind !== 'arrow') throw new Error('arrow copy expected');
    expect(inUnit(copy.geometry.start) && inUnit(copy.geometry.end) && inUnit(copy.geometry.control)).toBe(true);
    const dx = copy.geometry.start.x - arrow.geometry.start.x;
    const dy = copy.geometry.start.y - arrow.geometry.start.y;
    expect(copy.geometry.control?.x).toBeCloseTo(0.95 + dx, 6);
    expect(copy.geometry.control?.y).toBeCloseTo(0.99 + dy, 6);
    expect(copy.style).toEqual({ arrowStyle: 'curved' });
    expect(copy.style).not.toBe(arrow.style);
    const hlCopy = annotations[3];
    if (hlCopy?.kind !== 'highlight') throw new Error('highlight copy expected');
    expect(hlCopy.geometry.x + hlCopy.geometry.width).toBeLessThanOrEqual(1);
    expect(hlCopy.geometry.y + hlCopy.geometry.height).toBeLessThanOrEqual(1);
  });
});

describe('marquee and align with the expanded kinds', () => {
  it('selects a curved arrow by its control point, a straight one only by its endpoints', () => {
    const start = { x: 0.1, y: 0.1 };
    const end = { x: 0.2, y: 0.1 };
    const marquee = { x: 0.12, y: 0.35, width: 0.06, height: 0.1 };
    const arrow = curved('a', start, end, { x: 0.15, y: 0.4 });
    expect(marqueeSelection([arrow], marquee, SIZE)).toEqual(['a']);
    const straight: Annotation = { ...arrow, style: {} };
    expect(marqueeSelection([straight], marquee, SIZE)).toEqual([]);
  });

  it('aligns new rectangle kinds by their visual bounds', () => {
    const list = [box('b', 'blur', { x: 0.1, y: 0.1, width: 0.2, height: 0.2 }), box('p', 'pixelate', { x: 0.5, y: 0.4, width: 0.1, height: 0.1 })];
    const [b, p] = alignSelection(list, ids('b', 'p'), 'left', SIZE);
    if (!b || !p) throw new Error('list changed');
    expect(boundsOf(b, SIZE).x).toBeCloseTo(0.1, 6);
    expect(boundsOf(p, SIZE).x).toBeCloseTo(0.1, 6);
    expect(boundsOf(p, SIZE).y).toBeCloseTo(0.4, 6);
  });
});
