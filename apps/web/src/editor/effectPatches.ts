/**
 * Real blur/pixelate pixels for the editing layer. The base image is
 * filtered with the same algorithm as the PNG export (effects.ts via
 * effectsCanvas.ts) and the result of each effect rectangle is handed to the
 * SVG layer as an image patch, so the editing view shows exactly the pixels
 * that Copy / Download / Send export.
 *
 * Work is keyed by a signature of the pixel-relevant inputs only (image,
 * image size, effect rectangles snapped to pixels, effect sizes, redaction
 * rectangles). Selection, hover, UI state and non-effect annotations never
 * change it, so they never re-filter the image. Only the bounding box of the
 * effect rectangles is materialized, never the whole image.
 */
import { useEffect, useState } from 'react';
import type { Annotation } from '@/api/types';
import { collectEffectOps, collectRedactionRegions, effectSignature, regionHeight, regionWidth, type EffectOp, type PixelRegion } from './effects';
import { applyBaseEffects } from './effectsCanvas';
import type { ImageSize } from './model';

export interface EffectPatch {
  id: string;
  kind: 'blur' | 'pixelate';
  region: PixelRegion;
  /** Object URL of the filtered pixels of `region`; owned (and revoked) by the hook. */
  url: string;
}

export type EffectPatches = ReadonlyMap<string, EffectPatch>;

const NO_PATCHES: EffectPatches = new Map();

/** Pixel dimensions of a canvas image source, used to map image pixels onto a source of another resolution. */
function sourceSize(image: CanvasImageSource, fallback: ImageSize): ImageSize {
  const dims = image as { naturalWidth?: number; naturalHeight?: number; width?: number; height?: number };
  const width = dims.naturalWidth ?? dims.width;
  const height = dims.naturalHeight ?? dims.height;
  return typeof width === 'number' && typeof height === 'number' && width > 0 && height > 0 ? { width, height } : fallback;
}

async function canvasToUrl(canvas: HTMLCanvasElement): Promise<string | null> {
  const pending = Promise.withResolvers<Blob | null>();
  canvas.toBlob(pending.resolve, 'image/png');
  const blob = await pending.promise;
  return blob ? URL.createObjectURL(blob) : null;
}

/** Filters the effect area of `image` and returns one patch per op; `cancelled` is polled between encodes. */
export async function renderEffectPatches(
  image: CanvasImageSource,
  size: ImageSize,
  ops: readonly EffectOp[],
  redactions: readonly PixelRegion[],
  cancelled: () => boolean,
): Promise<EffectPatch[]> {
  if (ops.length === 0) return [];
  const bx = Math.min(...ops.map((o) => o.region.x0));
  const by = Math.min(...ops.map((o) => o.region.y0));
  const bw = Math.max(...ops.map((o) => o.region.x1)) - bx;
  const bh = Math.max(...ops.map((o) => o.region.y1)) - by;
  const work = document.createElement('canvas');
  work.width = bw;
  work.height = bh;
  const ctx = work.getContext('2d', { willReadFrequently: true });
  if (!ctx) return [];
  const src = sourceSize(image, size);
  const kx = src.width / size.width;
  const ky = src.height / size.height;
  ctx.drawImage(image, bx * kx, by * ky, bw * kx, bh * ky, 0, 0, bw, bh);
  applyBaseEffects(ctx, { x: bx, y: by }, ops, redactions);
  const out: EffectPatch[] = [];
  for (const op of ops) {
    if (cancelled()) break;
    const w = regionWidth(op.region);
    const h = regionHeight(op.region);
    const piece = document.createElement('canvas');
    piece.width = w;
    piece.height = h;
    const pctx = piece.getContext('2d');
    if (!pctx) continue;
    pctx.drawImage(work, op.region.x0 - bx, op.region.y0 - by, w, h, 0, 0, w, h);
    const url = await canvasToUrl(piece);
    if (url) out.push({ id: op.id, kind: op.kind, region: op.region, url });
  }
  return out;
}

/**
 * Filtered patches for every blur/pixelate annotation in `annotations`
 * (include an in-progress draft in the list to see it live). Patches may lag
 * one frame behind a fast drag; stale ones are replaced, never accumulated.
 */
export function useEffectPatches(image: CanvasImageSource | null | undefined, size: ImageSize, annotations: readonly Annotation[]): EffectPatches {
  const [patches, setPatches] = useState<EffectPatches>(NO_PATCHES);
  const signature = image ? effectSignature(annotations, size) : '';

  useEffect(() => {
    if (!image || signature === '') {
      setPatches(NO_PATCHES);
      return undefined;
    }
    let cancelled = false;
    const ops = collectEffectOps(annotations, size);
    const redactions = collectRedactionRegions(annotations, size);
    const frame = window.requestAnimationFrame(() => {
      renderEffectPatches(image, size, ops, redactions, () => cancelled)
        .then((list) => {
          if (cancelled) {
            for (const p of list) URL.revokeObjectURL(p.url);
            return;
          }
          setPatches(new Map(list.map((p) => [p.id, p])));
        })
        .catch(() => {
          if (!cancelled) setPatches(NO_PATCHES);
        });
    });
    return () => {
      cancelled = true;
      window.cancelAnimationFrame(frame);
    };
    // `signature` encodes every pixel-relevant input of annotations and size.
  }, [image, signature]);

  // Each replaced (or unmounted) set releases its object URLs.
  useEffect(
    () => () => {
      for (const p of patches.values()) URL.revokeObjectURL(p.url);
    },
    [patches],
  );

  return patches;
}
