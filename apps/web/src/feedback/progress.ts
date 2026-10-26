import type { DeliveryDetail, DeliveryState } from '@/api/types';
import type { MessageKey } from '@/i18n';

/**
 * The primary feedback progress is Sent / In progress / Done. The delivery states behind it stay
 * distinct and are reported as detail: queued is not read, read is not started, started is not
 * executed, and an Agent-reported result is not verified until a human says so.
 */

export type ProgressStage = 'sent' | 'inProgress' | 'done';

export const PROGRESS_STAGES: readonly ProgressStage[] = ['sent', 'inProgress', 'done'];

export type Progress = { kind: 'ready' } | { kind: 'canceled' } | { kind: 'stage'; stage: ProgressStage };

export function progressOf(state: DeliveryState): Progress {
  switch (state) {
    case 'READY':
      return { kind: 'ready' };
    case 'CANCELLED':
      return { kind: 'canceled' };
    case 'QUEUED':
    case 'ACKNOWLEDGED':
      return { kind: 'stage', stage: 'sent' };
    case 'IN_PROGRESS':
      return { kind: 'stage', stage: 'inProgress' };
    case 'RESOLVED':
    case 'VERIFIED':
      return { kind: 'stage', stage: 'done' };
  }
}

export function progressLabel(state: DeliveryState): MessageKey {
  const p = progressOf(state);
  if (p.kind === 'ready') return 'delivery.READY';
  if (p.kind === 'canceled') return 'delivery.CANCELLED';
  return `progress.${p.stage}`;
}

/** True when the exact delivery state says more than its primary label does. */
export function hasDetail(state: DeliveryState): boolean {
  return state === 'QUEUED' || state === 'ACKNOWLEDGED' || state === 'RESOLVED' || state === 'VERIFIED';
}

export type StepStatus = 'done' | 'current' | 'todo' | 'skipped';

/**
 * Per-step status. The Done step is only complete once a human verified; an Agent-reported result
 * is the current step. A canceled delivery marks every step skipped.
 */
export function stepStatuses(state: DeliveryState): Record<ProgressStage, StepStatus> {
  const p = progressOf(state);
  if (p.kind === 'canceled') return { sent: 'skipped', inProgress: 'skipped', done: 'skipped' };
  if (p.kind === 'ready') return { sent: 'todo', inProgress: 'todo', done: 'todo' };
  const current = PROGRESS_STAGES.indexOf(p.stage);
  const status = (stage: ProgressStage): StepStatus => {
    const index = PROGRESS_STAGES.indexOf(stage);
    if (index < current) return 'done';
    if (index > current) return 'todo';
    return state === 'VERIFIED' ? 'done' : 'current';
  };
  return { sent: status('sent'), inProgress: status('inProgress'), done: status('done') };
}

export type ProgressEvent = 'queued' | 'read' | 'started' | 'reported' | 'verified';

/** The delivery events that belong under each primary step. */
export const STAGE_EVENTS: Record<ProgressStage, readonly ProgressEvent[]> = {
  sent: ['queued', 'read'],
  inProgress: ['started'],
  done: ['reported', 'verified'],
};

/** When each event was recorded; absent means it has not happened. */
export function eventTimes(d: DeliveryDetail | null): Record<ProgressEvent, string | undefined> {
  return {
    queued: d?.queuedAt,
    read: d?.acknowledgedAt,
    started: d?.inProgressAt,
    reported: d?.resolvedAt,
    verified: d?.verifiedAt,
  };
}
