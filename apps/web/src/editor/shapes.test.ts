import { describe, expect, it } from 'vitest';
import type { ArrowAnnotation, ArrowStyle, FreehandAnnotation, HighlightAnnotation, TextAnnotation, TextStyle } from '@/api/types';
import { defaultArrowControl } from './model';
import { FONT_STACK, MONO_STACK, arrowPrimitive, freehandPath, highlightPrimitive, textFontFamily, textFontWeight, textParts } from './shapes';

const SIZE = { width: 1000, height: 500 };
const HEAD_BASE = 18 * Math.cos(Math.PI / 7); // head length for a 4px stroke times cos(spread)

function arrow(arrowStyle: ArrowStyle | undefined, geometry: ArrowAnnotation['geometry']): ArrowAnnotation {
  return { id: 'a', kind: 'arrow', geometry, style: { stroke: '#e5362b', strokeWidthPx: 4, ...(arrowStyle ? { arrowStyle } : {}) } };
}

const H = { start: { x: 0.1, y: 0.5 }, end: { x: 0.5, y: 0.5 } };

describe('arrowPrimitive', () => {
  it('keeps the original filled arrow: shaft stops at the head base, one filled head', () => {
    const p = arrowPrimitive(arrow(undefined, H), SIZE);
    expect(p.filledHeads).toHaveLength(1);
    expect(p.openHeads).toHaveLength(0);
    expect(p.shaft[0]).toEqual({ op: 'M', x: 100, y: 250 });
    const end = p.shaft[1];
    expect(end?.op).toBe('L');
    if (end?.op === 'L') {
      expect(end.x).toBeCloseTo(500 - HEAD_BASE, 6);
      expect(end.y).toBeCloseTo(250, 6);
    }
    expect(p.filledHeads[0]?.[0]).toEqual({ op: 'M', x: 500, y: 250 });
  });

  it('collapses the shaft of a very short filled arrow onto its start instead of poking past the head', () => {
    const p = arrowPrimitive(arrow('filled', { start: { x: 0.1, y: 0.5 }, end: { x: 0.105, y: 0.5 } }), SIZE);
    expect(p.shaft).toEqual([
      { op: 'M', x: 100, y: 250 },
      { op: 'L', x: 100, y: 250 },
    ]);
  });

  it('draws filled heads at both ends for double and trims the shaft at both bases', () => {
    const p = arrowPrimitive(arrow('double', H), SIZE);
    expect(p.filledHeads).toHaveLength(2);
    expect(p.openHeads).toHaveLength(0);
    const [from, to] = p.shaft;
    expect(from?.op === 'M' && from.x).toBeCloseTo(100 + HEAD_BASE, 6);
    expect(to?.op === 'L' && to.x).toBeCloseTo(500 - HEAD_BASE, 6);
  });

  it('draws no shaft for a double arrow shorter than its two heads', () => {
    expect(arrowPrimitive(arrow('double', { start: { x: 0.1, y: 0.5 }, end: { x: 0.12, y: 0.5 } }), SIZE).shaft).toEqual([]);
  });

  it('uses an open chevron whose shaft runs to the tip', () => {
    const p = arrowPrimitive(arrow('open', H), SIZE);
    expect(p.filledHeads).toHaveLength(0);
    expect(p.openHeads).toHaveLength(1);
    expect(p.shaft).toEqual([
      { op: 'M', x: 100, y: 250 },
      { op: 'L', x: 500, y: 250 },
    ]);
    const chevron = p.openHeads[0];
    expect(chevron?.map((c) => c.op)).toEqual(['M', 'L', 'L']);
    expect(chevron?.[1]).toEqual({ op: 'L', x: 500, y: 250 });
  });

  it('follows the control point for curved arrows and points the head along the curve tangent', () => {
    const geometry = { ...H, control: { x: 0.3, y: 0.1 } };
    const p = arrowPrimitive(arrow('curved', geometry), SIZE);
    expect(p.shaft[0]).toEqual({ op: 'M', x: 100, y: 250 });
    const q = p.shaft[1];
    expect(q?.op).toBe('Q');
    if (q?.op === 'Q') expect(Math.hypot(q.x - 500, q.y - 250)).toBeCloseTo(HEAD_BASE, 3);
    const head = p.filledHeads[0];
    const l = head?.[1];
    const r = head?.[2];
    if (l?.op === 'L' && r?.op === 'L') {
      const base = { x: (l.x + r.x) / 2, y: (l.y + r.y) / 2 };
      // Tangent at the end of a quadratic points from the control (300,50) to the end (500,250): 45 degrees.
      expect(Math.atan2(250 - base.y, 500 - base.x)).toBeCloseTo(Math.PI / 4, 6);
    } else {
      throw new Error('head triangle expected');
    }
  });

  it('draws a curve for a curved arrow without a stored control, using the default control', () => {
    const p = arrowPrimitive(arrow('curved', H), SIZE);
    const q = p.shaft[1];
    expect(q?.op).toBe('Q');
    const expected = defaultArrowControl(H.start, H.end);
    // The default pushes the middle off the straight chord (y=250).
    expect(expected.y).not.toBe(0.5);
    if (q?.op === 'Q') expect(q.cy).not.toBeCloseTo(250, 3);
  });

  it('ignores a control retained on an arrow that is not curved', () => {
    const p = arrowPrimitive(arrow('filled', { ...H, control: { x: 0.3, y: 0.1 } }), SIZE);
    expect(p.shaft[1]?.op).toBe('L');
  });

  it('starts a curved arrow exactly at its start point and gives it a single head', () => {
    const p = arrowPrimitive(arrow('curved', { ...H, control: { x: 0.3, y: 0.9 } }), SIZE);
    expect(p.shaft[0]).toEqual({ op: 'M', x: 100, y: 250 });
    expect(p.filledHeads).toHaveLength(1);
    expect(p.openHeads).toHaveLength(0);
  });
});

