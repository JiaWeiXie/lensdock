import { describe, expect, it } from 'vitest';
import type { Annotation, EllipseAnnotation, NormRect, Point } from '@/api/types';
import { boundsOf, fillOf, hitTest, moveBy, toWire, validateAnnotations } from './model';
import {
  alignSelection,
  duplicateSelection,
  marqueeSelection,
  moveSelection,
  reorderSelection,
  resizeSelection,
  selectionBounds,
} from './selection';
import { shapePrimitive } from './shapes';

const SIZE = { width: 1000, height: 500 };
const ids = (...v: string[]): Set<string> => new Set(v);
const measure = (text: string, fontSizePx: number): number => text.length * fontSizePx * 0.5;

function box(id: string, kind: 'rectangle' | 'ellipse' | 'diamond' | 'redact', g: NormRect, extra: Partial<Annotation> = {}): Annotation {
  if (kind === 'redact') return { id, kind, mode: 'solid', geometry: g };
  return { id, kind, geometry: g, ...extra } as Annotation;
}

function seg(id: string, kind: 'arrow' | 'line', start: Point, end: Point): Annotation {
  return { id, kind, geometry: { start, end } };
}

function pin(id: string, number: number, x: number, y: number): Annotation {
  return { id, kind: 'pin', geometry: { position: { x, y }, number } };
}

function text(id: string, x: number, y: number, value = 'hello'): Annotation {
  return { id, kind: 'text', geometry: { position: { x, y }, text: value }, style: { fontSizePx: 20 } };
}

function allPoints(a: Annotation): Point[] {
  const b = boundsOf(a, SIZE, measure);
  switch (a.kind) {
    case 'arrow':
    case 'line':
      return [a.geometry.start, a.geometry.end];
    case 'freehand':
      return a.geometry.points;
    case 'text':
    case 'pin':
      return [a.geometry.position];
    default:
      return [
        { x: b.x, y: b.y },
        { x: b.x + b.width, y: b.y + b.height },
      ];
  }
}

function expectNormalized(list: readonly Annotation[]): void {
  for (const a of list) {
    for (const p of allPoints(a)) {
      expect(p.x).toBeGreaterThanOrEqual(0);
      expect(p.x).toBeLessThanOrEqual(1);
      expect(p.y).toBeGreaterThanOrEqual(0);
      expect(p.y).toBeLessThanOrEqual(1);
      expect(Number.isFinite(p.x) && Number.isFinite(p.y)).toBe(true);
    }
  }
}

const scene = (): Annotation[] => [
  box('r', 'rectangle', { x: 0.1, y: 0.1, width: 0.2, height: 0.2 }),
  box('e', 'ellipse', { x: 0.5, y: 0.2, width: 0.2, height: 0.3 }),
  seg('l', 'line', { x: 0.2, y: 0.8 }, { x: 0.6, y: 0.9 }),
  pin('p', 1, 0.8, 0.5),
  text('t', 0.3, 0.6),
  box('d', 'diamond', { x: 0.7, y: 0.6, width: 0.1, height: 0.2 }),
];

