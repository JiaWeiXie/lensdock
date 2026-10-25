import type { FeedbackIntent, FeedbackKind, TaskFields } from '@/api/types';
import { codePointLength } from '@/util/text';

/** Field limits from schemas/feedback.schema.json (JSON Schema counts code points). */
export const TASK_LIMITS = {
  title: 200,
  description: 12000,
  expectedResult: 6000,
  actualResult: 6000,
  stepCount: 30,
  step: 2000,
} as const;

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

export type TaskIssue =
  | 'title.tooLong'
  | 'description.tooLong'
  | 'expected.tooLong'
  | 'actual.tooLong'
  | 'steps.tooMany'
  | 'steps.empty'
  | 'steps.tooLong';

/**
 * Publish-time validation. Every prose field is optional; only length and
 * reproduction-step shape are checked. Drafts are saved leniently by the server.
 */
export function validateTask(task: TaskFields): TaskIssue[] {
  const issues: TaskIssue[] = [];
  if (codePointLength(task.title) > TASK_LIMITS.title) issues.push('title.tooLong');
  if (codePointLength(task.description) > TASK_LIMITS.description) issues.push('description.tooLong');
  if (codePointLength(task.expectedResult) > TASK_LIMITS.expectedResult) issues.push('expected.tooLong');
  if (codePointLength(task.actualResult) > TASK_LIMITS.actualResult) issues.push('actual.tooLong');
  if (task.reproductionSteps.length > TASK_LIMITS.stepCount) issues.push('steps.tooMany');
  for (const s of task.reproductionSteps) {
    if (s.trim().length === 0) {
      issues.push('steps.empty');
      break;
    }
  }
  for (const s of task.reproductionSteps) {
    if (codePointLength(s) > TASK_LIMITS.step) {
      issues.push('steps.tooLong');
      break;
    }
  }
  return issues;
}

/** Task text as it is sent: nothing is normalized, only blank reproduction steps are dropped. */
export function sanitizeTaskForSave(task: TaskFields): TaskFields {
  return { ...task, reproductionSteps: task.reproductionSteps.filter((s) => s.trim().length > 0) };
}
