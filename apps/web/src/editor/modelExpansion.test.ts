import { describe, expect, it } from 'vitest';
import type { Annotation, ArrowAnnotation, NormRect, Point, TextStyle } from '@/api/types';
import {
  EFFECT_SIZE_DEFAULT_BLUR,
  arrowControlPoint,
  boundsOf,
  defaultArrowControl,
  defaultStyle,
  definingPoints,
  hitTest,
  moveBy,
  redactionsLast,
  requiresAdvancedSchema,
  toWire,
  translateAnnotation,
  validateAnnotations,
} from './model';

const SIZE = { width: 1000, height: 500 };

function curved(start: Point, end: Point, control?: Point): ArrowAnnotation {
  return { id: 'a', kind: 'arrow', geometry: { start, end, ...(control ? { control } : {}) }, style: { arrowStyle: 'curved' } };
}

function boxOf(id: string, kind: 'highlight' | 'spotlight' | 'blur' | 'pixelate' | 'rectangle', g: NormRect, style?: Annotation['style']): Annotation {
  return { id, kind, geometry: g, ...(style ? { style } : {}) } as Annotation;
}

describe('defaultArrowControl', () => {
  it('pushes the chord midpoint sideways by 30% of the chord', () => {
    expect(defaultArrowControl({ x: 0.2, y: 0.5 }, { x: 0.6, y: 0.5 })).toEqual({ x: 0.4, y: 0.62 });
  });

  it('flips to the other side when the first would leave the image', () => {
    expect(defaultArrowControl({ x: 0.2, y: 0.95 }, { x: 0.6, y: 0.95 })).toEqual({ x: 0.4, y: 0.83 });
  });

  it('degenerates to the start for a zero-length arrow and always yields a valid wire point', () => {
    expect(defaultArrowControl({ x: 0.3, y: 0.3 }, { x: 0.3, y: 0.3 })).toEqual({ x: 0.3, y: 0.3 });
    for (let i = 0; i <= 10; i += 1) {
      for (let j = 0; j <= 10; j += 1) {
        const c = defaultArrowControl({ x: i / 10, y: j / 10 }, { x: 1 - i / 10, y: 1 - (j * j) / 100 });
        expect(c.x).toBeGreaterThanOrEqual(0);
        expect(c.x).toBeLessThanOrEqual(1);
        expect(c.y).toBeGreaterThanOrEqual(0);
        expect(c.y).toBeLessThanOrEqual(1);
      }
    }
  });
});

describe('arrowControlPoint', () => {
  const s = { x: 0.2, y: 0.5 };
  const e = { x: 0.6, y: 0.5 };

  it('is null unless the arrow is curved, so a retained control is ignored', () => {
    expect(arrowControlPoint({ ...curved(s, e, { x: 0.4, y: 0.9 }), style: { arrowStyle: 'open' } })).toBeNull();
    expect(arrowControlPoint({ id: 'a', kind: 'arrow', geometry: { start: s, end: e, control: { x: 0.4, y: 0.9 } } })).toBeNull();
  });

  it('is the stored control, else the default', () => {
    expect(arrowControlPoint(curved(s, e, { x: 0.4, y: 0.9 }))).toEqual({ x: 0.4, y: 0.9 });
    expect(arrowControlPoint(curved(s, e))).toEqual(defaultArrowControl(s, e));
  });
});