describe('new shape kinds', () => {
  it('bounds ellipse/diamond like a rectangle and line like an arrow', () => {
    const g = { x: 0.1, y: 0.2, width: 0.3, height: 0.4 };
    expect(boundsOf(box('e', 'ellipse', g), SIZE)).toEqual(g);
    expect(boundsOf(box('d', 'diamond', g), SIZE)).toEqual(g);
    expect(boundsOf(seg('l', 'line', { x: 0.5, y: 0.1 }, { x: 0.2, y: 0.4 }), SIZE)).toEqual({
      x: 0.2, y: 0.1, width: expect.closeTo(0.3, 6), height: expect.closeTo(0.3, 6),
    });
  });

  it('hits an ellipse outline but not its hollow center, and the center when filled', () => {
    const g = { x: 0.2, y: 0.2, width: 0.4, height: 0.4 };
    const hollow = box('e', 'ellipse', g);
    const filled = box('f', 'ellipse', g, { style: { fill: '#00ff00' } });
    expect(hitTest([hollow], { x: 0.2, y: 0.4 }, SIZE, 5)).toBe('e');
    expect(hitTest([hollow], { x: 0.4, y: 0.4 }, SIZE, 5)).toBeNull();
    expect(hitTest([filled], { x: 0.4, y: 0.4 }, SIZE, 5)).toBe('f');
    // The bounding-box corner lies outside an ellipse, filled or not.
    expect(hitTest([filled], { x: 0.21, y: 0.21 }, SIZE, 2)).toBeNull();
  });

  it('hits a diamond along its slanted edges, not in the bounding-box corner', () => {
    const d = box('d', 'diamond', { x: 0.2, y: 0.2, width: 0.4, height: 0.4 });
    const midOfUpperLeftEdge = { x: 0.3, y: 0.3 };
    expect(hitTest([d], midOfUpperLeftEdge, SIZE, 4)).toBe('d');
    expect(hitTest([d], { x: 0.21, y: 0.21 }, SIZE, 4)).toBeNull();
    expect(hitTest([d], { x: 0.4, y: 0.4 }, SIZE, 4)).toBeNull();
    const filled = box('f', 'diamond', { x: 0.2, y: 0.2, width: 0.4, height: 0.4 }, { style: { fill: '#112233' } });
    expect(hitTest([filled], { x: 0.4, y: 0.4 }, SIZE, 4)).toBe('f');
  });

  it('hits a line within tolerance of the segment', () => {
    const l = seg('l', 'line', { x: 0.1, y: 0.5 }, { x: 0.9, y: 0.5 });
    expect(hitTest([l], { x: 0.5, y: 0.5 + 3 / SIZE.height }, SIZE, 4)).toBe('l');
    expect(hitTest([l], { x: 0.5, y: 0.6 }, SIZE, 4)).toBeNull();
  });

  it('ignores a malformed fill and non-fillable kinds', () => {
    expect(fillOf(box('r', 'rectangle', { x: 0, y: 0, width: 0.1, height: 0.1 }, { style: { fill: 'red' } }))).toBeNull();
    expect(fillOf({ ...seg('l', 'line', { x: 0, y: 0 }, { x: 1, y: 1 }), style: { fill: '#112233' } })).toBeNull();
    expect(fillOf(box('d', 'diamond', { x: 0, y: 0, width: 0.1, height: 0.1 }, { style: { fill: '#112233' } }))).toBe('#112233');
  });

  it('serializes every new kind with its own kind and a clean style', () => {
    const wire = toWire([
      box('e', 'ellipse', { x: 0.1, y: 0.1, width: 0.2, height: 0.2 }, { style: { stroke: '#e5362b', fill: '#00FF00', strokeWidthPx: 99 } }),
      box('d', 'diamond', { x: 0.4, y: 0.1, width: 0.2, height: 0.2 }, { style: { fill: 'nope' } }),
      seg('l', 'line', { x: 0.1, y: 0.7 }, { x: 0.9, y: 0.8 }),
    ]);
    expect(wire.map((a) => a.kind)).toEqual(['ellipse', 'diamond', 'line']);
    expect(wire[0]?.style).toEqual({ stroke: '#e5362b', fill: '#00FF00', strokeWidthPx: 32 });
    expect(wire[1]?.style).toEqual({});
    expect(validateAnnotations(wire)).toEqual([]);
  });

  it('validates new box kinds like rectangles', () => {
    const issues = validateAnnotations([
      box('e', 'ellipse', { x: 0.9, y: 0.1, width: 0.5, height: 0.1 }),
      box('d', 'diamond', { x: 0.1, y: 0.1, width: 0, height: 0.1 }),
    ]);
    expect(issues).toEqual([
      { code: 'bounds', id: 'e' },
      { code: 'bounds', id: 'd' },
    ]);
  });

  it('keeps moveBy inside the image for every new kind', () => {
    for (const a of [box('e', 'ellipse', { x: 0.8, y: 0.8, width: 0.2, height: 0.2 }), seg('l', 'line', { x: 0.9, y: 0.9 }, { x: 1, y: 1 })]) {
      expectNormalized([moveBy(a, 0.5, 0.5), moveBy(a, -2, -2)]);
    }
  });

  it('describes both renderers from one primitive (fill under stroke, px coordinates)', () => {
    const ellipse: EllipseAnnotation = { id: 'e', kind: 'ellipse', geometry: { x: 0.1, y: 0.2, width: 0.2, height: 0.4 }, style: { fill: '#0b8a84', stroke: '#112233', strokeWidthPx: 5 } };
    const e = shapePrimitive(ellipse, SIZE);
    expect(e).toMatchObject({ type: 'ellipse', cx: 200, cy: 200, rx: 100, ry: 100, fill: '#0b8a84', stroke: '#112233', strokeWidth: 5 });
    const d = shapePrimitive({ id: 'd', kind: 'diamond', geometry: { x: 0, y: 0, width: 0.2, height: 0.4 } }, SIZE);
    expect(d.type === 'polygon' && d.points).toEqual([
      { x: 100, y: 0 },
      { x: 200, y: 100 },
      { x: 100, y: 200 },
      { x: 0, y: 100 },
    ]);
    expect(d.fill).toBeNull();
    const l = shapePrimitive({ id: 'l', kind: 'line', geometry: { start: { x: 0.1, y: 0.2 }, end: { x: 0.3, y: 0.4 } } }, SIZE);
    expect(l).toMatchObject({ type: 'line', from: { x: 100, y: 100 }, to: { x: 300, y: 200 }, fill: null });
  });
});

