/**
 * Style plumbing shared by the toolbar, the inspector and the gesture handlers: which controls apply to
 * which kind, the values a control shows (for a selected object or for the next one drawn), the style a
 * freshly drawn object is saved with, and how a style patch changes an existing object. Pure, no DOM.
 *
 * Saved styles never carry a value that equals the legacy meaning of "absent" (filled arrow, outline text,
 * no smoothing), so a document that uses none of the new features keeps its old schema version.
 */
import type { Annotation, AnnotationKind, AnnotationStyle, ArrowStyle, NormRect, Point, TextStyle } from '@/api/types';
import {
  EFFECT_SIZE_DEFAULT_BLUR,
  EFFECT_SIZE_DEFAULT_PIXELATE,
  EFFECT_SIZE_MAX,
  EFFECT_SIZE_MIN,
  HIGHLIGHT_OPACITY_DEFAULT,
  HIGHLIGHT_OPACITY_MAX,
  HIGHLIGHT_OPACITY_MIN,
  ARROW_STYLES,
  HIGHLIGHT_COLOR,
  PALETTE,
  TEXT_STYLES,
  defaultArrowControl,
  fillOf,
  fontSizeOf,
  strokeOf,
  strokeWidthOf,
} from './model';

export type { ArrowStyle, TextStyle };
export { ARROW_STYLES, EFFECT_SIZE_MAX, EFFECT_SIZE_MIN, HIGHLIGHT_COLOR, HIGHLIGHT_OPACITY_MAX, HIGHLIGHT_OPACITY_MIN, TEXT_STYLES };

export const MIN_STROKE_WIDTH = 1;
export const MAX_STROKE_WIDTH = 32;
export const MIN_FONT_SIZE = 10;
export const MAX_FONT_SIZE = 128;


// ------------------------------------------------------------------- tools

/** Tools that create an object by dragging a rectangle. `filledRect` is a preset of the rectangle kind, not a wire kind. */
export type RectTool = 'rectangle' | 'filledRect' | 'ellipse' | 'diamond' | 'highlight' | 'spotlight' | 'blur' | 'pixelate';

export function isRectTool(tool: string): tool is RectTool {
  switch (tool) {
    case 'rectangle':
    case 'filledRect':
    case 'ellipse':
    case 'diamond':
    case 'highlight':
    case 'spotlight':
    case 'blur':
    case 'pixelate':
      return true;
    default:
      return false;
  }
}

/** The annotation kind a drawing tool creates, or null for tools that create nothing (select, pan, crop). */
export function kindOfTool(tool: string): AnnotationKind | null {
  switch (tool) {
    case 'filledRect':
      return 'rectangle';
    case 'rectangle':
    case 'ellipse':
    case 'diamond':
    case 'highlight':
    case 'spotlight':
    case 'blur':
    case 'pixelate':
    case 'line':
    case 'arrow':
    case 'freehand':
    case 'text':
    case 'pin':
    case 'redact':
      return tool;
    default:
      return null;
  }
}

// ------------------------------------------------------------- capabilities

/** Which controls are meaningful. A control that cannot change the saved result is never shown. */
export interface StyleCaps {
  color: boolean;
  fill: boolean;
  width: boolean;
  font: boolean;
  opacity: boolean;
  effect: boolean;
  arrow: boolean;
  text: boolean;
  smoothing: boolean;
}

const NO_CAPS: StyleCaps = { color: false, fill: false, width: false, font: false, opacity: false, effect: false, arrow: false, text: false, smoothing: false };

export function capsOfKind(kind: AnnotationKind): StyleCaps {
  switch (kind) {
    case 'rectangle':
    case 'ellipse':
    case 'diamond':
      return { ...NO_CAPS, color: true, fill: true, width: true };
    case 'line':
      return { ...NO_CAPS, color: true, width: true };
    case 'arrow':
      return { ...NO_CAPS, color: true, width: true, arrow: true };
    case 'freehand':
      return { ...NO_CAPS, color: true, width: true, smoothing: true };
    case 'text':
      return { ...NO_CAPS, color: true, font: true, text: true };
    case 'pin':
      return { ...NO_CAPS, color: true, font: true };
    case 'highlight':
      return { ...NO_CAPS, color: true, opacity: true };
    case 'blur':
    case 'pixelate':
      return { ...NO_CAPS, effect: true };
    case 'spotlight':
    case 'redact':
      return NO_CAPS;
  }
}

