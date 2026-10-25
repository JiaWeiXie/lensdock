import type { ReactNode } from 'react';
import type { Annotation, NormRect, Point } from '@/api/types';
import { useEffectPatches, type EffectPatches } from './effectPatches';
import { spotlightDimPath } from './effects';
import {
  HANDLES,
  REDACT_FILL,
  SPOTLIGHT_DIM,
  arrowControlPoint,
  boundsOf,
  handlePosition,
  redactionsLast,
  strokeOf,
  strokeWidthOf,
  toPx,
  type ImageSize,
} from './model';
import { pathData } from './paths';
import { selectionBounds } from './selection';
import { FONT_STACK, MITER_LIMIT, PIN_RING, arrowPrimitive, freehandPath, highlightPrimitive, measureTextWidth, pinNumberColor, pinParts, shapePrimitive, textParts } from './shapes';
import { useTextFonts } from './useTextFonts';

/** How the selection is transformed: bounding-box handles, two endpoint handles, or none. */
export type HandleMode = 'box' | 'endpoints' | 'none';

export function handleMode(selected: readonly Annotation[]): HandleMode {
  const only = selected.length === 1 ? selected[0] : undefined;
  if (only && (only.kind === 'arrow' || only.kind === 'line')) return 'endpoints';
  return selected.length > 0 ? 'box' : 'none';
}

/**
 * One annotation as SVG. Mirrors editor/raster.ts so the editing view matches the PNG export.
 * Base-layer effects (spotlight, blur, pixelate) draw nothing here: the layer paints them
 * underneath everything else from the shared pixel pipeline.
 */
export function AnnotationShape({ a, size, opacity = 1 }: { a: Annotation; size: ImageSize; opacity?: number }): ReactNode {
  switch (a.kind) {
    case 'spotlight':
    case 'blur':
    case 'pixelate':
      return null;
    case 'redact': {
      const g = a.geometry;
      return <rect x={g.x * size.width} y={g.y * size.height} width={g.width * size.width} height={g.height * size.height} fill={REDACT_FILL} opacity={opacity} />;
    }
    case 'highlight': {
      const p = highlightPrimitive(a, size);
      return <rect x={p.x} y={p.y} width={p.width} height={p.height} fill={p.fill} fillOpacity={p.opacity} opacity={opacity} />;
    }
    case 'rectangle':
    case 'ellipse':
    case 'diamond':
    case 'line': {
      const s = shapePrimitive(a, size);
      const paint = { stroke: s.stroke, strokeWidth: s.strokeWidth, opacity };
      switch (s.type) {
        case 'rect':
          return <rect x={s.x} y={s.y} width={s.width} height={s.height} fill={s.fill ?? 'none'} strokeLinejoin="miter" strokeMiterlimit={MITER_LIMIT} {...paint} />;
        case 'ellipse':
          return <ellipse cx={s.cx} cy={s.cy} rx={s.rx} ry={s.ry} fill={s.fill ?? 'none'} {...paint} />;
        case 'polygon':
          return <polygon points={s.points.map((q) => `${q.x},${q.y}`).join(' ')} fill={s.fill ?? 'none'} strokeLinejoin="miter" strokeMiterlimit={MITER_LIMIT} {...paint} />;
        case 'line':
          return <line x1={s.from.x} y1={s.from.y} x2={s.to.x} y2={s.to.y} fill="none" strokeLinecap="round" {...paint} />;
      }
    }
    case 'arrow': {
      const p = arrowPrimitive(a, size);
      return (
        <g opacity={opacity}>
          {p.shaft.length > 0 ? <path d={pathData(p.shaft)} fill="none" stroke={p.stroke} strokeWidth={p.width} strokeLinecap="round" strokeLinejoin="round" /> : null}
          {p.filledHeads.map((h, i) => (
            // Heads are a fixed, ordered pair (end, then start); index is their identity.
            <path key={i} d={pathData(h)} fill={p.stroke} />
          ))}
          {p.openHeads.map((h, i) => (
            <path key={i} d={pathData(h)} fill="none" stroke={p.stroke} strokeWidth={p.width} strokeLinecap="round" strokeLinejoin="round" />
          ))}
        </g>
      );
    }
    case 'freehand':
      return <path d={pathData(freehandPath(a, size))} fill="none" stroke={strokeOf(a)} strokeWidth={strokeWidthOf(a)} strokeLinecap="round" strokeLinejoin="round" opacity={opacity} />;
    case 'text': {
      const p = textParts(a, size);
      const block = (dx: number, dy: number, extra: { fill: string; fillOpacity?: number; stroke?: string; strokeWidth?: number; paintOrder?: 'stroke' }): ReactNode => (
        <text x={p.x + dx} fontFamily={p.family} fontWeight={p.weight} fontSize={p.fontSize} strokeLinejoin="round" style={{ whiteSpace: 'pre' }} {...extra}>
          {p.lines.map((l, i) => (
            // Line index is the identity of a line within one text block.
            <tspan key={i} x={p.x + dx} y={l.y + dy}>
              {l.text.length === 0 ? ' ' : l.text}
            </tspan>
          ))}
        </text>
      );
      return (
        <g opacity={opacity}>
          {p.backdrop ? <path d={pathData(p.backdrop.path)} fill={p.backdrop.fill} /> : null}
          {p.shadow ? block(p.shadow.dx, p.shadow.dy, { fill: p.shadow.color, fillOpacity: p.shadow.opacity }) : null}
          {block(0, 0, p.halo ? { fill: p.fill, stroke: p.halo.color, strokeWidth: p.halo.width, paintOrder: 'stroke' } : { fill: p.fill })}
          {p.underlines.map((u, i) => (
            <path key={i} d={pathData(u.path)} fill="none" stroke={p.fill} strokeWidth={u.width} />
          ))}
        </g>
      );
    }
    case 'pin': {
      const p = pinParts(a, size);
      return (
        <g opacity={opacity}>
          <circle cx={p.cx} cy={p.cy} r={p.r} fill={p.fill} stroke={PIN_RING} strokeWidth={Math.max(2, p.r / 7)} />
          <text x={p.cx} y={p.numberY} textAnchor="middle" fontFamily={FONT_STACK} fontWeight={600} fontSize={p.fontSize} fill={pinNumberColor(p.fill)}>
            {a.geometry.number}
          </text>
        </g>
      );
    }
  }
}

