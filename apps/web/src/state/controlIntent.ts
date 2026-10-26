import type { ControlState } from '@/api/types';
import type { MessageKey } from '@/i18n';

/**
 * Policy for turning a deliberate gesture on the live desktop into a control request.
 *
 * The gesture itself is never replayed: the stream that saw it is a view-only one that is
 * replaced once control is granted, so the first click only activates control. Everything here is
 * pure so the transitions can be exercised without a browser.
 */

export type IntentKind = 'pointer' | 'key';

export interface IntentGuard {
  /** Typing stays ignored until a fresh pointer gesture re-arms it, so keystrokes in flight never reclaim control. */
  needsPointer: boolean;
}

/** Fresh page: nothing has ended, but nothing acquires without a gesture either. */
export const OPEN_GUARD: IntentGuard = { needsPointer: false };

/** Guard to install when control ends for any reason: release, revocation, loss or a cancelled takeover. */
export const ENDED_GUARD: IntentGuard = { needsPointer: true };

export interface IntentInput {
  kind: IntentKind;
  guard: IntentGuard;
  /** Why control cannot be taken at all (role, maintenance, lock, capture, readiness), or null. */
  blocked: MessageKey | null;
  holdsLease: boolean;
  /** An acquisition or takeover handshake is running, or its status is still waiting on the user. */
  inFlight: boolean;
  controlState: ControlState | null;
  /** The runtime attributes the current HUMAN lease to this session (e.g. after a reload). */
  holderIsSelf: boolean;
}

export type IntentDecision =
  | { action: 'ignore' }
  | { action: 'blocked'; reason: MessageKey }
  | { action: 'reconcile' }
  | { action: 'acquire' }
  | { action: 'takeover' };

export interface IntentOutcome {
  decision: IntentDecision;
  guard: IntentGuard;
}

/**
 * Decides what a gesture does. Viewers, maintenance, locks, a running capture and a foreign
 * handoff block; a held lease and a running handshake ignore. A pointer-down is always a fresh
 * gesture once the lease is gone: release, cancel and the takeover dialog live outside the desktop
 * surface, so their own events can never arrive here. Typing waits for a pointer-down after
 * control ended. A gesture never forces: a drain timeout is surfaced to the user, who alone can force.
 */
export function decideIntent(input: IntentInput): IntentOutcome {
  const ignore = (guard: IntentGuard): IntentOutcome => ({ decision: { action: 'ignore' }, guard });

  let guard = input.guard;
  if (input.kind === 'pointer') guard = OPEN_GUARD;
  else if (guard.needsPointer) return ignore(guard);

  if (input.holdsLease || input.inFlight) return ignore(guard);
  if (input.blocked) return { decision: { action: 'blocked', reason: input.blocked }, guard };
  if (input.controlState === 'HANDOFF_PENDING') {
    return { decision: { action: 'blocked', reason: 'control.blocked.handoff' }, guard };
  }
  if (input.controlState === 'HUMAN' && input.holderIsSelf) return { decision: { action: 'reconcile' }, guard };
  if (input.controlState === 'IDLE') return { decision: { action: 'acquire' }, guard };
  return { decision: { action: 'takeover' }, guard };
}

export interface KeyGesture {
  key: string;
  repeat: boolean;
  isComposing: boolean;
  ctrlKey: boolean;
  metaKey: boolean;
  altKey: boolean;
}

const EDIT_KEYS: Record<string, true> = {
  Enter: true,
  Backspace: true,
  Delete: true,
  ArrowUp: true,
  ArrowDown: true,
  ArrowLeft: true,
  ArrowRight: true,
  Home: true,
  End: true,
  PageUp: true,
  PageDown: true,
};

/**
 * True only for a keystroke that is typing. Held-key repeats, IME composition, modifier chords
 * (browser and LensDock shortcuts), Tab/Escape navigation and bare modifiers never ask for control.
 */
export function isDeliberateKey(e: KeyGesture): boolean {
  if (e.repeat || e.isComposing || e.ctrlKey || e.metaKey || e.altKey) return false;
  return e.key.length === 1 || Object.hasOwn(EDIT_KEYS, e.key);
}