/** Controls for a drawing tool. The filled-rectangle preset takes its fill from the colour, so it has no separate fill control. */
export function capsOfTool(tool: string): StyleCaps {
  if (tool === 'filledRect') return { ...NO_CAPS, color: true, width: true };
  const kind = kindOfTool(tool);
  return kind ? capsOfKind(kind) : NO_CAPS;
}

export function mergeCaps(list: readonly StyleCaps[]): StyleCaps {
  const out = { ...NO_CAPS };
  for (const c of list) for (const k of Object.keys(out) as (keyof StyleCaps)[]) out[k] = out[k] || c[k];
  return out;
}

export function hasAnyCaps(c: StyleCaps): boolean {
  return Object.values(c).some(Boolean);
}

// ------------------------------------------------------------------ values

/** What the controls display. */
export interface StyleValues {
  color: string;
  /** `null` = no fill. */
  fill: string | null;
  width: number;
  font: number;
  opacity: number;
  effect: number;
  arrow: ArrowStyle;
  text: TextStyle;
  smoothing: boolean;
}

/** Style the next drawn object is created with. Remembered per tool family so no hidden default differs from what the controls show. */
export interface DrawDefaults {
  color: string;
  highlightColor: string;
  fill: string | null;
  width: number;
  font: number;
  opacity: number;
  blurSize: number;
  pixelSize: number;
  arrow: ArrowStyle;
  text: TextStyle;
  smoothing: boolean;
}

export const INITIAL_DEFAULTS: DrawDefaults = {
  color: PALETTE[0] ?? '#e5362b',
  highlightColor: HIGHLIGHT_COLOR,
  fill: null,
  width: 4,
  font: 28,
  opacity: HIGHLIGHT_OPACITY_DEFAULT,
  blurSize: EFFECT_SIZE_DEFAULT_BLUR,
  pixelSize: EFFECT_SIZE_DEFAULT_PIXELATE,
  arrow: 'filled',
  text: 'outline',
  smoothing: true,
};

export function valuesOfDefaults(d: DrawDefaults, kind: AnnotationKind | null): StyleValues {
  return {
    color: kind === 'highlight' ? d.highlightColor : d.color,
    fill: d.fill,
    width: d.width,
    font: d.font,
    opacity: d.opacity,
    effect: kind === 'pixelate' ? d.pixelSize : d.blurSize,
    arrow: d.arrow,
    text: d.text,
    smoothing: d.smoothing,
  };
}

/** Values shown for an existing object: absent fields display their legacy meaning. */
export function valuesOf(a: Annotation): StyleValues {
  const s = a.kind === 'redact' ? undefined : a.style;
  return {
    color: a.kind === 'highlight' ? (s?.stroke ?? HIGHLIGHT_COLOR) : strokeOf(a),
    fill: fillOf(a),
    width: strokeWidthOf(a),
    font: fontSizeOf(a),
    opacity: s?.opacity ?? HIGHLIGHT_OPACITY_DEFAULT,
    effect: s?.effectSizePx ?? (a.kind === 'pixelate' ? EFFECT_SIZE_DEFAULT_PIXELATE : EFFECT_SIZE_DEFAULT_BLUR),
    arrow: s?.arrowStyle ?? 'filled',
    text: s?.textStyle ?? 'outline',
    smoothing: s?.smoothing ?? false,
  };
}

/** Values for several objects: each control shows the first object that has it. */
export function valuesOfMany(items: readonly Annotation[], fallback: StyleValues): StyleValues {
  const out: StyleValues = { ...fallback };
  for (const key of Object.keys(out) as (keyof StyleValues)[]) {
    const owner = items.find((a) => capsOfKind(a.kind)[key]);
    if (owner) Object.assign(out, { [key]: valuesOf(owner)[key] });
  }
  return out;
}

