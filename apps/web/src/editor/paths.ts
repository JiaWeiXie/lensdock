/**
 * Path primitives shared by the SVG editing layer, the PNG rasterizer and the
 * hit tests. Pure functions (no DOM): both renderers stroke exactly the same
 * command list, so a curve, a smoothed freehand stroke or a label backdrop
 * cannot drift between what people edit and what gets exported.
 */

export interface PxPoint {
  x: number;
  y: number;
}

export type PathCmd =
  | { op: 'M'; x: number; y: number }
  | { op: 'L'; x: number; y: number }
  | { op: 'Q'; cx: number; cy: number; x: number; y: number }
  | { op: 'Z' };

/** SVG path `d` attribute of a command list. */
export function pathData(cmds: readonly PathCmd[]): string {
  return cmds
    .map((c) => {
      switch (c.op) {
        case 'M':
        case 'L':
          return `${c.op}${c.x} ${c.y}`;
        case 'Q':
          return `Q${c.cx} ${c.cy} ${c.x} ${c.y}`;
        case 'Z':
          return 'Z';
      }
    })
    .join('');
}

/** The part of CanvasRenderingContext2D / Path2D that path playback needs (keeps this module DOM-free). */
export interface PathSink {
  moveTo(x: number, y: number): void;
  lineTo(x: number, y: number): void;
  quadraticCurveTo(cx: number, cy: number, x: number, y: number): void;
  closePath(): void;
}

/** Appends the commands to the sink's current path (the caller owns beginPath/stroke/fill). */
export function tracePath(sink: PathSink, cmds: readonly PathCmd[]): void {
  for (const c of cmds) {
    switch (c.op) {
      case 'M':
        sink.moveTo(c.x, c.y);
        break;
      case 'L':
        sink.lineTo(c.x, c.y);
        break;
      case 'Q':
        sink.quadraticCurveTo(c.cx, c.cy, c.x, c.y);
        break;
      case 'Z':
        sink.closePath();
        break;
    }
  }
}

// -------------------------------------------------------------- quadratics

export function quadPoint(s: PxPoint, c: PxPoint, e: PxPoint, t: number): PxPoint {
  const u = 1 - t;
  return { x: u * u * s.x + 2 * u * t * c.x + t * t * e.x, y: u * u * s.y + 2 * u * t * c.y + t * t * e.y };
}

/** Polyline approximation of a quadratic Bezier, endpoints included. */
export function flattenQuad(s: PxPoint, c: PxPoint, e: PxPoint, segments = 24): PxPoint[] {
  const out: PxPoint[] = [];
  for (let i = 0; i <= segments; i += 1) out.push(quadPoint(s, c, e, i / segments));
  return out;
}

const TRIM_ITERATIONS = 40;

/**
 * Sub-curve of a quadratic Bezier that skips `startDist` pixels (straight-line
 * distance from the start) and `endDist` pixels (from the end), used to stop a
 * shaft at the base of an arrow head. Null when nothing is left.
 */
export function trimQuad(s: PxPoint, c: PxPoint, e: PxPoint, startDist: number, endDist: number): { s: PxPoint; c: PxPoint; e: PxPoint } | null {
  let t0 = 0;
  if (startDist > 0) {
    if (Math.hypot(e.x - s.x, e.y - s.y) < startDist) return null;
    let lo = 0;
    let hi = 1;
    for (let i = 0; i < TRIM_ITERATIONS; i += 1) {
      const mid = (lo + hi) / 2;
      const p = quadPoint(s, c, e, mid);
      if (Math.hypot(p.x - s.x, p.y - s.y) < startDist) lo = mid;
      else hi = mid;
    }
    t0 = hi;
  }
  let t1 = 1;
  if (endDist > 0) {
    if (Math.hypot(e.x - s.x, e.y - s.y) < endDist) return null;
    let lo = 0;
    let hi = 1;
    for (let i = 0; i < TRIM_ITERATIONS; i += 1) {
      const mid = (lo + hi) / 2;
      const p = quadPoint(s, c, e, mid);
      if (Math.hypot(p.x - e.x, p.y - e.y) < endDist) hi = mid;
      else lo = mid;
    }
    t1 = lo;
  }
  if (t1 <= t0) return null;
  const a = quadPoint(s, c, e, t0);
  const b = quadPoint(s, c, e, t1);
  // Blossom of the quadratic at (t0, t1) gives the control point of the sub-curve.
  const wS = (1 - t0) * (1 - t1);
  const wC = t0 * (1 - t1) + t1 * (1 - t0);
  const wE = t0 * t1;
  return { s: a, c: { x: wS * s.x + wC * c.x + wE * e.x, y: wS * s.y + wC * c.y + wE * e.y }, e: b };
}

