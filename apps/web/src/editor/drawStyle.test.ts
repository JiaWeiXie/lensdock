import { describe, expect, it } from 'vitest';
import type { Annotation } from '@/api/types';
import { INITIAL_DEFAULTS, applyPatchToDefaults, arrowAnnotation, capsOfKind, capsOfTool, newStyle, rectAnnotation, valuesOf, withStyle } from './drawStyle';
import { defaultArrowControl } from './model';

const arrow = (extra: Partial<Extract<Annotation, { kind: 'arrow' }>> = {}): Annotation => ({
  id: 'a',
  kind: 'arrow',
  geometry: { start: { x: 0.1, y: 0.1 }, end: { x: 0.6, y: 0.4 } },
  comment: '',
  style: { stroke: '#e5362b', strokeWidthPx: 4 },
  ...extra,
});

describe('new object styles', () => {
  it('keeps the default arrow and text legacy-shaped so old documents keep their schema version', () => {
    expect(newStyle('arrow', INITIAL_DEFAULTS)).toEqual({ stroke: '#e5362b', strokeWidthPx: 4 });
    expect(newStyle('text', INITIAL_DEFAULTS)).toEqual({ stroke: '#e5362b', fontSizePx: 28 });
    const a = arrowAnnotation('x', { x: 0, y: 0 }, { x: 1, y: 1 }, INITIAL_DEFAULTS);
    expect(a.kind === 'arrow' && a.geometry.control).toBeUndefined();
  });

  it('saves every visible non-default choice explicitly', () => {
    const d = { ...INITIAL_DEFAULTS, arrow: 'curved' as const, text: 'mono' as const, smoothing: false };
    expect(newStyle('arrow', d)).toMatchObject({ arrowStyle: 'curved' });
    expect(newStyle('text', d)).toMatchObject({ textStyle: 'mono' });
    expect(newStyle('freehand', d)).not.toHaveProperty('smoothing');
    expect(newStyle('freehand', INITIAL_DEFAULTS)).toMatchObject({ smoothing: true });
    const curved = arrowAnnotation('x', { x: 0.1, y: 0.1 }, { x: 0.9, y: 0.3 }, d);
    expect(curved.kind === 'arrow' && curved.geometry.control).toEqual(defaultArrowControl({ x: 0.1, y: 0.1 }, { x: 0.9, y: 0.3 }));
  });

  it('gives the filled rectangle preset a fill equal to its colour and spotlight no style', () => {
    const d = { ...INITIAL_DEFAULTS, color: '#2f6fed', fill: '#ffffff' };
    expect(newStyle('filledRect', d)).toMatchObject({ stroke: '#2f6fed', fill: '#2f6fed' });
    const s = rectAnnotation('spotlight', 's', { x: 0, y: 0, width: 0.5, height: 0.5 }, d);
    expect(s.kind).toBe('spotlight');
    expect('style' in s).toBe(false);
    const f = rectAnnotation('filledRect', 'f', { x: 0, y: 0, width: 0.5, height: 0.5 }, d);
    expect(f.kind).toBe('rectangle');
  });

  it('uses the highlighter yellow and the per-kind effect sizes', () => {
    expect(newStyle('highlight', INITIAL_DEFAULTS)).toEqual({ stroke: '#f2a900', opacity: 0.35 });
    expect(newStyle('blur', INITIAL_DEFAULTS)).toEqual({ effectSizePx: 12 });
    expect(newStyle('pixelate', INITIAL_DEFAULTS)).toEqual({ effectSizePx: 16 });
  });
});

describe('style patches', () => {
  it('materializes a control point when an arrow becomes curved and keeps it when switching back', () => {
    const curved = withStyle(arrow(), { arrowStyle: 'curved' });
    expect(curved.kind === 'arrow' && curved.geometry.control).toEqual(defaultArrowControl({ x: 0.1, y: 0.1 }, { x: 0.6, y: 0.4 }));
    expect(curved.style?.arrowStyle).toBe('curved');
    const back = withStyle(curved, { arrowStyle: 'filled' });
    expect(back.style).not.toHaveProperty('arrowStyle');
    expect(back.kind === 'arrow' && back.geometry.control).toBeDefined();
  });

  it('removes values equal to the legacy default instead of saving them', () => {
    const text: Annotation = { id: 't', kind: 'text', geometry: { position: { x: 0.1, y: 0.1 }, text: 'x' }, comment: '', style: { stroke: '#e5362b', fontSizePx: 20, textStyle: 'bold' } };
    expect(withStyle(text, { textStyle: 'outline' }).style).not.toHaveProperty('textStyle');
    const pen: Annotation = { id: 'p', kind: 'freehand', geometry: { points: [{ x: 0, y: 0 }, { x: 1, y: 1 }] }, comment: '', style: { smoothing: true } };
    expect(withStyle(pen, { smoothing: false }).style).not.toHaveProperty('smoothing');
  });

  it('applies only the fields that belong to the kind and clamps ranges', () => {
    const blur: Annotation = { id: 'b', kind: 'blur', geometry: { x: 0, y: 0, width: 0.2, height: 0.2 }, comment: '', style: { effectSizePx: 12 } };
    const next = withStyle(blur, { stroke: '#000000', strokeWidthPx: 9, effectSizePx: 500, opacity: 0.5 });
    expect(next.style).toEqual({ effectSizePx: 64 });
    const hl: Annotation = { id: 'h', kind: 'highlight', geometry: { x: 0, y: 0, width: 0.2, height: 0.2 }, comment: '' };
    expect(valuesOf(hl).color).toBe('#f2a900');
    expect(withStyle(hl, { opacity: 0.01 }).style?.opacity).toBe(0.1);
    const red: Annotation = { id: 'r', kind: 'redact', mode: 'solid', geometry: { x: 0, y: 0, width: 0.2, height: 0.2 }, comment: '' };
    expect(withStyle(red, { stroke: '#ffffff' })).toBe(red);
  });

  it('remembers the patch as the default for the same kind family only', () => {
    expect(applyPatchToDefaults(INITIAL_DEFAULTS, { stroke: '#111111' }, 'highlight')).toMatchObject({ highlightColor: '#111111', color: '#e5362b' });
    expect(applyPatchToDefaults(INITIAL_DEFAULTS, { effectSizePx: 30 }, 'pixelate')).toMatchObject({ pixelSize: 30, blurSize: 12 });
  });

  it('shows only controls that can change the saved result', () => {
    expect(capsOfKind('redact')).toEqual(capsOfKind('spotlight'));
    expect(capsOfKind('blur').color).toBe(false);
    expect(capsOfKind('arrow').arrow).toBe(true);
    expect(capsOfTool('filledRect').fill).toBe(false);
    expect(capsOfTool('rectangle').fill).toBe(true);
  });
});
