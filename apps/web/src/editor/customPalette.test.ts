import { describe, expect, it } from 'vitest';
import { MAX_CUSTOM_COLORS, addColor, parsePalette, removeColor } from './customPalette';
import { PALETTE } from './model';

describe('custom palette', () => {
  it('keeps only valid, unique, non-built-in hex colours from storage', () => {
    const raw = JSON.stringify(['#AABBCC', '#aabbcc', 'red', '#12345', '#123456', 7, null, PALETTE[0], '#00ff00ff']);
    expect(parsePalette(raw)).toEqual(['#aabbcc', '#123456']);
  });

  it('survives corrupt or oversized storage', () => {
    expect(parsePalette(null)).toEqual([]);
    expect(parsePalette('{not json')).toEqual([]);
    expect(parsePalette('{"a":1}')).toEqual([]);
    expect(parsePalette(JSON.stringify(['#aabbcc']) + ' '.repeat(5000))).toEqual([]);
  });

  it('caps the stored palette', () => {
    const many = Array.from({ length: 40 }, (_, i) => `#${(i + 1).toString(16).padStart(6, '0')}`);
    expect(parsePalette(JSON.stringify(many))).toHaveLength(MAX_CUSTOM_COLORS);
  });

  it('adds newest first and refuses duplicates, built-ins, bad values and overflow', () => {
    let list: readonly string[] = [];
    list = addColor(list, '#AA0000');
    list = addColor(list, '#00bb00');
    expect(list).toEqual(['#00bb00', '#aa0000']);
    expect(addColor(list, '#AA0000')).toBe(list);
    expect(addColor(list, PALETTE[0] ?? '#e5362b')).toBe(list);
    expect(addColor(list, 'javascript:1')).toBe(list);
    const full = Array.from({ length: MAX_CUSTOM_COLORS }, (_, i) => `#${(i + 1).toString(16).padStart(6, '0')}`);
    expect(addColor(full, '#abcdef')).toBe(full);
  });

  it('removes a colour case-insensitively and leaves the list alone when absent', () => {
    const list = ['#00bb00', '#aa0000'];
    expect(removeColor(list, '#AA0000')).toEqual(['#00bb00']);
    expect(removeColor(list, '#111111')).toBe(list);
  });
});
