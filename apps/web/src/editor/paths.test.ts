import { describe, expect, it } from 'vitest';
import { flattenPath, flattenQuad, pathData, quadPoint, roundedRectPath, smoothPath, tracePath, trimQuad, type PathCmd } from './paths';

const dist = (a: { x: number; y: number }, b: { x: number; y: number }): number => Math.hypot(a.x - b.x, a.y - b.y);

describe('pathData / tracePath', () => {
  const cmds: PathCmd[] = [
    { op: 'M', x: 1, y: 2 },
    { op: 'L', x: 3, y: 4 },
    { op: 'Q', cx: 5, cy: 6, x: 7, y: 8 },
    { op: 'Z' },
  ];

  it('serializes to an SVG path', () => {
    expect(pathData(cmds)).toBe('M1 2L3 4Q5 6 7 8Z');
  });

  it('plays the same commands onto a canvas-like sink', () => {
    const calls: string[] = [];
    tracePath(
      {
        moveTo: (x, y) => calls.push(`M${x} ${y}`),
        lineTo: (x, y) => calls.push(`L${x} ${y}`),
        quadraticCurveTo: (cx, cy, x, y) => calls.push(`Q${cx} ${cy} ${x} ${y}`),
        closePath: () => calls.push('Z'),
      },
      cmds,
    );
    expect(calls.join('')).toBe(pathData(cmds));
  });
});

describe('smoothPath', () => {
  const p = (x: number, y: number): { x: number; y: number } => ({ x, y });

  it('returns nothing for no points and a lone move for one point', () => {
    expect(smoothPath([])).toEqual([]);
    expect(smoothPath([p(1, 1)])).toEqual([{ op: 'M', x: 1, y: 1 }]);
  });

  it('keeps two points a straight segment', () => {
    expect(smoothPath([p(0, 0), p(10, 10)])).toEqual([
      { op: 'M', x: 0, y: 0 },
      { op: 'L', x: 10, y: 10 },
    ]);
  });

  it('passes raw points as controls through the midpoints and ends exactly on the first and last raw point', () => {
    expect(smoothPath([p(0, 0), p(10, 0), p(10, 10), p(20, 10)])).toEqual([
      { op: 'M', x: 0, y: 0 },
      { op: 'Q', cx: 10, cy: 0, x: 10, y: 5 },
      { op: 'Q', cx: 10, cy: 10, x: 15, y: 10 },
      { op: 'L', x: 20, y: 10 },
    ]);
  });
});

describe('flattenPath', () => {
  it('includes the endpoints and samples quadratics', () => {
    const pts = flattenPath(smoothPath([{ x: 0, y: 0 }, { x: 10, y: 20 }, { x: 20, y: 0 }]), 4);
    expect(pts[0]).toEqual({ x: 0, y: 0 });
    expect(pts[pts.length - 1]).toEqual({ x: 20, y: 0 });
    // First quad: control (10,20) to midpoint (15,10): t=0.5 is (0.25*0 + 0.5*10 + 0.25*15, 0.5*20 + 0.25*10).
    expect(pts[2]).toEqual({ x: 0.25 * 0 + 0.5 * 10 + 0.25 * 15, y: 0.5 * 20 + 0.25 * 10 });
  });
});

describe('quadratics', () => {
  const s = { x: 0, y: 0 };
  const c = { x: 50, y: 100 };
  const e = { x: 100, y: 0 };

  it('hits the endpoints and the analytic midpoint', () => {
    expect(quadPoint(s, c, e, 0)).toEqual(s);
    expect(quadPoint(s, c, e, 1)).toEqual(e);
    expect(quadPoint(s, c, e, 0.5)).toEqual({ x: 50, y: 50 });
    expect(flattenQuad(s, c, e, 8)).toHaveLength(9);
  });

  it('trims by straight-line distance from either end and keeps the curve tangent', () => {
    const t = trimQuad(s, c, e, 15, 20);
    expect(t).not.toBeNull();
    if (!t) return;
    expect(dist(t.s, s)).toBeCloseTo(15, 4);
    expect(dist(t.e, e)).toBeCloseTo(20, 4);
    // The trimmed curve is part of the original: its midpoint lies on the original curve.
    const mid = quadPoint(t.s, t.c, t.e, 0.5);
    const near = flattenQuad(s, c, e, 2000).reduce((m, q) => Math.min(m, dist(q, mid)), Infinity);
    expect(near).toBeLessThan(0.2);
  });

  it('does not trim when no distance is requested', () => {
    const t = trimQuad(s, c, e, 0, 0);
    expect(t?.s).toEqual(s);
    expect(t?.e).toEqual(e);
  });

  it('returns null when nothing would be left', () => {
    expect(trimQuad(s, c, e, 90, 90)).toBeNull();
    expect(trimQuad(s, c, e, 0, 500)).toBeNull();
  });
});

describe('roundedRectPath', () => {
  it('stays inside its box and clamps the radius to half the short side', () => {
    const cmds = roundedRectPath(10, 20, 30, 8, 100);
    const xs = cmds.flatMap((c) => (c.op === 'Z' ? [] : [c.x]));
    const ys = cmds.flatMap((c) => (c.op === 'Z' ? [] : [c.y]));
    expect(Math.min(...xs)).toBe(10);
    expect(Math.max(...xs)).toBe(40);
    expect(Math.min(...ys)).toBe(20);
    expect(Math.max(...ys)).toBe(28);
    // Radius 4 (half of the 8px height): the first move starts 4px in.
    expect(cmds[0]).toEqual({ op: 'M', x: 14, y: 20 });
    expect(cmds[cmds.length - 1]).toEqual({ op: 'Z' });
  });
});