// -------------------------------------------------------------- freehand

/**
 * Smooth quadratic chain through raw stroke points: every raw point is a
 * control point and the curve passes through the midpoints between them, with
 * the first and last raw point as exact endpoints. Fewer than 3 points stay a
 * straight segment.
 */
export function smoothPath(points: readonly PxPoint[]): PathCmd[] {
  const first = points[0];
  if (!first) return [];
  const cmds: PathCmd[] = [{ op: 'M', x: first.x, y: first.y }];
  const last = points[points.length - 1];
  if (!last || points.length < 2) return cmds;
  if (points.length === 2) {
    cmds.push({ op: 'L', x: last.x, y: last.y });
    return cmds;
  }
  for (let i = 1; i < points.length - 1; i += 1) {
    const p = points[i];
    const n = points[i + 1];
    if (!p || !n) continue;
    cmds.push({ op: 'Q', cx: p.x, cy: p.y, x: (p.x + n.x) / 2, y: (p.y + n.y) / 2 });
  }
  cmds.push({ op: 'L', x: last.x, y: last.y });
  return cmds;
}

/** Plain polyline (the legacy, un-smoothed freehand stroke). */
export function polylinePath(points: readonly PxPoint[]): PathCmd[] {
  return points.map((p, i): PathCmd => (i === 0 ? { op: 'M', x: p.x, y: p.y } : { op: 'L', x: p.x, y: p.y }));
}

/** Polyline approximation of a command list for hit testing (quadratics are split into `quadSegments`). */
export function flattenPath(cmds: readonly PathCmd[], quadSegments = 6): PxPoint[] {
  const out: PxPoint[] = [];
  let cur: PxPoint | null = null;
  for (const c of cmds) {
    switch (c.op) {
      case 'M':
      case 'L':
        cur = { x: c.x, y: c.y };
        out.push(cur);
        break;
      case 'Q': {
        if (!cur) break;
        const ctrl = { x: c.cx, y: c.cy };
        const end = { x: c.x, y: c.y };
        for (let i = 1; i <= quadSegments; i += 1) out.push(quadPoint(cur, ctrl, end, i / quadSegments));
        cur = end;
        break;
      }
      case 'Z':
        break;
    }
  }
  return out;
}

// ---------------------------------------------------------------- boxes

/** Rounded rectangle with quadratic corners (identical in SVG and Canvas). */
export function roundedRectPath(x: number, y: number, w: number, h: number, radius: number): PathCmd[] {
  const r = Math.max(0, Math.min(radius, w / 2, h / 2));
  const right = x + w;
  const bottom = y + h;
  return [
    { op: 'M', x: x + r, y },
    { op: 'L', x: right - r, y },
    { op: 'Q', cx: right, cy: y, x: right, y: y + r },
    { op: 'L', x: right, y: bottom - r },
    { op: 'Q', cx: right, cy: bottom, x: right - r, y: bottom },
    { op: 'L', x: x + r, y: bottom },
    { op: 'Q', cx: x, cy: bottom, x, y: bottom - r },
    { op: 'L', x, y: y + r },
    { op: 'Q', cx: x, cy: y, x: x + r, y },
    { op: 'Z' },
  ];
}
