/**
 * Drawing parameters shared by the SVG editing layer and the PNG rasterizer,
 * so what people see while editing is what gets exported.
 */
import type {
  ArrowAnnotation,
  DiamondAnnotation,
  EllipseAnnotation,
  FreehandAnnotation,
  HighlightAnnotation,
  LineAnnotation,
  PinAnnotation,
  RectangleAnnotation,
  TextAnnotation,
  TextStyle,
} from '@/api/types';
import {
  BASELINE,
  LINE_HEIGHT,
  arrowControlPoint,
  arrowStyleOf,
  diamondVertices,
  fillOf,
  fontSizeOf,
  headTriangle,
  highlightOpacityOf,
  isSmoothed,
  labelPadding,
  pinRadiusPx,
  strokeOf,
  strokeWidthOf,
  textLines,
  textStyleOf,
  toPx,
  type ImageSize,
  type TextMeasure,
} from './model';
import { polylinePath, roundedRectPath, smoothPath, trimQuad, type PathCmd, type PxPoint } from './paths';

export const CANVAS_FAMILIES = ['IBM Plex Sans', 'Noto Sans TC', 'Noto Sans JP', 'Noto Sans KR', 'Noto Sans SC', 'Noto Color Emoji'] as const;
export const FONT_STACK = `${CANVAS_FAMILIES.map((f) => `"${f}"`).join(', ')}, sans-serif`;

/** Bundled mono face (400 only is loaded); CJK and emoji fall back to the same bundled faces as the sans stack. */
export const MONO_FAMILY = 'IBM Plex Mono';
export const MONO_STACK = `"${MONO_FAMILY}", ${CANVAS_FAMILIES.slice(1)
  .map((f) => `"${f}"`)
  .join(', ')}, monospace`;

/** CSS font-family of a text annotation style (the in-place editor, SVG and Canvas all use this). */
export function textFontFamily(textStyle?: TextStyle): string {
  return textStyle === 'mono' ? MONO_STACK : FONT_STACK;
}

/** CSS font-weight of a text annotation style: `bold` uses the loaded 600 face. */
export function textFontWeight(textStyle?: TextStyle): 400 | 600 {
  return textStyle === 'bold' ? 600 : 400;
}

export function canvasFont(sizePx: number, weight: 400 | 600 = 400, family: string = FONT_STACK): string {
  return `${weight} ${sizePx}px ${family}`;
}

export const HALO_COLOR = '#ffffff';
export const PIN_RING = '#ffffff';

export function haloWidthPx(fontSizePx: number): number {
  return Math.max(2, fontSizePx / 6);
}

/** SVG's default `stroke-miterlimit` is 4 but Canvas's is 10; both renderers pin 4 so thin diamond tips are beveled identically. */
export const MITER_LIMIT = 4;

/**
 * Geometry of the closed shapes and the plain line in image pixels. The SVG
 * layer and the PNG rasterizer both draw exactly this: fill first (when
 * present), then the stroke on top.
 */
export type ShapePrimitive = { stroke: string; strokeWidth: number; fill: string | null } & (
  | { type: 'rect'; x: number; y: number; width: number; height: number }
  | { type: 'ellipse'; cx: number; cy: number; rx: number; ry: number }
  | { type: 'polygon'; points: { x: number; y: number }[] }
  | { type: 'line'; from: { x: number; y: number }; to: { x: number; y: number } }
);

export function shapePrimitive(a: RectangleAnnotation | EllipseAnnotation | DiamondAnnotation | LineAnnotation, size: ImageSize): ShapePrimitive {
  const paint = { stroke: strokeOf(a), strokeWidth: strokeWidthOf(a), fill: fillOf(a) };
  switch (a.kind) {
    case 'rectangle': {
      const g = a.geometry;
      return { ...paint, type: 'rect', x: g.x * size.width, y: g.y * size.height, width: g.width * size.width, height: g.height * size.height };
    }
    case 'ellipse': {
      const g = a.geometry;
      const rx = (g.width * size.width) / 2;
      const ry = (g.height * size.height) / 2;
      return { ...paint, type: 'ellipse', cx: g.x * size.width + rx, cy: g.y * size.height + ry, rx, ry };
    }
    case 'diamond':
      return { ...paint, type: 'polygon', points: diamondVertices(a.geometry, size) };
    case 'line':
      return { ...paint, type: 'line', from: toPx(a.geometry.start, size), to: toPx(a.geometry.end, size) };
  }
}

/**
 * Arrow in image pixels as path commands. Both renderers stroke `shaft` (round caps and
 * joins), fill every `filledHeads` path with the arrow color and stroke every `openHeads`
 * polyline. The shaft of a filled head stops at the head base so a thick line cannot poke
 * through the tip; a curved arrow follows the quadratic through its control point.
 */