describe('highlightPrimitive', () => {
  const base: HighlightAnnotation = { id: 'h', kind: 'highlight', geometry: { x: 0.1, y: 0.2, width: 0.3, height: 0.4 } };

  it('defaults to the amber 35% alpha rectangle in image pixels', () => {
    expect(highlightPrimitive(base, SIZE)).toEqual({ x: 100, y: 100, width: 300, height: 200, fill: '#f2a900', opacity: 0.35 });
  });

  it('uses the stroke color and clamps opacity to 0.1..0.8', () => {
    expect(highlightPrimitive({ ...base, style: { stroke: '#112233', opacity: 0.95 } }, SIZE)).toMatchObject({ fill: '#112233', opacity: 0.8 });
    expect(highlightPrimitive({ ...base, style: { opacity: 0.01 } }, SIZE).opacity).toBe(0.1);
  });
});

describe('freehandPath', () => {
  const points = [
    { x: 0.1, y: 0.5 },
    { x: 0.5, y: 0.9 },
    { x: 0.9, y: 0.5 },
  ];
  const legacy: FreehandAnnotation = { id: 'f', kind: 'freehand', geometry: { points } };

  it('stays a polyline for legacy strokes without the smoothing flag', () => {
    expect(freehandPath(legacy, SIZE).map((c) => c.op)).toEqual(['M', 'L', 'L']);
    expect(freehandPath({ ...legacy, style: { smoothing: false } }, SIZE).map((c) => c.op)).toEqual(['M', 'L', 'L']);
  });

  it('uses the smooth quadratic chain only when smoothing is explicitly true', () => {
    expect(freehandPath({ ...legacy, style: { smoothing: true } }, SIZE).map((c) => c.op)).toEqual(['M', 'Q', 'L']);
  });
});

