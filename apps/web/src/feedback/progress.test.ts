import { describe, expect, it } from 'vitest';
import type { DeliveryState } from '@/api/types';
import { eventTimes, hasDetail, progressLabel, progressOf, stepStatuses } from './progress';

describe('feedback progress', () => {
  it('collapses delivery states into Sent, In progress and Done', () => {
    expect(progressOf('QUEUED')).toEqual({ kind: 'stage', stage: 'sent' });
    expect(progressOf('ACKNOWLEDGED')).toEqual({ kind: 'stage', stage: 'sent' });
    expect(progressOf('IN_PROGRESS')).toEqual({ kind: 'stage', stage: 'inProgress' });
    expect(progressOf('RESOLVED')).toEqual({ kind: 'stage', stage: 'done' });
    expect(progressOf('VERIFIED')).toEqual({ kind: 'stage', stage: 'done' });
  });

  it('never presents a queued or read delivery as started or finished', () => {
    for (const state of ['QUEUED', 'ACKNOWLEDGED'] as const) {
      expect(stepStatuses(state)).toEqual({ sent: 'current', inProgress: 'todo', done: 'todo' });
      expect(progressLabel(state)).toBe('progress.sent');
    }
  });

  it('keeps an unsent draft and a canceled delivery out of the progress', () => {
    expect(progressOf('READY')).toEqual({ kind: 'ready' });
    expect(stepStatuses('READY')).toEqual({ sent: 'todo', inProgress: 'todo', done: 'todo' });
    expect(progressLabel('READY')).toBe('delivery.READY');

    expect(progressOf('CANCELLED')).toEqual({ kind: 'canceled' });
    expect(stepStatuses('CANCELLED')).toEqual({ sent: 'skipped', inProgress: 'skipped', done: 'skipped' });
    expect(progressLabel('CANCELLED')).toBe('delivery.CANCELLED');
  });

  it('treats an Agent-reported result as the current step until a human verifies it', () => {
    expect(stepStatuses('IN_PROGRESS')).toEqual({ sent: 'done', inProgress: 'current', done: 'todo' });
    expect(stepStatuses('RESOLVED')).toEqual({ sent: 'done', inProgress: 'done', done: 'current' });
    expect(stepStatuses('VERIFIED')).toEqual({ sent: 'done', inProgress: 'done', done: 'done' });
  });

  it('reports detail exactly where the primary label hides a distinction', () => {
    const withDetail: DeliveryState[] = ['QUEUED', 'ACKNOWLEDGED', 'RESOLVED', 'VERIFIED'];
    const without: DeliveryState[] = ['READY', 'IN_PROGRESS', 'CANCELLED'];
    for (const s of withDetail) expect(hasDetail(s)).toBe(true);
    for (const s of without) expect(hasDetail(s)).toBe(false);
  });

  it('keeps each delivery event on its own timestamp', () => {
    expect(eventTimes(null)).toEqual({ queued: undefined, read: undefined, started: undefined, reported: undefined, verified: undefined });
    const times = eventTimes({
      feedbackId: 'f',
      revision: 1,
      workspaceId: 'w',
      state: 'ACKNOWLEDGED',
      queuedAt: '2026-10-26T10:00:00Z',
      acknowledgedAt: '2026-10-26T10:01:00Z',
    });
    expect(times.queued).toBe('2026-10-26T10:00:00Z');
    expect(times.read).toBe('2026-10-26T10:01:00Z');
    expect(times.started).toBeUndefined();
    expect(times.reported).toBeUndefined();
  });
});