export interface LayerProps {
  annotations: readonly Annotation[];
  size: ImageSize;
  /** CSS pixels per image pixel, used to keep chrome (handles, outlines) a constant on-screen size. */
  zoom: number;
  selectedIds: ReadonlySet<string>;
  /** Annotation hidden because its text is being edited in place. */
  hiddenId: string | null;
  draft: Annotation | null;
  crop: NormRect | null;
  /** Rubber-band rectangle of an in-progress marquee selection. */
  marquee: NormRect | null;
  /** Base image pixels (pass the editor bitmap); needed to show real blur/pixelate while editing. */
  image?: CanvasImageSource | null;
}

function isEffect(a: Annotation): boolean {
  return a.kind === 'blur' || a.kind === 'pixelate';
}

/** Filtered base pixels of one effect rectangle, painted under every other annotation. */
function EffectPatch({ id, patches, zoom }: { id: string; patches: EffectPatches; zoom: number }): ReactNode {
  const patch = patches.get(id);
  if (!patch) return null;
  const { region } = patch;
  return (
    <image
      href={patch.url}
      x={region.x0}
      y={region.y0}
      width={region.x1 - region.x0}
      height={region.y1 - region.y0}
      preserveAspectRatio="none"
      // Blocks stay crisp when zoomed in; zoomed out the browser's normal downscale applies.
      style={patch.kind === 'pixelate' && zoom >= 1 ? { imageRendering: 'pixelated' } : undefined}
    />
  );
}