export interface ArrowPrimitive {
  stroke: string;
  width: number;
  shaft: PathCmd[];
  filledHeads: PathCmd[][];
  openHeads: PathCmd[][];
}

function trianglePath(h: readonly [PxPoint, PxPoint, PxPoint]): PathCmd[] {
  return [
    { op: 'M', x: h[0].x, y: h[0].y },
    { op: 'L', x: h[1].x, y: h[1].y },
    { op: 'L', x: h[2].x, y: h[2].y },
    { op: 'Z' },
  ];
}

function chevronPath(h: readonly [PxPoint, PxPoint, PxPoint]): PathCmd[] {
  return [
    { op: 'M', x: h[1].x, y: h[1].y },
    { op: 'L', x: h[0].x, y: h[0].y },
    { op: 'L', x: h[2].x, y: h[2].y },
  ];
}

function angleBetween(from: PxPoint, to: PxPoint, fallback: number): number {
  return Math.hypot(to.x - from.x, to.y - from.y) > 1e-6 ? Math.atan2(to.y - from.y, to.x - from.x) : fallback;
}

export function arrowPrimitive(a: ArrowAnnotation, size: ImageSize): ArrowPrimitive {
  const width = strokeWidthOf(a);
  const style = arrowStyleOf(a);
  const s = toPx(a.geometry.start, size);
  const e = toPx(a.geometry.end, size);
  const controlNorm = arrowControlPoint(a);
  const c = controlNorm ? toPx(controlNorm, size) : null;
  const chord = Math.atan2(e.y - s.y, e.x - s.x);
  const endHead = headTriangle(e, angleBetween(c ?? s, e, chord), width);
  const startHead = headTriangle(s, angleBetween(c ?? e, s, chord + Math.PI), width);
  const mid = (h: readonly [PxPoint, PxPoint, PxPoint]): PxPoint => ({ x: (h[1].x + h[2].x) / 2, y: (h[1].y + h[2].y) / 2 });
  const endBase = mid(endHead);
  const startBase = mid(startHead);
  const baseDist = Math.hypot(e.x - endBase.x, e.y - endBase.y);
  const chordLen = Math.hypot(e.x - s.x, e.y - s.y);
  const stroke = strokeOf(a);

  if (style === 'open') {
    const shaft: PathCmd[] = c
      ? [
          { op: 'M', x: s.x, y: s.y },
          { op: 'Q', cx: c.x, cy: c.y, x: e.x, y: e.y },
        ]
      : [
          { op: 'M', x: s.x, y: s.y },
          { op: 'L', x: e.x, y: e.y },
        ];
    return { stroke, width, shaft, filledHeads: [], openHeads: [chevronPath(endHead)] };
  }

  const double = style === 'double';
  const filledHeads = double ? [trianglePath(startHead), trianglePath(endHead)] : [trianglePath(endHead)];
  let shaft: PathCmd[];
  if (c) {
    const t = trimQuad(s, c, e, double ? baseDist : 0, baseDist);
    shaft = t
      ? [
          { op: 'M', x: t.s.x, y: t.s.y },
          { op: 'Q', cx: t.c.x, cy: t.c.y, x: t.e.x, y: t.e.y },
        ]
      : [];
  } else if (double) {
    shaft =
      chordLen > 2 * baseDist
        ? [
            { op: 'M', x: startBase.x, y: startBase.y },
            { op: 'L', x: endBase.x, y: endBase.y },
          ]
        : [];
  } else {
    // A shaft shorter than the head collapses onto its start (a round dot hidden by the head).
    const to = chordLen > baseDist ? endBase : s;
    shaft = [
      { op: 'M', x: s.x, y: s.y },
      { op: 'L', x: to.x, y: to.y },
    ];
  }
  return { stroke, width, shaft, filledHeads, openHeads: [] };
}

/** Freehand stroke as path commands: the shared smooth quadratic chain when smoothing is on, else the legacy polyline. */
export function freehandPath(a: FreehandAnnotation, size: ImageSize): PathCmd[] {
  const pts = a.geometry.points.map((p) => toPx(p, size));
  return isSmoothed(a) ? smoothPath(pts) : polylinePath(pts);
}

/** Highlight rectangle in image pixels; drawn with plain alpha (it is not a redaction). */
export function highlightPrimitive(a: HighlightAnnotation, size: ImageSize): { x: number; y: number; width: number; height: number; fill: string; opacity: number } {
  const g = a.geometry;
  return { x: g.x * size.width, y: g.y * size.height, width: g.width * size.width, height: g.height * size.height, fill: strokeOf(a), opacity: highlightOpacityOf(a) };
}

