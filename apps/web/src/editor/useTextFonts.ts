import { useEffect, useState } from 'react';
import type { Annotation } from '@/api/types';
import { fontLoadJobs } from './raster';

/**
 * Starts loading the bundled faces that the text annotations use (canvas
 * measuring does not trigger a font load by itself) and returns a counter that
 * changes whenever fonts finish loading. Pass the counter to a memo that
 * measures text, or just render with it: text bounds measured before a face
 * was ready are re-measured once it is, so selection boxes, hit areas and the
 * label backdrop match the exported glyph widths.
 */
export function useTextFonts(annotations: readonly Annotation[]): number {
  const [epoch, setEpoch] = useState(0);
  const key = annotations.flatMap((a) => (a.kind === 'text' ? [`${a.style?.textStyle ?? ''}\u0000${a.geometry.text}`] : [])).join('\u0001');

  useEffect(() => {
    if (key === '' || typeof document === 'undefined' || !document.fonts) return undefined;
    let live = true;
    const bump = (): void => {
      if (live) setEpoch((n) => n + 1);
    };
    void Promise.allSettled(fontLoadJobs(annotations)).then(bump);
    document.fonts.addEventListener('loadingdone', bump);
    return () => {
      live = false;
      document.fonts.removeEventListener('loadingdone', bump);
    };
    // `key` covers the text and style of every text annotation, which is all the font jobs depend on.
  }, [key]);

  return epoch;
}