describe('selectionBounds', () => {
  it('is null for an empty selection and the union otherwise', () => {
    expect(selectionBounds([], SIZE)).toBeNull();
    const b = selectionBounds([box('a', 'rectangle', { x: 0.1, y: 0.1, width: 0.1, height: 0.1 }), box('b', 'ellipse', { x: 0.5, y: 0.4, width: 0.2, height: 0.3 })], SIZE);
    expect(b?.x).toBeCloseTo(0.1);
    expect(b?.y).toBeCloseTo(0.1);
    expect((b?.x ?? 0) + (b?.width ?? 0)).toBeCloseTo(0.7);
    expect((b?.y ?? 0) + (b?.height ?? 0)).toBeCloseTo(0.7);
  });

  it('clamps pins that overhang the image edge and never yields NaN', () => {
    const b = selectionBounds([pin('p', 1, 0, 1), text('t', 0.5, 0.5, '')], SIZE, () => Number.NaN);
    expect(b).not.toBeNull();
    for (const v of Object.values(b ?? {})) expect(Number.isFinite(v)).toBe(true);
    expect(b?.x).toBeGreaterThanOrEqual(0);
    expect((b?.y ?? 0) + (b?.height ?? 0)).toBeLessThanOrEqual(1);
  });

  it('gives a zero-width box for a lone vertical line', () => {
    const b = selectionBounds([seg('l', 'line', { x: 0.4, y: 0.1 }, { x: 0.4, y: 0.9 })], SIZE);
    expect(b).toEqual({ x: 0.4, y: 0.1, width: 0, height: 0.8 });
  });
});

