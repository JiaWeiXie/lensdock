import type { FeedbackIntent, FeedbackKind, TaskFields } from '@/api/types';

export const FEEDBACK_KINDS: readonly FeedbackKind[] = ['bug', 'task', 'question', 'visual-regression'];
export const FEEDBACK_INTENTS: readonly FeedbackIntent[] = ['analyze-only', 'reproduce', 'modify-and-verify'];

export function emptyTask(): TaskFields {
  return {
    kind: 'bug',
    intent: 'analyze-only',
    title: '',
    description: '',
    expectedResult: '',
    actualResult: '',
    reproductionSteps: [],
  };
}

export type TaskIssue = 'steps.empty';

/**
 * Publish-time validation. Every prose field is optional and none has a
 * length or count cap; only the reproduction-step shape is checked. Drafts
 * are saved leniently by the server.
 */
export function validateTask(task: TaskFields): TaskIssue[] {
  const issues: TaskIssue[] = [];
  for (const s of task.reproductionSteps) {
    if (s.trim().length === 0) {
      issues.push('steps.empty');
      break;
    }
  }
  return issues;
}

/** Task text as it is sent: nothing is normalized, only blank reproduction steps are dropped. */
export function sanitizeTaskForSave(task: TaskFields): TaskFields {
  return { ...task, reproductionSteps: task.reproductionSteps.filter((s) => s.trim().length > 0) };
}
