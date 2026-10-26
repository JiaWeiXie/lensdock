import { describe, expect, it } from 'vitest';
import {
  ENDED_GUARD,
  OPEN_GUARD,
  decideIntent,
  isDeliberateKey,
  type IntentInput,
} from './controlIntent';

const base: IntentInput = {
  kind: 'pointer',
  guard: OPEN_GUARD,
  blocked: null,
  holdsLease: false,
  inFlight: false,
  controlState: 'IDLE',
  holderIsSelf: false,
};

const decide = (patch: Partial<IntentInput>) => decideIntent({ ...base, ...patch });

describe('decideIntent', () => {
  it('acquires directly from an idle workspace and takes over a holder through the graceful handshake', () => {
    expect(decide({}).decision).toEqual({ action: 'acquire' });
    expect(decide({ controlState: 'AGENT' }).decision).toEqual({ action: 'takeover' });
    expect(decide({ controlState: 'HUMAN' }).decision).toEqual({ action: 'takeover' });
  });

  it('never requests anything while this session already holds the lease or a handshake is running', () => {
    expect(decide({ holdsLease: true, controlState: 'HUMAN', holderIsSelf: true }).decision.action).toBe('ignore');
    expect(decide({ inFlight: true }).decision.action).toBe('ignore');
    expect(decide({ inFlight: true, controlState: 'AGENT' }).decision.action).toBe('ignore');
  });

  it('reports why control is blocked instead of requesting it', () => {
    expect(decide({ blocked: 'control.blocked.role' }).decision).toEqual({ action: 'blocked', reason: 'control.blocked.role' });
    expect(decide({ blocked: 'control.blocked.maintenance', controlState: 'MAINTENANCE' }).decision).toEqual({
      action: 'blocked',
      reason: 'control.blocked.maintenance',
    });
  });

  it('leaves another session\'s pending handoff alone', () => {
    expect(decide({ controlState: 'HANDOFF_PENDING' }).decision).toEqual({ action: 'blocked', reason: 'control.blocked.handoff' });
  });

  it('re-adopts its own HUMAN lease instead of taking over from itself', () => {
    expect(decide({ controlState: 'HUMAN', holderIsSelf: true }).decision).toEqual({ action: 'reconcile' });
  });

  it('accepts a fresh click immediately after control ended, with no cooldown', () => {
    const click = decide({ kind: 'pointer', guard: ENDED_GUARD });
    expect(click.decision).toEqual({ action: 'acquire' });
    expect(click.guard).toEqual(OPEN_GUARD);
    expect(decide({ kind: 'pointer', guard: ENDED_GUARD, controlState: 'AGENT' }).decision).toEqual({ action: 'takeover' });
  });

  it('ignores a click while the lease is still held (release not completed) or a handshake is running', () => {
    expect(decide({ kind: 'pointer', guard: ENDED_GUARD, holdsLease: true, controlState: 'HUMAN' }).decision.action).toBe('ignore');
    expect(decide({ kind: 'pointer', guard: ENDED_GUARD, inFlight: true }).decision.action).toBe('ignore');
  });

  it('keeps typing from taking control back after control ended until a click re-arms it', () => {
    const typed = decide({ kind: 'key', guard: ENDED_GUARD });
    expect(typed.decision.action).toBe('ignore');
    expect(typed.guard).toEqual(ENDED_GUARD);

    const clicked = decide({ kind: 'pointer', guard: typed.guard });
    expect(clicked.decision.action).toBe('acquire');
    expect(decide({ kind: 'key', guard: clicked.guard }).decision.action).toBe('acquire');
  });

  it('lets a first keystroke on a fresh page request control', () => {
    expect(decide({ kind: 'key' }).decision).toEqual({ action: 'acquire' });
  });
});

describe('isDeliberateKey', () => {
  const key = (k: string, patch: Partial<Parameters<typeof isDeliberateKey>[0]> = {}) =>
    isDeliberateKey({ key: k, repeat: false, isComposing: false, ctrlKey: false, metaKey: false, altKey: false, ...patch });

  it('accepts typing and editing keys', () => {
    expect(key('a')).toBe(true);
    expect(key(' ')).toBe(true);
    expect(key('Enter')).toBe(true);
    expect(key('ArrowLeft')).toBe(true);
  });

  it('rejects repeats, IME composition, shortcut chords, navigation and bare modifiers', () => {
    expect(key('a', { repeat: true })).toBe(false);
    expect(key('a', { isComposing: true })).toBe(false);
    expect(key('r', { ctrlKey: true })).toBe(false);
    expect(key('k', { metaKey: true })).toBe(false);
    expect(key('K', { altKey: true })).toBe(false);
    expect(key('Tab')).toBe(false);
    expect(key('Escape')).toBe(false);
    expect(key('Shift')).toBe(false);
    expect(key('F5')).toBe(false);
  });
});