export interface TextParts {
  x: number;
  lines: { text: string; y: number }[];
  fontSize: number;
  fill: string;
  family: string;
  weight: 400 | 600;
  /** White outline stroked under the glyphs (outline, mono, bold, underline). */
  halo: { color: string; width: number } | null;
  /** Hard offset copy of the glyphs drawn first (shadow style); no blur, so SVG and Canvas match exactly. */
  shadow: { dx: number; dy: number; color: string; opacity: number } | null;
  /** Colored rounded box behind the lines (label style). */
  backdrop: { path: PathCmd[]; fill: string } | null;
  /** One rule per non-empty line (underline style), stroked in the text color. */
  underlines: { path: PathCmd[]; width: number }[];
}

/**
 * Everything needed to draw a text annotation in either renderer. `measure` supplies line
 * widths for the label backdrop and underlines (the same bundled fonts as the export).
 */
export function textParts(a: TextAnnotation, size: ImageSize, measure: TextMeasure = measureTextWidth): TextParts {
  const fontSize = fontSizeOf(a);
  const style = textStyleOf(a);
  const rawStyle = a.style?.textStyle;
  const pos = toPx(a.geometry.position, size);
  const pad = style === 'label' ? labelPadding(fontSize) : { x: 0, y: 0 };
  const x = pos.x + pad.x;
  const color = strokeOf(a);
  const lines = textLines(a.geometry.text).map((text, i) => ({ text, y: pos.y + pad.y + fontSize * (BASELINE + LINE_HEIGHT * i) }));
  const widths = lines.map((l) => {
    const w = l.text.length === 0 ? 0 : measure(l.text, fontSize, rawStyle);
    return Number.isFinite(w) ? Math.max(0, w) : 0;
  });
  const widest = Math.max(0, ...widths);
  const shadowOffset = Math.max(1, fontSize / 14);
  return {
    x,
    fontSize,
    fill: style === 'label' ? pinNumberColor(color) : color,
    family: textFontFamily(rawStyle),
    weight: textFontWeight(rawStyle),
    lines,
    halo: style === 'plain' || style === 'label' || style === 'shadow' ? null : { color: HALO_COLOR, width: haloWidthPx(fontSize) },
    shadow: style === 'shadow' ? { dx: shadowOffset, dy: shadowOffset, color: '#000000', opacity: 0.45 } : null,
    backdrop: style === 'label' ? { path: roundedRectPath(pos.x, pos.y, widest + 2 * pad.x, lines.length * fontSize * LINE_HEIGHT + 2 * pad.y, fontSize * 0.25), fill: color } : null,
    underlines:
      style === 'underline'
        ? lines.flatMap((l, i): { path: PathCmd[]; width: number }[] => {
            const w = widths[i] ?? 0;
            if (w <= 0) return [];
            const y = l.y + fontSize * 0.12;
            return [
              {
                path: [
                  { op: 'M', x, y },
                  { op: 'L', x: x + w, y },
                ],
                width: Math.max(1, fontSize / 14),
              },
            ];
          })
        : [],
  };
}

export function pinParts(a: PinAnnotation, size: ImageSize): { cx: number; cy: number; r: number; fill: string; fontSize: number; numberY: number } {
  const c = toPx(a.geometry.position, size);
  const r = pinRadiusPx(a.style);
  const fontSize = Math.round(r * 1.05);
  return { cx: c.x, cy: c.y, r, fill: strokeOf(a), fontSize, numberY: c.y + fontSize * 0.35 };
}

/** Pin number color: white on dark fills, near-black on light fills, for legible numbers. */
export function pinNumberColor(fill: string): string {
  const n = Number.parseInt(fill.slice(1), 16);
  const r = (n >> 16) & 255;
  const g = (n >> 8) & 255;
  const b = n & 255;
  return 0.299 * r + 0.587 * g + 0.114 * b > 160 ? '#0b2540' : '#ffffff';
}

let measureCtx: CanvasRenderingContext2D | null = null;

/** Widest line of a text annotation in image pixels, measured with the bundled fonts of its text style. */
export function measureTextWidth(text: string, fontSizePx: number, textStyle?: TextStyle): number {
  measureCtx ??= document.createElement('canvas').getContext('2d');
  if (!measureCtx) return Array.from(text).length * fontSizePx * 0.62;
  measureCtx.font = canvasFont(fontSizePx, textFontWeight(textStyle), textFontFamily(textStyle));
  let widest = 0;
  for (const line of textLines(text)) widest = Math.max(widest, measureCtx.measureText(line).width);
  return widest;
}