describe('moveSelection', () => {
  it('moves the group rigidly and clamps once so nothing shears at the edge', () => {
    const list = scene();
    const sel = ids('r', 'e', 'l', 'p');
    const moved = moveSelection(list, sel, 0.9, 0.9);
    expectNormalized(moved);
    // Every selected shape shifted by the same amount: pairwise offsets are unchanged.
    const origin = (a: Annotation): Point => allPoints(a)[0] ?? { x: 0, y: 0 };
    const shift = (id: string): Point => {
      const before = list.find((a) => a.id === id) as Annotation;
      const after = moved.find((a) => a.id === id) as Annotation;
      return { x: origin(after).x - origin(before).x, y: origin(after).y - origin(before).y };
    };
    for (const id of ['e', 'l', 'p']) {
      expect(shift(id).x).toBeCloseTo(shift('r').x, 5);
      expect(shift(id).y).toBeCloseTo(shift('r').y, 5);
    }
    // The pin (x=0.8) limits the horizontal move and the line (y=0.9) the vertical one.
    const pinAfter = moved.find((a) => a.id === 'p');
    const lineAfter = moved.find((a) => a.id === 'l');
    expect(pinAfter?.kind === 'pin' && pinAfter.geometry.position.x).toBeCloseTo(1, 5);
    expect(lineAfter?.kind === 'line' && lineAfter.geometry.end.y).toBeCloseTo(1, 5);
  });

  it('leaves unselected shapes as the same objects and keeps order and ids', () => {
    const list = scene();
    const moved = moveSelection(list, ids('r'), 0.05, 0.05);
    expect(moved.map((a) => a.id)).toEqual(list.map((a) => a.id));
    for (const [i, a] of list.entries()) if (a.id !== 'r') expect(moved[i]).toBe(a);
    expect(moved[0]).not.toBe(list[0]);
  });

  it('is a no-op for an empty selection, zero delta and non-finite delta', () => {
    const list = scene();
    expect(moveSelection(list, ids(), 0.1, 0.1)).toEqual(list);
    expect(moveSelection(list, ids('r'), 0, 0)).toEqual(list);
    expect(moveSelection(list, ids('r'), Number.NaN, Number.POSITIVE_INFINITY).every((a, i) => a.id === list[i]?.id)).toBe(true);
    expectNormalized(moveSelection(list, ids('r', 'l'), Number.NaN, Number.POSITIVE_INFINITY));
  });

  it('stays normalized after many repeated moves in both directions', () => {
    let list = scene();
    for (let i = 0; i < 40; i += 1) {
      list = moveSelection(list, ids('r', 'e', 'l', 'p', 't', 'd'), i % 2 === 0 ? 0.137 : -0.211, i % 3 === 0 ? 0.09 : -0.17);
      expectNormalized(list);
    }
    expect(validateAnnotations(toWire(list))).toEqual([]);
  });
});

describe('resizeSelection', () => {
  const list = scene();
  const sel = ids('r', 'e', 'l', 'p', 'd');
  const before = selectionBounds(list.filter((a) => sel.has(a.id)), SIZE, measure) as NormRect;

  it('maps the group affinely so relative positions scale with the box', () => {
    const after = { x: before.x, y: before.y, width: before.width / 2, height: before.height / 2 };
    const out = resizeSelection(list, sel, before, after, SIZE);
    expectNormalized(out);
    const r = out.find((a) => a.id === 'r');
    const e = out.find((a) => a.id === 'e');
    if (r?.kind !== 'rectangle' || e?.kind !== 'ellipse') throw new Error('kind changed');
    const r0 = list[0] as typeof r;
    const e0 = list[1] as typeof e;
    expect(r.geometry.width).toBeCloseTo(r0.geometry.width / 2, 5);
    expect(e.geometry.height).toBeCloseTo(e0.geometry.height / 2, 5);
    expect(e.geometry.x - r.geometry.x).toBeCloseTo((e0.geometry.x - r0.geometry.x) / 2, 5);
    // The same bounds come back out of the resized selection.
    const nowBounds = selectionBounds(out.filter((a) => sel.has(a.id)), SIZE, measure) as NormRect;
    expect(nowBounds.width).toBeCloseTo(after.width, 2);
  });

  it('leaves unselected shapes identical and keeps ids, kinds, order and pin numbers', () => {
    const out = resizeSelection(list, sel, before, { x: 0, y: 0, width: 0.9, height: 0.9 }, SIZE);
    expect(out.map((a) => `${a.id}:${a.kind}`)).toEqual(list.map((a) => `${a.id}:${a.kind}`));
    expect(out.find((a) => a.id === 't')).toBe(list.find((a) => a.id === 't'));
    const p = out.find((a) => a.id === 'p');
    expect(p?.kind === 'pin' && p.geometry.number).toBe(1);
  });

  it('bounds text and pin font sizes to 8..128 however extreme the resize', () => {
    const items = [text('t', 0.2, 0.2), pin('p', 1, 0.5, 0.5)];
    const b = selectionBounds(items, SIZE, measure) as NormRect;
    const tiny = resizeSelection(items, ids('t', 'p'), b, { x: b.x, y: b.y, width: 0.0001, height: 0.0001 }, SIZE);
    const huge = resizeSelection(items, ids('t', 'p'), b, { x: 0, y: 0, width: 1, height: 1 }, SIZE);
    for (const a of [...tiny, ...huge]) {
      const fs = a.style?.fontSizePx ?? 20;
      expect(fs).toBeGreaterThanOrEqual(8);
      expect(fs).toBeLessThanOrEqual(128);
    }
    expectNormalized(tiny);
    expectNormalized(huge);
  });

  it('never degenerates boxes below one source pixel', () => {
    const out = resizeSelection(list, sel, before, { x: 0.5, y: 0.5, width: 0, height: 0 }, SIZE);
    for (const a of out) {
      if (a.kind === 'rectangle' || a.kind === 'ellipse' || a.kind === 'diamond') {
        expect(a.geometry.width * SIZE.width).toBeGreaterThanOrEqual(0.99);
        expect(a.geometry.height * SIZE.height).toBeGreaterThanOrEqual(0.99);
      }
    }
    expect(validateAnnotations(toWire(out))).toEqual([]);
  });

  it('keeps a lone zero-extent line straight and finite', () => {
    const h = seg('h', 'line', { x: 0.2, y: 0.5 }, { x: 0.8, y: 0.5 });
    const b = selectionBounds([h], SIZE) as NormRect;
    expect(b.height).toBe(0);
    const out = resizeSelection([h], ids('h'), b, { x: 0.1, y: 0.4, width: 0.3, height: 0.2 }, SIZE);
    const g = out[0]?.kind === 'line' ? out[0].geometry : null;
    expect(g?.start.y).toBe(g?.end.y);
    expect(g?.end.x).toBeCloseTo(0.4, 5);
    expectNormalized(out);
  });

  it('stays normalized across repeated opposite resizes', () => {
    let cur = list;
    for (let i = 0; i < 30; i += 1) {
      const b = selectionBounds(cur.filter((a) => sel.has(a.id)), SIZE, measure) as NormRect;
      const grow = i % 2 === 0;
      cur = resizeSelection(cur, sel, b, grow ? { x: 0, y: 0, width: 1, height: 1 } : { x: 0.3, y: 0.3, width: 0.2, height: 0.2 }, SIZE);
      expectNormalized(cur);
    }
    expect(validateAnnotations(toWire(cur))).toEqual([]);
  });
});