describe('curved arrow geometry', () => {
  const s = { x: 0.2, y: 0.5 };
  const e = { x: 0.6, y: 0.5 };

  it('bounds include the control point so the handle sits inside the selection box', () => {
    const b = boundsOf(curved(s, e, { x: 0.4, y: 0.9 }), SIZE);
    expect(b.x).toBeCloseTo(0.2, 9);
    expect(b.y).toBeCloseTo(0.5, 9);
    expect(b.width).toBeCloseTo(0.4, 9);
    expect(b.height).toBeCloseTo(0.4, 9);
    const straight = boundsOf({ ...curved(s, e, { x: 0.4, y: 0.9 }), style: {} }, SIZE);
    expect(straight.height).toBeCloseTo(0, 9);
  });

  it('moves are clamped by the control point too', () => {
    const moved = moveBy(curved(s, e, { x: 0.4, y: 0.95 }), 0, 0.2);
    if (moved.kind !== 'arrow') throw new Error('arrow expected');
    expect(moved.geometry.start.y).toBeCloseTo(0.55, 6);
    expect(moved.geometry.control?.y).toBeCloseTo(1, 6);
    const left = moveBy(curved(s, e, { x: 0.1, y: 0.7 }), -0.5, 0);
    if (left.kind !== 'arrow') throw new Error('arrow expected');
    expect(left.geometry.control?.x).toBeCloseTo(0, 6);
    expect(left.geometry.start.x).toBeCloseTo(0.1, 6);
  });

  it('a retained control on a straight arrow follows the move but never leaves the image or blocks the move', () => {
    const straight: Annotation = { id: 'a', kind: 'arrow', geometry: { start: s, end: e, control: { x: 0.4, y: 0.95 } } };
    expect(definingPoints(straight)).toEqual([s, e]);
    const moved = moveBy(straight, 0, 0.2);
    if (moved.kind !== 'arrow') throw new Error('arrow expected');
    expect(moved.geometry.start.y).toBeCloseTo(0.7, 6);
    expect(moved.geometry.control).toEqual({ x: 0.4, y: 1 });
  });

  it('translating keeps the control relative to the endpoints', () => {
    const t = translateAnnotation(curved(s, e, { x: 0.4, y: 0.9 }), 0.1, -0.1);
    if (t.kind !== 'arrow') throw new Error('arrow expected');
    expect(t.geometry.control).toEqual({ x: 0.5, y: 0.8 });
    const plain = translateAnnotation({ id: 'l', kind: 'line', geometry: { start: s, end: e } }, 0.1, 0);
    expect('control' in plain.geometry).toBe(false);
  });

  it('hit tests follow the curve, not the chord', () => {
    const a = curved({ x: 0.1, y: 0.5 }, { x: 0.9, y: 0.5 }, { x: 0.5, y: 0.9 });
    // The quadratic peaks at (0.5, 0.7) halfway along.
    expect(hitTest([a], { x: 0.5, y: 0.7 }, SIZE, 3)).toBe('a');
    expect(hitTest([a], { x: 0.5, y: 0.5 }, SIZE, 3)).toBeNull();
    const straight: Annotation = { id: 'a', kind: 'arrow', geometry: a.geometry };
    expect(hitTest([straight], { x: 0.5, y: 0.5 }, SIZE, 3)).toBe('a');
  });
});

describe('smoothed freehand', () => {
  const points = [
    { x: 0.1, y: 0.5 },
    { x: 0.5, y: 0.9 },
    { x: 0.9, y: 0.5 },
  ];

  it('is hit where the smooth curve is, and legacy strokes keep the raw polyline', () => {
    // Curve point at t=0.5 of the first quad: (0.45, 0.75); the raw polyline is about 45px away there.
    const smooth: Annotation = { id: 'f', kind: 'freehand', geometry: { points }, style: { smoothing: true } };
    const legacy: Annotation = { id: 'f', kind: 'freehand', geometry: { points } };
    expect(hitTest([smooth], { x: 0.45, y: 0.75 }, SIZE, 2)).toBe('f');
    expect(hitTest([legacy], { x: 0.45, y: 0.75 }, SIZE, 2)).toBeNull();
    // The raw box contains the curve, so bounds are identical either way.
    expect(boundsOf(smooth, SIZE)).toEqual(boundsOf(legacy, SIZE));
  });
});