/** SVG editing layer. Chrome (selection, handles, marquee, crop) is editor-only and never exported. */
export function AnnotationLayer({ annotations, size, zoom, selectedIds, hiddenId, draft, crop, marquee, image }: LayerProps): ReactNode {
  const px = 1 / Math.max(zoom, 0.01);
  // Text measured before its face loaded is re-measured once the bundled fonts are ready.
  useTextFonts(annotations);
  const withDraft = draft ? [...annotations, draft] : annotations;
  const patches = useEffectPatches(image, size, withDraft);
  const dimPath = spotlightDimPath(withDraft, size);
  const ordered = redactionsLast(annotations);
  const selected = annotations.filter((a) => selectedIds.has(a.id) && a.id !== hiddenId);
  const sel = selected.length > 0 ? selectionBounds(selected, size, measureTextWidth) : null;
  const mode = handleMode(selected);
  const only = selected.length === 1 ? selected[0] : undefined;
  const control = only && only.kind === 'arrow' ? arrowControlPoint(only) : null;

  return (
    <svg className="annlayer" viewBox={`0 0 ${size.width} ${size.height}`} width="100%" height="100%" preserveAspectRatio="none" aria-hidden="true" focusable="false">
      {ordered.filter(isEffect).map((a) => (
        <g key={a.id} data-annotation={a.id}>
          <EffectPatch id={a.id} patches={patches} zoom={zoom} />
        </g>
      ))}
      {draft && isEffect(draft) ? <EffectPatch id={draft.id} patches={patches} zoom={zoom} /> : null}
      {dimPath.length > 0 ? <path className="annlayer__dim" d={pathData(dimPath)} fill="#000000" fillOpacity={SPOTLIGHT_DIM} /> : null}
      {ordered
        .filter((a) => !isEffect(a))
        .map((a) =>
          a.id === hiddenId ? null : (
            <g key={a.id} data-annotation={a.id}>
              <AnnotationShape a={a} size={size} />
              {a.kind === 'redact' ? (
                <rect
                  x={a.geometry.x * size.width}
                  y={a.geometry.y * size.height}
                  width={a.geometry.width * size.width}
                  height={a.geometry.height * size.height}
                  fill="none"
                  stroke="#ffffff"
                  strokeWidth={1.5 * px}
                  strokeDasharray={`${6 * px} ${4 * px}`}
                />
              ) : null}
            </g>
          ),
        )}
      {draft && !isEffect(draft) ? <AnnotationShape a={draft} size={size} opacity={0.85} /> : null}

      {sel ? (
        <g className="annlayer__sel">
          {selected.length > 1
            ? selected.map((a) => {
                const b = boundsOf(a, size, measureTextWidth);
                return (
                  <rect
                    key={a.id}
                    x={b.x * size.width - 2 * px}
                    y={b.y * size.height - 2 * px}
                    width={b.width * size.width + 4 * px}
                    height={b.height * size.height + 4 * px}
                    fill="none"
                    stroke="#2ec4b6"
                    strokeWidth={1 * px}
                  />
                );
              })
            : null}
          <rect
            x={sel.x * size.width - 3 * px}
            y={sel.y * size.height - 3 * px}
            width={sel.width * size.width + 6 * px}
            height={sel.height * size.height + 6 * px}
            fill="none"
            stroke="#2ec4b6"
            strokeWidth={1.5 * px}
            strokeDasharray={`${5 * px} ${3 * px}`}
          />
          {mode === 'box'
            ? HANDLES.map((h) => {
                const pos = toPx(handlePosition(sel, h), size);
                return <rect key={h} x={pos.x - 4.5 * px} y={pos.y - 4.5 * px} width={9 * px} height={9 * px} fill="#ffffff" stroke="#0b8a84" strokeWidth={1.5 * px} />;
              })
            : null}
          {mode === 'endpoints' && only && (only.kind === 'arrow' || only.kind === 'line') ? (
            <>
              {control && only.kind === 'arrow' ? <CurveGuide start={only.geometry.start} control={control} end={only.geometry.end} size={size} px={px} /> : null}
              {([only.geometry.start, only.geometry.end] as Point[]).map((pt, i) => {
                const pos = toPx(pt, size);
                // Two fixed endpoints; index is their identity.
                return <circle key={i} cx={pos.x} cy={pos.y} r={5.5 * px} fill="#ffffff" stroke="#0b8a84" strokeWidth={1.5 * px} />;
              })}
              {control ? <circle data-handle="curve" cx={toPx(control, size).x} cy={toPx(control, size).y} r={5.5 * px} fill="#0b8a84" stroke="#ffffff" strokeWidth={1.5 * px} /> : null}
            </>
          ) : null}
        </g>
      ) : null}

      {marquee ? (
        <rect
          className="annlayer__marquee"
          x={marquee.x * size.width}
          y={marquee.y * size.height}
          width={marquee.width * size.width}
          height={marquee.height * size.height}
          fill="rgb(46 196 182 / 14%)"
          stroke="#2ec4b6"
          strokeWidth={1.5 * px}
        />
      ) : null}

      {crop ? <CropOverlay crop={crop} size={size} px={px} /> : null}
    </svg>
  );
}

/** Dashed guide from both endpoints to the curve control (editor-only chrome). */
function CurveGuide({ start, control, end, size, px }: { start: Point; control: Point; end: Point; size: ImageSize; px: number }): ReactNode {
  const s = toPx(start, size);
  const c = toPx(control, size);
  const e = toPx(end, size);
  return <polyline points={`${s.x},${s.y} ${c.x},${c.y} ${e.x},${e.y}`} fill="none" stroke="#2ec4b6" strokeWidth={1 * px} strokeDasharray={`${4 * px} ${3 * px}`} />;
}

function CropOverlay({ crop, size, px }: { crop: NormRect; size: ImageSize; px: number }): ReactNode {
  const x = crop.x * size.width;
  const y = crop.y * size.height;
  const w = crop.width * size.width;
  const h = crop.height * size.height;
  const outer = `M0 0H${size.width}V${size.height}H0Z`;
  const inner = `M${x} ${y}H${x + w}V${y + h}H${x}Z`;
  return (
    <g className="annlayer__crop">
      <path d={`${outer} ${inner}`} fillRule="evenodd" fill="rgb(11 37 64 / 55%)" />
      <rect x={x} y={y} width={w} height={h} fill="none" stroke="#f2b84b" strokeWidth={2 * px} />
      {HANDLES.map((hd) => {
        const pos = toPx(handlePosition(crop, hd), size);
        return <rect key={hd} x={pos.x - 5 * px} y={pos.y - 5 * px} width={10 * px} height={10 * px} fill="#f2b84b" stroke="#0b2540" strokeWidth={1.5 * px} />;
      })}
    </g>
  );
}
