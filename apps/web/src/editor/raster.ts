/**
 * PNG rasterization of the base image plus annotations. This is the
 * client-side preview that the server re-encodes and re-redacts; it must wait
 * for the bundled fonts so exported text never falls back to missing glyphs.
 *
 * Paint order (same as the SVG editing layer): the base image, then blur and
 * pixelate on the base (with solid-redaction pixels already blackened so no
 * filter can carry a secret out), the spotlight dim, every other annotation
 * in list order, and solid redactions last.
 */
import type { Annotation } from '@/api/types';
import { collectEffectOps, collectRedactionRegions } from './effects';
import { applyBaseEffects, drawSpotlightDim } from './effectsCanvas';
import { REDACT_FILL, redactionsLast, strokeOf, strokeWidthOf, type ImageSize } from './model';
import { tracePath } from './paths';
import {
  CANVAS_FAMILIES,
  MITER_LIMIT,
  MONO_FAMILY,
  PIN_RING,
  arrowPrimitive,
  canvasFont,
  freehandPath,
  highlightPrimitive,
  pinNumberColor,
  pinParts,
  shapePrimitive,
  textParts,
} from './shapes';

const MAX_EXPORT_PIXELS = 24_000_000;

export class FontNotReadyError extends Error {
  constructor(detail: string) {
    super(`fonts not ready: ${detail}`);
    this.name = 'FontNotReadyError';
  }
}

export class ExportError extends Error {
  constructor(readonly reason: 'too-large' | 'encode-failed' | 'no-canvas') {
    super(reason);
    this.name = 'ExportError';
  }
}

/** All text the export will draw, so exactly the needed unicode-range chunks are requested. */
export function exportTexts(list: readonly Annotation[]): string {
  let s = '0123456789';
  for (const a of list) if (a.kind === 'text') s += a.geometry.text;
  return s;
}

/** Font loads that make every face used by the list's text (all styles) available to Canvas and SVG. */
export function fontLoadJobs(list: readonly Annotation[]): Promise<unknown>[] {
  const text = exportTexts(list);
  // The color emoji face is one 5.8 MB file with no unicode-range, so it is requested only when the
  // text really contains pictographs, flags or keycaps; otherwise every export would download it.
  const needsEmoji = /[\p{Extended_Pictographic}\p{Regional_Indicator}\u20e3]/u.test(text);
  const jobs: Promise<unknown>[] = [];
  for (const family of CANVAS_FAMILIES) {
    if (family === 'Noto Color Emoji' && !needsEmoji) continue;
    for (const weight of [400, 600]) {
      jobs.push(document.fonts.load(`${weight} 20px "${family}"`, text));
    }
  }
  let mono = '';
  for (const a of list) if (a.kind === 'text' && a.style?.textStyle === 'mono') mono += a.geometry.text;
  if (mono.length > 0) jobs.push(document.fonts.load(`400 20px "${MONO_FAMILY}"`, `0123456789${mono}`));
  return jobs;
}

/**
 * Loads every face that covers the annotation text and waits until they are
 * usable. Rejects with FontNotReadyError instead of drawing with a fallback.
 */
export async function ensureFontsReady(list: readonly Annotation[], timeoutMs = 20_000): Promise<void> {
  const jobs = fontLoadJobs(list);
  const timeout = Promise.withResolvers<never>();
  const timer = window.setTimeout(() => timeout.reject(new FontNotReadyError('timeout')), timeoutMs);
  try {
    await Promise.race([Promise.all(jobs).then(() => document.fonts.ready), timeout.promise]);
  } catch (err) {
    throw err instanceof FontNotReadyError ? err : new FontNotReadyError(err instanceof Error ? err.message : 'load failed');
  } finally {
    window.clearTimeout(timer);
  }
  // Every face that was requested must have finished loading; a failed face reports "error".
  for (const face of document.fonts) {
    if (face.status === 'error') throw new FontNotReadyError(face.family);
  }
}

type Ctx = CanvasRenderingContext2D;

