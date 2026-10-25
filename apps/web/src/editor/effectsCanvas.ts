/**
 * Canvas glue for the shared base-image effects (see effects.ts). The PNG
 * export and the editing layer both go through these functions, so blur,
 * pixelate and the spotlight dim are the same pixels in both.
 */
import { REDACT_FILL, SPOTLIGHT_DIM, type ImageSize } from './model';
import { applyEffectToBuffer, regionHeight, regionWidth, spotlightDimPath, type EffectOp, type PixelRegion } from './effects';
import { tracePath } from './paths';
import type { Annotation } from '@/api/types';

type Ctx = CanvasRenderingContext2D;

/**
 * Runs the blur/pixelate ops in order on the canvas. `origin` is the image
 * pixel that canvas pixel (0,0) shows (0,0 for a full export; the top-left of
 * the working box when only the affected area is materialized).
 *
 * Solid-redaction regions are painted black first, so nothing an effect
 * samples can contain a redacted secret. Only the pixels of each op's own
 * rectangle are read back (one bounded buffer per op, reused as scratch).
 */
export function applyBaseEffects(ctx: Ctx, origin: { x: number; y: number }, ops: readonly EffectOp[], redactions: readonly PixelRegion[]): void {
  if (ops.length === 0) return;
  ctx.fillStyle = REDACT_FILL;
  for (const r of redactions) ctx.fillRect(r.x0 - origin.x, r.y0 - origin.y, regionWidth(r), regionHeight(r));
  let scratch: Uint8ClampedArray | undefined;
  for (const op of ops) {
    const x = op.region.x0 - origin.x;
    const y = op.region.y0 - origin.y;
    const image = ctx.getImageData(x, y, regionWidth(op.region), regionHeight(op.region));
    if (op.kind === 'blur' && (!scratch || scratch.length < image.data.length)) scratch = new Uint8ClampedArray(image.data.length);
    applyEffectToBuffer(image.data, op, scratch);
    ctx.putImageData(image, x, y);
  }
}

/** Dims everything outside the union of the spotlights at the fixed 0.6 opacity (one fill, no seams). */
export function drawSpotlightDim(ctx: Ctx, list: readonly Annotation[], size: ImageSize): void {
  const path = spotlightDimPath(list, size);
  if (path.length === 0) return;
  ctx.save();
  ctx.globalAlpha = SPOTLIGHT_DIM;
  ctx.fillStyle = '#000000';
  ctx.beginPath();
  tracePath(ctx, path);
  ctx.fill();
  ctx.restore();
}