describe('new rectangle kinds', () => {
  const g = { x: 0.2, y: 0.2, width: 0.3, height: 0.3 };

  it.each(['highlight', 'spotlight', 'blur', 'pixelate'] as const)('%s is a rectangle for bounds, hit tests, moves and validation', (kind) => {
    const a = boxOf('b', kind, g);
    expect(boundsOf(a, SIZE)).toEqual(g);
    expect(definingPoints(a)).toEqual([
      { x: 0.2, y: 0.2 },
      { x: 0.5, y: 0.2 + 0.3 },
    ]);
    expect(hitTest([a], { x: 0.3, y: 0.3 }, SIZE, 2)).toBe('b');
    expect(hitTest([a], { x: 0.6, y: 0.3 }, SIZE, 2)).toBeNull();
    const edge = moveBy(boxOf('b', kind, { x: 0.8, y: 0.8, width: 0.2, height: 0.2 }), 0.5, 0.5);
    expect(edge.kind).toBe(kind);
    expect(boundsOf(edge, SIZE)).toEqual({ x: 0.8, y: 0.8, width: 0.2, height: 0.2 });
    expect(validateAnnotations([a])).toEqual([]);
    expect(validateAnnotations([boxOf('z', kind, { ...g, width: 0 })]).map((i) => i.code)).toEqual(['bounds']);
    expect(validateAnnotations([boxOf('o', kind, { ...g, x: 0.9 })]).map((i) => i.code)).toEqual(['bounds']);
  });

  it('paints effects first, then the spotlight, then everything else, with redactions last', () => {
    const redact: Annotation = { id: 'x', kind: 'redact', mode: 'solid', geometry: g };
    const list = [boxOf('r', 'rectangle', g), redact, boxOf('b1', 'blur', g), boxOf('s', 'spotlight', g), boxOf('h', 'highlight', g), boxOf('p', 'pixelate', g), boxOf('b2', 'blur', g)];
    expect(redactionsLast(list).map((a) => a.id)).toEqual(['b1', 'p', 'b2', 's', 'r', 'h', 'x']);
  });

  it('hit tests use paint order, so a shape drawn over a blur is picked first even if the blur is later in the list', () => {
    const arrow: Annotation = { id: 'arrow', kind: 'arrow', geometry: { start: { x: 0.2, y: 0.5 }, end: { x: 0.8, y: 0.5 } } };
    const blur = boxOf('blur', 'blur', { x: 0.3, y: 0.3, width: 0.4, height: 0.4 });
    expect(hitTest(redactionsLast([arrow, blur]), { x: 0.5, y: 0.5 }, SIZE, 4)).toBe('arrow');
    expect(hitTest(redactionsLast([arrow, blur]), { x: 0.5, y: 0.35 }, SIZE, 4)).toBe('blur');
  });
});

describe('text style bounds', () => {
  function text(textStyle?: TextStyle): Annotation {
    return { id: 't', kind: 'text', geometry: { position: { x: 0.1, y: 0.2 }, text: 'hello' }, style: { fontSizePx: 20, ...(textStyle ? { textStyle } : {}) } };
  }

  it('measures with the style of the text', () => {
    const seen: (TextStyle | undefined)[] = [];
    boundsOf(text('mono'), SIZE, (_t, _fs, style) => {
      seen.push(style);
      return 100;
    });
    boundsOf(text(), SIZE, (_t, _fs, style) => {
      seen.push(style);
      return 100;
    });
    expect(seen).toEqual(['mono', undefined]);
  });

  it('the label backdrop padding is part of the bounds, other styles are unchanged', () => {
    const measure = (): number => 100;
    expect(boundsOf(text('plain'), SIZE, measure)).toEqual({ x: 0.1, y: 0.2, width: 0.1, height: 25 / 500 });
    const label = boundsOf(text('label'), SIZE, measure);
    expect(label.x).toBe(0.1);
    expect(label.y).toBe(0.2);
    expect(label.width).toBeCloseTo((100 + 18) / 1000, 9);
    expect(label.height).toBeCloseTo((25 + 8.8) / 500, 9);
  });
});

