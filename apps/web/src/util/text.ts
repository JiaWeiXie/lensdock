/**
 * Text helpers. The server counts JSON string length in Unicode code points
 * (JSON Schema maxLength), while people edit grapheme clusters, so limits are
 * enforced in code points but never cut a cluster in half (ZWJ emoji, skin
 * tones, flags, variation selectors).
 */

const segmenter: Intl.Segmenter | null =
  typeof Intl !== 'undefined' && 'Segmenter' in Intl ? new Intl.Segmenter(undefined, { granularity: 'grapheme' }) : null;

/** Splits text into user-perceived characters. */
export function graphemes(text: string): string[] {
  if (segmenter === null) return Array.from(text);
  const out: string[] = [];
  for (const part of segmenter.segment(text)) out.push(part.segment);
  return out;
}

/** Counts Unicode code points (the unit of JSON Schema maxLength). */
export function codePointLength(text: string): number {
  let n = 0;
  for (const _ of text) n += 1;
  return n;
}

/** Counts grapheme clusters, the unit users see in counters. */
export function graphemeLength(text: string): number {
  return graphemes(text).length;
}

/**
 * Truncates to at most `limit` code points without splitting a grapheme
 * cluster. Returns the text unchanged when it already fits.
 */
export function truncateToCodePoints(text: string, limit: number): string {
  if (codePointLength(text) <= limit) return text;
  let used = 0;
  let out = '';
  for (const g of graphemes(text)) {
    const n = codePointLength(g);
    if (used + n > limit) break;
    used += n;
    out += g;
  }
  return out;
}

/** UTF-8 byte length without allocating a copy. */
export function utf8ByteLength(text: string): number {
  let n = 0;
  for (const ch of text) {
    const cp = ch.codePointAt(0) ?? 0;
    n += cp < 0x80 ? 1 : cp < 0x800 ? 2 : cp < 0x10000 ? 3 : 4;
  }
  return n;
}