describe('text styles', () => {
  const measure = (text: string): number => text.length * 10;

  function text(textStyle: TextStyle | undefined, body = 'ab\ncde'): TextAnnotation {
    return {
      id: 't',
      kind: 'text',
      geometry: { position: { x: 0.1, y: 0.2 }, text: body },
      style: { fontSizePx: 20, stroke: '#ffffff', ...(textStyle ? { textStyle } : {}) },
    };
  }

  it('keeps the original white-halo outline when no style is set', () => {
    const p = textParts(text(undefined), SIZE, measure);
    expect(p.halo?.color).toBe('#ffffff');
    expect(p.halo?.width).toBeCloseTo(20 / 6, 6);
    expect(p.shadow).toBeNull();
    expect(p.backdrop).toBeNull();
    expect(p.underlines).toEqual([]);
    expect(p.weight).toBe(400);
    expect(p.family).toBe(FONT_STACK);
    expect(p.x).toBe(100);
    expect(p.lines.map((l) => l.y)).toEqual([100 + 20 * 0.92, 100 + 20 * (0.92 + 1.25)]);
    expect(textParts(text('outline'), SIZE, measure)).toEqual(p);
  });

  it('plain has no halo', () => {
    expect(textParts(text('plain'), SIZE, measure).halo).toBeNull();
  });

  it('label puts the text on a rounded backdrop in the stroke color with contrast text', () => {
    const p = textParts(text('label'), SIZE, measure);
    // Padding 0.45em x / 0.22em y, widest line 30px, radius 0.25em.
    expect(p.x).toBeCloseTo(100 + 9, 6);
    expect(p.lines[0]?.y).toBeCloseTo(100 + 4.4 + 18.4, 6);
    expect(p.backdrop?.fill).toBe('#ffffff');
    expect(p.fill).toBe('#0b2540');
    expect(p.halo).toBeNull();
    const right = p.backdrop?.path[1];
    expect(right?.op === 'L' && right.x).toBeCloseTo(100 + (30 + 18) - 5, 6);
    const bottom = p.backdrop?.path[4];
    expect(bottom?.op === 'Q' && bottom.y).toBeCloseTo(100 + 2 * 25 + 8.8, 6);
  });

  it('shadow is a hard offset copy without a halo', () => {
    const p = textParts(text('shadow'), SIZE, measure);
    expect(p.halo).toBeNull();
    expect(p.shadow?.dx).toBeCloseTo(20 / 14, 6);
    expect(p.shadow?.dy).toBeCloseTo(20 / 14, 6);
    expect(p.shadow?.opacity).toBe(0.45);
  });

  it('mono and bold switch to the bundled mono face and the loaded 600 weight', () => {
    expect(textParts(text('mono'), SIZE, measure).family).toBe(MONO_STACK);
    expect(textParts(text('bold'), SIZE, measure).weight).toBe(600);
    expect(textFontFamily('mono')).toBe(MONO_STACK);
    expect(textFontFamily('bold')).toBe(FONT_STACK);
    expect(textFontWeight('bold')).toBe(600);
    expect(textFontWeight('mono')).toBe(400);
    expect(textFontWeight(undefined)).toBe(400);
  });

  it('underline draws one rule per non-empty line, as wide as the measured line', () => {
    const p = textParts(text('underline', 'ab\n\ncde'), SIZE, measure);
    expect(p.halo).not.toBeNull();
    expect(p.underlines).toHaveLength(2);
    const first = p.underlines[0];
    expect(first?.path[0]).toEqual({ op: 'M', x: 100, y: expect.closeTo(100 + 20 * 0.92 + 2.4, 6) });
    expect(first?.path[1]).toEqual({ op: 'L', x: 120, y: expect.closeTo(100 + 20 * 0.92 + 2.4, 6) });
    expect(first?.width).toBeCloseTo(20 / 14, 6);
    expect(p.underlines[1]?.path[1]).toEqual({ op: 'L', x: 130, y: expect.any(Number) });
  });
});
