import { describe, expect, it } from 'vitest';
import { canRedo, canUndo, commit, createHistory, redo, undo } from './history';

describe('history', () => {
  it('undoes and redoes a single change', () => {
    let h = createHistory('a');
    h = commit(h, 'b');
    expect(h.present).toBe('b');
    expect(canUndo(h)).toBe(true);
    h = undo(h);
    expect(h.present).toBe('a');
    expect(canRedo(h)).toBe(true);
    h = redo(h);
    expect(h.present).toBe('b');
  });

  it('drops the future after a new edit', () => {
    let h = createHistory(1);
    h = commit(h, 2);
    h = undo(h);
    h = commit(h, 3);
    expect(h.present).toBe(3);
    expect(canRedo(h)).toBe(false);
  });

  it('coalesces rapid edits with the same key', () => {
    let h = createHistory('');
    h = commit(h, 'a', { coalesceKey: 'comment', now: 1000 });
    h = commit(h, 'ab', { coalesceKey: 'comment', now: 1200 });
    h = commit(h, 'abc', { coalesceKey: 'comment', now: 1400 });
    expect(h.past).toHaveLength(1);
    h = undo(h);
    expect(h.present).toBe('');
  });

  it('does not coalesce different keys or late edits', () => {
    let h = createHistory('');
    h = commit(h, 'a', { coalesceKey: 'comment', now: 1000 });
    h = commit(h, 'ab', { coalesceKey: 'other', now: 1100 });
    expect(h.past).toHaveLength(2);
    h = commit(h, 'abc', { coalesceKey: 'other', now: 9000 });
    expect(h.past).toHaveLength(3);
  });

  it('ignores identical states and empty undo', () => {
    const h = createHistory('x');
    expect(commit(h, 'x')).toBe(h);
    expect(undo(h)).toBe(h);
    expect(redo(h)).toBe(h);
  });

  it('bounds the past to the limit', () => {
    let h = createHistory(0);
    for (let i = 1; i <= 20; i += 1) h = commit(h, i, { limit: 5 });
    expect(h.past.length).toBe(5);
  });
});
