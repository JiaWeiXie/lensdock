import { describe, expect, it } from 'vitest';
import { codePointLength, graphemeLength, truncateToCodePoints, utf8ByteLength } from './text';

describe('grapheme handling', () => {
  // A family emoji (ZWJ sequence) and a flag (regional indicator pair) each count
  // as one user-visible character, but several code points.
  const family = '\u{1F468}\u200D\u{1F469}\u200D\u{1F467}';
  const flag = '\u{1F1F9}\u{1F1FC}';

  it('counts graphemes, not code units', () => {
    expect(graphemeLength(family)).toBe(1);
    expect(graphemeLength(flag)).toBe(1);
    expect(graphemeLength('繁體中文')).toBe(4);
  });

  it('counts code points like JSON Schema maxLength', () => {
    expect(codePointLength(family)).toBe(5);
    expect(codePointLength('繁體中文')).toBe(4);
  });

  it('never splits a grapheme cluster when truncating', () => {
    const text = `a${family}b`;
    // The family needs 5 code points, so a 4 code point budget keeps only 'a'.
    expect(truncateToCodePoints(text, 4)).toBe('a');
    expect(truncateToCodePoints(text, 6)).toBe(`a${family}`);
    expect(truncateToCodePoints(text, 100)).toBe(text);
  });

  it('measures UTF-8 bytes', () => {
    expect(utf8ByteLength('abc')).toBe(3);
    expect(utf8ByteLength('中')).toBe(3);
    expect(utf8ByteLength('\u{1F600}')).toBe(4);
  });
});