function drawAnnotation(ctx: Ctx, a: Annotation, size: ImageSize): void {
  switch (a.kind) {
    case 'spotlight':
    case 'blur':
    case 'pixelate':
      // Base-layer effects: applied before this pass (see renderAnnotatedPng).
      break;
    case 'redact': {
      const g = a.geometry;
      ctx.fillStyle = REDACT_FILL;
      ctx.fillRect(Math.floor(g.x * size.width), Math.floor(g.y * size.height), Math.ceil(g.width * size.width) + 1, Math.ceil(g.height * size.height) + 1);
      break;
    }
    case 'highlight': {
      const p = highlightPrimitive(a, size);
      ctx.save();
      ctx.globalAlpha = p.opacity;
      ctx.fillStyle = p.fill;
      ctx.fillRect(p.x, p.y, p.width, p.height);
      ctx.restore();
      break;
    }
    case 'rectangle':
    case 'ellipse':
    case 'diamond':
    case 'line': {
      const p = shapePrimitive(a, size);
      ctx.beginPath();
      switch (p.type) {
        case 'rect':
          ctx.rect(p.x, p.y, p.width, p.height);
          break;
        case 'ellipse':
          ctx.ellipse(p.cx, p.cy, p.rx, p.ry, 0, 0, Math.PI * 2);
          break;
        case 'polygon':
          p.points.forEach((q, i) => {
            if (i === 0) ctx.moveTo(q.x, q.y);
            else ctx.lineTo(q.x, q.y);
          });
          ctx.closePath();
          break;
        case 'line':
          ctx.moveTo(p.from.x, p.from.y);
          ctx.lineTo(p.to.x, p.to.y);
          break;
      }
      // Fill sits under the stroke, as in the SVG layer.
      if (p.fill) {
        ctx.fillStyle = p.fill;
        ctx.fill();
      }
      ctx.strokeStyle = p.stroke;
      ctx.lineWidth = p.strokeWidth;
      ctx.lineJoin = 'miter';
      ctx.miterLimit = MITER_LIMIT;
      ctx.lineCap = p.type === 'line' ? 'round' : 'butt';
      ctx.stroke();
      break;
    }
    case 'arrow': {
      const p = arrowPrimitive(a, size);
      ctx.strokeStyle = p.stroke;
      ctx.fillStyle = p.stroke;
      ctx.lineWidth = p.width;
      ctx.lineCap = 'round';
      ctx.lineJoin = 'round';
      if (p.shaft.length > 0) {
        ctx.beginPath();
        tracePath(ctx, p.shaft);
        ctx.stroke();
      }
      for (const head of p.filledHeads) {
        ctx.beginPath();
        tracePath(ctx, head);
        ctx.fill();
      }
      for (const head of p.openHeads) {
        ctx.beginPath();
        tracePath(ctx, head);
        ctx.stroke();
      }
      break;
    }
    case 'freehand': {
      ctx.strokeStyle = strokeOf(a);
      ctx.lineWidth = strokeWidthOf(a);
      ctx.lineCap = 'round';
      ctx.lineJoin = 'round';
      ctx.beginPath();
      tracePath(ctx, freehandPath(a, size));
      ctx.stroke();
      break;
    }
    case 'text': {
      const p = textParts(a, size);
      ctx.font = canvasFont(p.fontSize, p.weight, p.family);
      ctx.textAlign = 'left';
      ctx.textBaseline = 'alphabetic';
      ctx.lineJoin = 'round';
      if (p.backdrop) {
        ctx.beginPath();
        tracePath(ctx, p.backdrop.path);
        ctx.fillStyle = p.backdrop.fill;
        ctx.fill();
      }
      if (p.shadow) {
        ctx.save();
        ctx.globalAlpha = p.shadow.opacity;
        ctx.fillStyle = p.shadow.color;
        for (const line of p.lines) ctx.fillText(line.text, p.x + p.shadow.dx, line.y + p.shadow.dy);
        ctx.restore();
      }
      if (p.halo) {
        ctx.lineWidth = p.halo.width;
        ctx.strokeStyle = p.halo.color;
      }
      ctx.fillStyle = p.fill;
      for (const line of p.lines) {
        if (p.halo) ctx.strokeText(line.text, p.x, line.y);
        ctx.fillText(line.text, p.x, line.y);
      }
      for (const u of p.underlines) {
        ctx.beginPath();
        tracePath(ctx, u.path);
        ctx.strokeStyle = p.fill;
        ctx.lineWidth = u.width;
        ctx.lineCap = 'butt';
        ctx.stroke();
      }
      break;
    }
    case 'pin': {
      const p = pinParts(a, size);
      ctx.beginPath();
      ctx.arc(p.cx, p.cy, p.r, 0, Math.PI * 2);
      ctx.fillStyle = p.fill;
      ctx.fill();
      ctx.lineWidth = Math.max(2, p.r / 7);
      ctx.strokeStyle = PIN_RING;
      ctx.stroke();
      ctx.font = canvasFont(p.fontSize, 600);
      ctx.textAlign = 'center';
      ctx.textBaseline = 'alphabetic';
      ctx.fillStyle = pinNumberColor(p.fill);
      ctx.fillText(String(a.geometry.number), p.cx, p.numberY);
      break;
    }
  }
}

export interface RasterResult {
  blob: Blob;
  width: number;
  height: number;
}

/** Renders the base image with annotations at the image's native pixel size. */
export async function renderAnnotatedPng(
  image: CanvasImageSource,
  size: ImageSize,
  annotations: readonly Annotation[],
): Promise<RasterResult> {
  if (size.width * size.height > MAX_EXPORT_PIXELS) throw new ExportError('too-large');
  const ordered = redactionsLast(annotations);
  await ensureFontsReady(ordered);
  const ops = collectEffectOps(ordered, size);
  const canvas = document.createElement('canvas');
  canvas.width = size.width;
  canvas.height = size.height;
  const ctx = canvas.getContext('2d', { alpha: false, willReadFrequently: ops.length > 0 });
  if (!ctx) throw new ExportError('no-canvas');
  ctx.drawImage(image, 0, 0, size.width, size.height);
  applyBaseEffects(ctx, { x: 0, y: 0 }, ops, collectRedactionRegions(ordered, size));
  drawSpotlightDim(ctx, ordered, size);
  for (const a of ordered) drawAnnotation(ctx, a, size);
  const pending = Promise.withResolvers<Blob | null>();
  canvas.toBlob(pending.resolve, 'image/png');
  const blob = await pending.promise;
  if (!blob) throw new ExportError('encode-failed');
  return { blob, width: size.width, height: size.height };
}