describe('duplicateSelection', () => {
  it('copies with fresh unique ids, unused pin numbers and one common bounded offset', () => {
    const list = [pin('p1', 1, 0.3, 0.3), pin('p2', 2, 0.5, 0.5), box('r', 'rectangle', { x: 0.2, y: 0.2, width: 0.4, height: 0.4 })];
    const { annotations, selectedIds } = duplicateSelection(list, ids('p1', 'r'), SIZE);
    expect(annotations).toHaveLength(5);
    expect(new Set(annotations.map((a) => a.id)).size).toBe(5);
    expect(selectedIds).toHaveLength(2);
    expect(selectedIds.every((id) => !list.some((a) => a.id === id))).toBe(true);
    const pins = annotations.filter((a) => a.kind === 'pin').map((a) => (a.kind === 'pin' ? a.geometry.number : 0));
    expect(new Set(pins).size).toBe(3);
    expect(pins).toContain(3);
    expect(validateAnnotations(annotations)).toEqual([]);
    const copyP = annotations.find((a) => a.id === selectedIds[0]);
    const copyR = annotations.find((a) => a.id === selectedIds[1]);
    if (copyP?.kind !== 'pin' || copyR?.kind !== 'rectangle') throw new Error('unexpected copy kinds');
    const dxP = copyP.geometry.position.x - 0.3;
    expect(copyR.geometry.x - 0.2).toBeCloseTo(dxP, 5);
    expect(Math.abs(dxP) * SIZE.width).toBeLessThanOrEqual(16 + 1e-6);
    expect(dxP).not.toBe(0);
    // Originals are untouched and stay beneath the copies.
    expect(annotations.slice(0, 3)).toEqual(list);
  });

  it('mirrors the offset at the bottom-right edge instead of stacking exactly on top', () => {
    const list = [box('r', 'rectangle', { x: 0.9, y: 0.9, width: 0.1, height: 0.1 })];
    const { annotations } = duplicateSelection(list, ids('r'), SIZE);
    const copy = annotations[1];
    if (copy?.kind !== 'rectangle') throw new Error('kind changed');
    expect(copy.geometry.x).toBeLessThan(0.9);
    expect(copy.geometry.y).toBeLessThan(0.9);
    expectNormalized(annotations);
  });

  it('does not alias style objects of the originals', () => {
    const list = [box('r', 'rectangle', { x: 0.1, y: 0.1, width: 0.2, height: 0.2 }, { style: { stroke: '#e5362b' } })];
    const { annotations } = duplicateSelection(list, ids('r'), SIZE);
    expect(annotations[1]?.style).toEqual(list[0]?.style);
    expect(annotations[1]?.style).not.toBe(list[0]?.style);
  });

  it('duplicates past 100 annotations without any cap', () => {
    const list = Array.from({ length: 150 }, (_, i) => box(`r${i}`, 'rectangle', { x: 0.1, y: 0.1, width: 0.1, height: 0.1 }));
    const { annotations, selectedIds } = duplicateSelection(list, ids('r0', 'r1'), SIZE);
    expect(annotations).toHaveLength(152);
    expect(selectedIds).toHaveLength(2);
    expect(validateAnnotations(annotations)).toEqual([]);
  });

  it('keeps pin numbers unique when duplicating the last free numbers', () => {
    const list = Array.from({ length: 98 }, (_, i) => pin(`p${i}`, i + 1, 0.5, 0.5));
    const { annotations } = duplicateSelection(list, ids('p0', 'p1'), SIZE);
    const numbers = annotations.map((a) => (a.kind === 'pin' ? a.geometry.number : 0));
    expect(new Set(numbers).size).toBe(100);
    expect(Math.max(...numbers)).toBe(100);
    expect(validateAnnotations(annotations)).toEqual([]);
  });

  it('returns an empty selection for ids that are not in the list', () => {
    const { annotations, selectedIds } = duplicateSelection(scene(), ids('missing'), SIZE);
    expect(annotations).toHaveLength(scene().length);
    expect(selectedIds).toEqual([]);
  });
});