// ------------------------------------------------------------------ patches

export interface StylePatch {
  stroke?: string;
  strokeWidthPx?: number;
  fontSizePx?: number;
  /** A colour sets the fill, `null` removes it. */
  fill?: string | null;
  opacity?: number;
  effectSizePx?: number;
  arrowStyle?: ArrowStyle;
  textStyle?: TextStyle;
  smoothing?: boolean;
}

function clampInt(v: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, Math.round(v)));
}

/** Opacity is stored with two decimals so a slider step never leaves binary-float noise in the wire form. */
function clampOpacity(v: number): number {
  return Math.min(HIGHLIGHT_OPACITY_MAX, Math.max(HIGHLIGHT_OPACITY_MIN, Math.round(v * 100) / 100));
}

/** Remembers a patch as the default for the next object of `kind`. */
export function applyPatchToDefaults(d: DrawDefaults, patch: StylePatch, kind: AnnotationKind | null): DrawDefaults {
  const next = { ...d };
  if (patch.stroke !== undefined) {
    if (kind === 'highlight') next.highlightColor = patch.stroke;
    else next.color = patch.stroke;
  }
  if (patch.strokeWidthPx !== undefined) next.width = clampInt(patch.strokeWidthPx, MIN_STROKE_WIDTH, MAX_STROKE_WIDTH);
  if (patch.fontSizePx !== undefined) next.font = clampInt(patch.fontSizePx, MIN_FONT_SIZE, MAX_FONT_SIZE);
  if (patch.fill !== undefined) next.fill = patch.fill;
  if (patch.opacity !== undefined) next.opacity = clampOpacity(patch.opacity);
  if (patch.effectSizePx !== undefined) {
    const size = clampInt(patch.effectSizePx, EFFECT_SIZE_MIN, EFFECT_SIZE_MAX);
    if (kind === 'pixelate') next.pixelSize = size;
    else next.blurSize = size;
  }
  if (patch.arrowStyle !== undefined) next.arrow = patch.arrowStyle;
  if (patch.textStyle !== undefined) next.text = patch.textStyle;
  if (patch.smoothing !== undefined) next.smoothing = patch.smoothing;
  return next;
}

/**
 * Applies a patch where it is meaningful for the object's kind. Redactions and spotlights carry no style.
 * Absent-equals-default values are removed rather than written, and switching an arrow to the curved
 * style materializes its control point so the handle has something to drag.
 */
export function withStyle(a: Annotation, patch: StylePatch): Annotation {
  if (a.kind === 'redact' || a.kind === 'spotlight') return a;
  const caps = capsOfKind(a.kind);
  const next: AnnotationStyle = { ...(a.style ?? {}) };
  if (patch.stroke !== undefined && caps.color) next.stroke = patch.stroke;
  if (patch.strokeWidthPx !== undefined && caps.width) next.strokeWidthPx = clampInt(patch.strokeWidthPx, MIN_STROKE_WIDTH, MAX_STROKE_WIDTH);
  if (patch.fontSizePx !== undefined && caps.font) next.fontSizePx = clampInt(patch.fontSizePx, MIN_FONT_SIZE, MAX_FONT_SIZE);
  if (patch.fill !== undefined && caps.fill) {
    if (patch.fill === null) delete next.fill;
    else next.fill = patch.fill;
  }
  if (patch.opacity !== undefined && caps.opacity) next.opacity = clampOpacity(patch.opacity);
  if (patch.effectSizePx !== undefined && caps.effect) next.effectSizePx = clampInt(patch.effectSizePx, EFFECT_SIZE_MIN, EFFECT_SIZE_MAX);
  if (patch.textStyle !== undefined && caps.text) {
    if (patch.textStyle === 'outline') delete next.textStyle;
    else next.textStyle = patch.textStyle;
  }
  if (patch.smoothing !== undefined && caps.smoothing) {
    if (patch.smoothing) next.smoothing = true;
    else delete next.smoothing;
  }
  if (patch.arrowStyle !== undefined && caps.arrow) {
    if (patch.arrowStyle === 'filled') delete next.arrowStyle;
    else next.arrowStyle = patch.arrowStyle;
  }
  if (a.kind === 'arrow' && patch.arrowStyle === 'curved' && !a.geometry.control) {
    return { ...a, style: next, geometry: { ...a.geometry, control: defaultArrowControl(a.geometry.start, a.geometry.end) } };
  }
  return { ...a, style: next } as Annotation;
}

