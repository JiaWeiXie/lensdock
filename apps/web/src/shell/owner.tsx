import type { ReactNode } from 'react';
import type { ControlState } from '@/api/types';
import type { MessageKey } from '@/i18n';
import { useClock } from '@/vnc/DesktopProvider';
import { useControl } from '@/state/control';
import { useSession } from '@/state/session';
import { useWorkspace } from '@/state/workspace';
import { secondsUntil } from '@/util/format';

export type OwnerState = ControlState | 'UNKNOWN';

export interface OwnerView {
  state: OwnerState;
  labelKey: MessageKey;
  holderId: string | null;
  /** This browser session holds the HUMAN lease. */
  mine: boolean;
  epoch: number | null;
  generation: string | null;
  secondsLeft: number | null;
}

const LABELS: Record<OwnerState, MessageKey> = {
  IDLE: 'owner.IDLE',
  HUMAN: 'owner.HUMAN',
  AGENT: 'owner.AGENT',
  HANDOFF_PENDING: 'owner.HANDOFF_PENDING',
  CAPTURE: 'owner.CAPTURE',
  MAINTENANCE: 'owner.MAINTENANCE',
  ERROR_LOCKED: 'owner.ERROR_LOCKED',
  UNKNOWN: 'owner.UNKNOWN',
};

/** One answer to "who may write to the browser right now". Server state wins; our lease only adds detail. */
export function useOwner(): OwnerView {
  const { workspace } = useWorkspace();
  const { lease } = useControl();
  const { actor } = useSession();
  const now = useClock();

  const state: OwnerState = workspace ? workspace.controlState : 'UNKNOWN';
  const mine = lease !== null && state === 'HUMAN' && workspace?.holderId === actor?.id;
  const expiresAt = mine ? lease.expiresAt : (workspace?.lease?.expiresAt ?? workspace?.leaseExpiresAt ?? null);
  return {
    state,
    labelKey: mine ? 'owner.HUMAN_MINE' : LABELS[state],
    holderId: workspace?.holderId ?? null,
    mine,
    epoch: mine ? lease.epoch : (workspace?.lease?.epoch ?? workspace?.epoch ?? null),
    generation: workspace?.runtimeGeneration ?? null,
    secondsLeft: state === 'HUMAN' || state === 'AGENT' ? secondsUntil(expiresAt, now) : null,
  };
}

/** State glyph: the silhouette carries the state, so color is never the only signal. */
export function OwnerGlyph({ state, size = 44 }: { state: OwnerState; size?: number }): ReactNode {
  const common = { width: size, height: size, viewBox: '0 0 44 44', 'aria-hidden': true as const, focusable: false as const };
  switch (state) {
    case 'HUMAN':
      return (
        <svg {...common} className="glyph glyph--human">
          <rect x="6" y="6" width="32" height="32" rx="3" fill="currentColor" />
          <circle cx="22" cy="18" r="5" fill="var(--rail)" />
          <path d="M12 33c1-6 5-9 10-9s9 3 10 9z" fill="var(--rail)" />
        </svg>
      );
    case 'AGENT':
      return (
        <svg {...common} className="glyph glyph--agent">
          <path d="M22 3 41 22 22 41 3 22z" fill="currentColor" />
          <rect x="15" y="16" width="14" height="11" rx="2" fill="var(--rail)" />
          <circle cx="19" cy="21.5" r="1.7" fill="currentColor" />
          <circle cx="25" cy="21.5" r="1.7" fill="currentColor" />
        </svg>
      );
    case 'HANDOFF_PENDING':
      return (
        <svg {...common} className="glyph glyph--handoff">
          <path d="M8 6h28M8 38h28M11 6c0 10 22 10 22 16S11 28 11 38M33 6c0 10-22 10-22 16s22 6 22 16" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
          <path d="M16 33h12l-6-5z" fill="currentColor" />
        </svg>
      );
    case 'CAPTURE':
      return (
        <svg {...common} className="glyph glyph--capture">
          <circle cx="22" cy="22" r="16" fill="none" stroke="currentColor" strokeWidth="3" />
          <path d="M22 4v9M22 31v9M4 22h9M31 22h9" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
          <circle cx="22" cy="22" r="4" fill="currentColor" />
        </svg>
      );
    case 'MAINTENANCE':
      return (
        <svg {...common} className="glyph glyph--maint">
          <path d="M22 4 37 13v18L22 40 7 31V13z" fill="none" stroke="currentColor" strokeWidth="3" strokeLinejoin="round" />
          <path d="M16 24 22 18l6 6M22 18v10" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      );
    case 'ERROR_LOCKED':
      return (
        <svg {...common} className="glyph glyph--locked">
          <rect x="8" y="19" width="28" height="20" rx="3" fill="currentColor" />
          <path d="M14 19v-5a8 8 0 0 1 16 0v5" fill="none" stroke="currentColor" strokeWidth="3" />
          <path d="M18 26l8 8M26 26l-8 8" stroke="var(--rail)" strokeWidth="3" strokeLinecap="round" />
        </svg>
      );
    case 'IDLE':
      return (
        <svg {...common} className="glyph glyph--idle">
          <circle cx="22" cy="22" r="15" fill="none" stroke="currentColor" strokeWidth="3" strokeDasharray="4 5" />
        </svg>
      );
    default:
      return (
        <svg {...common} className="glyph glyph--unknown">
          <circle cx="22" cy="22" r="15" fill="none" stroke="currentColor" strokeWidth="3" />
          <path d="M17 18a5 5 0 1 1 7 4.5c-1.5.8-2 1.6-2 3M22 31v1" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
        </svg>
      );
  }
}