describe('alignSelection', () => {
  const trio = (): Annotation[] => [
    box('a', 'rectangle', { x: 0.1, y: 0.1, width: 0.1, height: 0.1 }),
    box('b', 'ellipse', { x: 0.4, y: 0.3, width: 0.3, height: 0.2 }),
    box('c', 'diamond', { x: 0.6, y: 0.6, width: 0.2, height: 0.1 }),
  ];
  const rectOf = (list: readonly Annotation[], id: string): NormRect => boundsOf(list.find((a) => a.id === id) as Annotation, SIZE);

  it('aligns edges and centers to the group bounds and leaves sizes alone', () => {
    const list = trio();
    const sel = ids('a', 'b', 'c');
    const left = alignSelection(list, sel, 'left', SIZE);
    for (const id of ['a', 'b', 'c']) expect(rectOf(left, id).x).toBeCloseTo(0.1, 5);
    const right = alignSelection(list, sel, 'right', SIZE);
    for (const id of ['a', 'b', 'c']) expect(rectOf(right, id).x + rectOf(right, id).width).toBeCloseTo(0.8, 5);
    const middle = alignSelection(list, sel, 'middle', SIZE);
    const center = (r: NormRect): number => r.y + r.height / 2;
    expect(center(rectOf(middle, 'a'))).toBeCloseTo(center(rectOf(middle, 'c')), 5);
    expect(rectOf(middle, 'b').width).toBeCloseTo(0.3, 5);
    const top = alignSelection(list, sel, 'top', SIZE);
    const bottom = alignSelection(list, sel, 'bottom', SIZE);
    for (const id of ['a', 'b', 'c']) {
      expect(rectOf(top, id).y).toBeCloseTo(0.1, 5);
      expect(rectOf(bottom, id).y + rectOf(bottom, id).height).toBeCloseTo(0.7, 5);
    }
    const hcenter = alignSelection(list, sel, 'center', SIZE);
    expect(rectOf(hcenter, 'a').x + rectOf(hcenter, 'a').width / 2).toBeCloseTo(rectOf(hcenter, 'c').x + rectOf(hcenter, 'c').width / 2, 5);
  });

  it('leaves unselected shapes untouched, and a single shape unchanged', () => {
    const list = [...trio(), text('t', 0.05, 0.9)];
    const out = alignSelection(list, ids('a', 'b'), 'right', SIZE, measure);
    expect(out.find((a) => a.id === 'c')).toBe(list[2]);
    expect(out.find((a) => a.id === 't')).toBe(list[3]);
    expect(alignSelection(list, ids('a'), 'left', SIZE)).toEqual(list);
  });

  it('aligns text by its measured box and stays finite', () => {
    const list = [box('a', 'rectangle', { x: 0.5, y: 0.1, width: 0.2, height: 0.1 }), text('t', 0.1, 0.5, 'abcdef')];
    const out = alignSelection(list, ids('a', 't'), 'right', SIZE, measure);
    const t = boundsOf(out[1] as Annotation, SIZE, measure);
    expect(t.x + t.width).toBeCloseTo(0.7, 5);
    expectNormalized(out);
    expectNormalized(alignSelection(list, ids('a', 't'), 'center', SIZE, () => Number.NaN));
  });
});