// ------------------------------------------------------------- new objects

/** Style saved with a freshly drawn object. Values equal to the legacy "absent" meaning are omitted. */
export function newStyle(tool: RectTool | 'line' | 'arrow' | 'freehand' | 'text' | 'pin', d: DrawDefaults): AnnotationStyle {
  switch (tool) {
    case 'rectangle':
    case 'ellipse':
    case 'diamond':
      return { stroke: d.color, strokeWidthPx: d.width, ...(d.fill ? { fill: d.fill } : {}) };
    case 'filledRect':
      return { stroke: d.color, strokeWidthPx: d.width, fill: d.color };
    case 'line':
      return { stroke: d.color, strokeWidthPx: d.width };
    case 'arrow':
      return { stroke: d.color, strokeWidthPx: d.width, ...(d.arrow !== 'filled' ? { arrowStyle: d.arrow } : {}) };
    case 'freehand':
      return { stroke: d.color, strokeWidthPx: d.width, ...(d.smoothing ? { smoothing: true } : {}) };
    case 'text':
      return { stroke: d.color, fontSizePx: d.font, ...(d.text !== 'outline' ? { textStyle: d.text } : {}) };
    case 'pin':
      return { stroke: d.color, fontSizePx: d.font };
    case 'highlight':
      return { stroke: d.highlightColor, opacity: d.opacity };
    case 'blur':
      return { effectSizePx: d.blurSize };
    case 'pixelate':
      return { effectSizePx: d.pixelSize };
    case 'spotlight':
      return {};
  }
}

/** A rectangle-geometry annotation for a drawing tool. Spotlights are saved without a style key. */
export function rectAnnotation(tool: RectTool, id: string, geometry: NormRect, d: DrawDefaults): Annotation {
  switch (tool) {
    case 'rectangle':
    case 'filledRect':
      return { id, kind: 'rectangle', geometry, comment: '', style: newStyle(tool, d) };
    case 'ellipse':
    case 'diamond':
    case 'highlight':
    case 'blur':
    case 'pixelate':
      return { id, kind: tool, geometry, comment: '', style: newStyle(tool, d) };
    case 'spotlight':
      return { id, kind: 'spotlight', geometry, comment: '' };
  }
}

/** Arrow being drawn: a curved arrow gets its control point immediately so the live preview already bends. */
export function arrowAnnotation(id: string, start: Point, end: Point, d: DrawDefaults): Annotation {
  return {
    id,
    kind: 'arrow',
    geometry: d.arrow === 'curved' ? { start, end, control: defaultArrowControl(start, end) } : { start, end },
    comment: '',
    style: newStyle('arrow', d),
  };
}

/** Rectangle-geometry kinds, including the redaction. */
export function isRectGeometry(a: Annotation): a is Extract<Annotation, { geometry: NormRect }> {
  switch (a.kind) {
    case 'rectangle':
    case 'ellipse':
    case 'diamond':
    case 'redact':
    case 'highlight':
    case 'spotlight':
    case 'blur':
    case 'pixelate':
      return true;
    default:
      return false;
  }
}

/** Blur and pixelate only change how the base image looks; they are not redaction. */
export function isCosmeticEffect(kind: AnnotationKind): boolean {
  return kind === 'blur' || kind === 'pixelate';
}

/** Overlays that do not hide anything: used to decide whether the "not a redaction" warning applies. */
export function isOverlayKind(kind: AnnotationKind): boolean {
  return kind !== 'redact' && kind !== 'text' && kind !== 'pin';
}