describe('toWire for the expanded kinds', () => {
  const g = { x: 0.2, y: 0.2, width: 0.3, height: 0.3 };

  it('writes no style for a spotlight and keeps redactions style-free', () => {
    const [spot] = toWire([boxOf('s', 'spotlight', g, { stroke: '#112233', opacity: 0.5 })]);
    expect(spot).toEqual({ id: 's', kind: 'spotlight', geometry: g, comment: '' });
    expect('style' in (spot as object)).toBe(false);
    const [red] = toWire([{ id: 'r', kind: 'redact', mode: 'solid', geometry: g, style: { stroke: '#ffffff' } }]);
    expect('style' in (red as object)).toBe(false);
  });

  it('materializes highlight color and opacity with defaults, clamps and cleans them', () => {
    expect(toWire([boxOf('h', 'highlight', g)])[0]?.style).toEqual({ stroke: '#f2a900', opacity: 0.35 });
    expect(toWire([boxOf('h', 'highlight', g, { stroke: 'red', opacity: 0.333333 })])[0]?.style).toEqual({ stroke: '#f2a900', opacity: 0.33 });
    expect(toWire([boxOf('h', 'highlight', g, { stroke: '#112233', opacity: 5, effectSizePx: 9, fill: '#ffffff' })])[0]?.style).toEqual({ stroke: '#112233', opacity: 0.8 });
  });

  it('writes only an integer effect size for blur and pixelate, defaulting per kind', () => {
    expect(toWire([boxOf('b', 'blur', g)])[0]?.style).toEqual({ effectSizePx: EFFECT_SIZE_DEFAULT_BLUR });
    expect(toWire([boxOf('p', 'pixelate', g)])[0]?.style).toEqual({ effectSizePx: 16 });
    expect(toWire([boxOf('b', 'blur', g, { effectSizePx: 100, stroke: '#112233' })])[0]?.style).toEqual({ effectSizePx: 64 });
    expect(toWire([boxOf('b', 'blur', g, { effectSizePx: 7.6 })])[0]?.style).toEqual({ effectSizePx: 8 });
    expect(toWire([boxOf('b', 'blur', g, { effectSizePx: 0 })])[0]?.style).toEqual({ effectSizePx: 2 });
  });

  it('never leaks a new style field onto a kind it does not belong to', () => {
    const wrong = { stroke: '#112233', smoothing: true, opacity: 0.5, effectSizePx: 5, arrowStyle: 'open', textStyle: 'mono' } as const;
    expect(toWire([boxOf('r', 'rectangle', g, wrong)])[0]?.style).toEqual({ stroke: '#112233' });
    const pin: Annotation = { id: 'p', kind: 'pin', geometry: { position: { x: 0.5, y: 0.5 }, number: 1 }, style: wrong };
    expect(toWire([pin])[0]?.style).toEqual({ stroke: '#112233' });
  });

  it('arrow: strips the default style, keeps the others, materializes the curve control, retains a straight control', () => {
    const base = { start: { x: 0.2, y: 0.5 }, end: { x: 0.6, y: 0.5 } };
    const filled: Annotation = { id: 'a', kind: 'arrow', geometry: base, style: { arrowStyle: 'filled', strokeWidthPx: 3 } };
    expect(toWire([filled])[0]).toEqual({ id: 'a', kind: 'arrow', geometry: base, comment: '', style: { strokeWidthPx: 3 } });
    const open = toWire([{ ...filled, style: { arrowStyle: 'open' } }])[0];
    expect(open?.style).toEqual({ arrowStyle: 'open' });
    const c = toWire([curved(base.start, base.end)])[0];
    expect(c?.kind === 'arrow' && c.geometry.control).toEqual(defaultArrowControl(base.start, base.end));
    expect(c?.style).toEqual({ arrowStyle: 'curved' });
    const retained = toWire([{ ...filled, geometry: { ...base, control: { x: 0.30000004, y: 0.1 } } }])[0];
    expect(retained?.kind === 'arrow' && retained.geometry.control).toEqual({ x: 0.3, y: 0.1 });
    expect(retained?.style).toEqual({ strokeWidthPx: 3 });
  });

  it('text and freehand: new styles only when they differ from the legacy default', () => {
    const t: Annotation = { id: 't', kind: 'text', geometry: { position: { x: 0.1, y: 0.1 }, text: 'x' } };
    expect(toWire([{ ...t, style: { textStyle: 'outline' } }])[0]?.style).toEqual({});
    expect(toWire([{ ...t, style: { textStyle: 'mono' } }])[0]?.style).toEqual({ textStyle: 'mono' });
    expect(toWire([{ ...t, style: { textStyle: 'sparkle' as TextStyle } }])[0]?.style).toEqual({});
    const f: Annotation = { id: 'f', kind: 'freehand', geometry: { points: [{ x: 0, y: 0 }, { x: 1, y: 1 }] } };
    expect(toWire([f])[0]?.style).toEqual({});
    expect(toWire([{ ...f, style: { smoothing: false } }])[0]?.style).toEqual({});
    expect(toWire([{ ...f, style: { smoothing: true } }])[0]?.style).toEqual({ smoothing: true });
  });

  it('new strokes default to smoothing, new effects to their default size', () => {
    expect(defaultStyle('freehand', SIZE).smoothing).toBe(true);
    expect(defaultStyle('blur', SIZE)).toEqual({ effectSizePx: 12 });
    expect(defaultStyle('pixelate', SIZE)).toEqual({ effectSizePx: 16 });
    expect(defaultStyle('highlight', SIZE)).toEqual({ stroke: '#f2a900', opacity: 0.35 });
    expect(defaultStyle('spotlight', SIZE)).toEqual({});
  });
});