describe('reorderSelection', () => {
  const abc = (): Annotation[] => ['a', 'b', 'c', 'd', 'e'].map((id) => box(id, 'rectangle', { x: 0.1, y: 0.1, width: 0.1, height: 0.1 }));
  const order = (list: readonly Annotation[]): string => list.map((a) => a.id).join('');

  it('moves to front/back preserving relative order of both groups', () => {
    expect(order(reorderSelection(abc(), ids('b', 'd'), 'front'))).toBe('acebd');
    expect(order(reorderSelection(abc(), ids('b', 'd'), 'back'))).toBe('bdace');
  });

  it('steps forward/backward past unselected neighbours only', () => {
    expect(order(reorderSelection(abc(), ids('b', 'd'), 'forward'))).toBe('acbed');
    expect(order(reorderSelection(abc(), ids('b', 'd'), 'backward'))).toBe('badce');
    expect(order(reorderSelection(abc(), ids('a', 'b'), 'forward'))).toBe('cabde');
    expect(order(reorderSelection(abc(), ids('d', 'e'), 'forward'))).toBe('abcde');
    expect(order(reorderSelection(abc(), ids('a', 'b'), 'backward'))).toBe('abcde');
  });

  it('keeps every object and does not mutate the input', () => {
    const list = abc();
    const out = reorderSelection(list, ids('c'), 'front');
    expect(order(list)).toBe('abcde');
    expect([...out].sort((x, y) => x.id.localeCompare(y.id))).toEqual(list);
  });
});

describe('marqueeSelection', () => {
  it('selects shapes whose bounds intersect the marquee, in list order', () => {
    const list = scene();
    expect(marqueeSelection(list, { x: 0, y: 0, width: 0.4, height: 0.4 }, SIZE, measure)).toEqual(['r']);
    expect(marqueeSelection(list, { x: 0.45, y: 0.15, width: 0.4, height: 0.4 }, SIZE, measure)).toEqual(['e', 'p']);
  });

  it('accepts a marquee dragged backwards', () => {
    const list = scene();
    expect(marqueeSelection(list, { x: 0.4, y: 0.4, width: -0.4, height: -0.4 }, SIZE, measure)).toEqual(['r']);
  });

  it('selects an axis-aligned line (zero-height bounds) and nothing for a zero-area marquee', () => {
    const list = [seg('h', 'line', { x: 0.2, y: 0.5 }, { x: 0.8, y: 0.5 })];
    expect(marqueeSelection(list, { x: 0.4, y: 0.4, width: 0.1, height: 0.2 }, SIZE)).toEqual(['h']);
    expect(marqueeSelection(list, { x: 0.5, y: 0.5, width: 0, height: 0 }, SIZE)).toEqual([]);
    expect(marqueeSelection(list, { x: Number.NaN, y: 0, width: 1, height: 1 }, SIZE)).toEqual([]);
  });
});