describe('requiresAdvancedSchema', () => {
  const g = { x: 0.2, y: 0.2, width: 0.3, height: 0.3 };
  const line = { start: { x: 0.2, y: 0.5 }, end: { x: 0.6, y: 0.5 } };

  it('stays false for documents that use nothing newer than schema 0.2', () => {
    const legacy: Annotation[] = [
      boxOf('r', 'rectangle', g, { stroke: '#112233' }),
      { id: 'a', kind: 'arrow', geometry: line, style: { arrowStyle: 'filled' } },
      { id: 't', kind: 'text', geometry: { position: { x: 0.1, y: 0.1 }, text: 'x' }, style: { textStyle: 'outline' } },
      { id: 'f', kind: 'freehand', geometry: { points: [{ x: 0, y: 0 }, { x: 1, y: 1 }] }, style: { smoothing: false } },
      { id: 'p', kind: 'pin', geometry: { position: { x: 0.5, y: 0.5 }, number: 1 } },
      { id: 'x', kind: 'redact', mode: 'solid', geometry: g },
    ];
    expect(requiresAdvancedSchema(legacy)).toBe(false);
    expect(requiresAdvancedSchema([])).toBe(false);
  });

  it.each<[string, Annotation]>([
    ['highlight', boxOf('h', 'highlight', g)],
    ['spotlight', boxOf('s', 'spotlight', g)],
    ['blur', boxOf('b', 'blur', g)],
    ['pixelate', boxOf('p', 'pixelate', g)],
    ['curved arrow', curved(line.start, line.end)],
    ['open arrow', { id: 'a', kind: 'arrow', geometry: line, style: { arrowStyle: 'open' } }],
    ['double arrow', { id: 'a', kind: 'arrow', geometry: line, style: { arrowStyle: 'double' } }],
    ['retained control', { id: 'a', kind: 'arrow', geometry: { ...line, control: { x: 0.4, y: 0.4 } } }],
    ['label text', { id: 't', kind: 'text', geometry: { position: { x: 0.1, y: 0.1 }, text: 'x' }, style: { textStyle: 'label' } }],
    ['smoothed freehand', { id: 'f', kind: 'freehand', geometry: { points: [{ x: 0, y: 0 }, { x: 1, y: 1 }] }, style: { smoothing: true } }],
  ])('is true for %s', (_name, a) => {
    expect(requiresAdvancedSchema([a])).toBe(true);
  });
});

describe('validateAnnotations for curved arrows', () => {
  it('flags a control point outside the image', () => {
    const a = curved({ x: 0.2, y: 0.5 }, { x: 0.6, y: 0.5 }, { x: 1.2, y: 0.5 });
    expect(validateAnnotations([a]).map((i) => i.code)).toEqual(['bounds']);
    expect(validateAnnotations([curved({ x: 0.2, y: 0.5 }, { x: 0.6, y: 0.5 }, { x: 1, y: 0 })])).toEqual([]);
  });
});
