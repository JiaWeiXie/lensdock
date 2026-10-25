import { useId, useState, type ReactNode } from 'react';
import type { FeedbackIntent, FeedbackKind, TaskFields } from '@/api/types';
import { Button, Field } from '@/components/ui';
import { useI18n } from '@/i18n';
import { codePointLength, truncateToCodePoints } from '@/util/text';
import { FEEDBACK_INTENTS, FEEDBACK_KINDS, TASK_LIMITS, type TaskIssue } from './task';

export interface PinRef {
  number: number;
  comment: string;
}

/**
 * Feedback task form. Text is stored exactly as typed (no normalization);
 * limits count code points like the server but never cut a grapheme cluster.
 */
export function TaskForm({
  value,
  onChange,
  compact = false,
  disabled = false,
  issues = [],
  pins = [],
}: {
  value: TaskFields;
  onChange: (next: TaskFields) => void;
  compact?: boolean;
  disabled?: boolean;
  issues?: readonly TaskIssue[];
  pins?: readonly PinRef[];
}): ReactNode {
  const { t } = useI18n();
  const uid = useId();
  const id = (n: string): string => `${uid}-${n}`;
  const has = (i: TaskIssue): boolean => issues.includes(i);
  const [expanded, setExpanded] = useState(false);
  // Advanced fields stay mounted while collapsed (native <details>), so text is never cleared.
  // A validation issue inside them forces the disclosure open so the error is visible.
  const advancedIssue = issues.some((i) => i !== 'title.tooLong' && i !== 'description.tooLong');

  const setStep = (index: number, text: string): void => {
    const next = [...value.reproductionSteps];
    next[index] = truncateToCodePoints(text, TASK_LIMITS.step);
    onChange({ ...value, reproductionSteps: next });
  };
  const moveStep = (index: number, delta: -1 | 1): void => {
    const to = index + delta;
    if (to < 0 || to >= value.reproductionSteps.length) return;
    const next = [...value.reproductionSteps];
    const a = next[index];
    const b = next[to];
    if (a === undefined || b === undefined) return;
    next[index] = b;
    next[to] = a;
    onChange({ ...value, reproductionSteps: next });
  };
  const removeStep = (index: number): void => {
    onChange({ ...value, reproductionSteps: value.reproductionSteps.filter((_, i) => i !== index) });
  };
  const addStep = (text = ''): void => {
    if (value.reproductionSteps.length >= TASK_LIMITS.stepCount) return;
    onChange({ ...value, reproductionSteps: [...value.reproductionSteps, text] });
  };

  return (
    <div className="taskform">
      <Field
        label={t('task.title.optional')}
        htmlFor={id('title')}
        hint={t('common.count', { n: codePointLength(value.title), max: TASK_LIMITS.title })}
        error={has('title.tooLong') ? t('task.err.tooLong') : null}
      >
        <input
          id={id('title')}
          className="input"
          disabled={disabled}
          placeholder={t('task.title.placeholder')}
          value={value.title}
          onChange={(e) => onChange({ ...value, title: truncateToCodePoints(e.target.value, TASK_LIMITS.title) })}
        />
      </Field>

      <Field
        label={t('task.notes.optional')}
        htmlFor={id('desc')}
        hint={t('common.count', { n: codePointLength(value.description), max: TASK_LIMITS.description })}
        error={has('description.tooLong') ? t('task.err.tooLong') : null}
      >
        <textarea id={id('desc')} className="input" rows={compact ? 3 : 4} disabled={disabled} value={value.description} onChange={(e) => onChange({ ...value, description: truncateToCodePoints(e.target.value, TASK_LIMITS.description) })} />
      </Field>

      <details
        className="taskform__advanced"
        open={expanded || advancedIssue}
        onToggle={(e) => setExpanded(e.currentTarget.open)}
      >
        <summary>{t('task.advanced')}</summary>
        <div className="taskform__advancedbody">
          <div className="taskform__pair">
            <Field label={t('task.kind')} htmlFor={id('kind')}>
              <select id={id('kind')} className="input" disabled={disabled} value={value.kind} onChange={(e) => onChange({ ...value, kind: e.target.value as FeedbackKind })}>
                {FEEDBACK_KINDS.map((k) => (
                  <option key={k} value={k}>
                    {t(`task.kind.${k}`)}
                  </option>
                ))}
              </select>
            </Field>
            <Field label={t('task.intent')} htmlFor={id('intent')} hint={t(`task.intent.${value.intent}.hint`)}>
              <select id={id('intent')} className="input" disabled={disabled} value={value.intent} onChange={(e) => onChange({ ...value, intent: e.target.value as FeedbackIntent })}>
                {FEEDBACK_INTENTS.map((k) => (
                  <option key={k} value={k}>
                    {t(`task.intent.${k}`)}
                  </option>
                ))}
              </select>
            </Field>
          </div>

          {!compact ? (
            <>
              <div className="taskform__pair">
                <Field
                  label={t('task.expected')}
                  htmlFor={id('exp')}
                  hint={t('common.count', { n: codePointLength(value.expectedResult), max: TASK_LIMITS.expectedResult })}
                  error={has('expected.tooLong') ? t('task.err.tooLong') : null}
                >
                  <textarea id={id('exp')} className="input" rows={3} disabled={disabled} value={value.expectedResult} onChange={(e) => onChange({ ...value, expectedResult: truncateToCodePoints(e.target.value, TASK_LIMITS.expectedResult) })} />
                </Field>
                <Field
                  label={t('task.actual')}
                  htmlFor={id('act')}
                  hint={t('common.count', { n: codePointLength(value.actualResult), max: TASK_LIMITS.actualResult })}
                  error={has('actual.tooLong') ? t('task.err.tooLong') : null}
                >
                  <textarea id={id('act')} className="input" rows={3} disabled={disabled} value={value.actualResult} onChange={(e) => onChange({ ...value, actualResult: truncateToCodePoints(e.target.value, TASK_LIMITS.actualResult) })} />
                </Field>
              </div>

              <fieldset className="steps">
                <legend>{t('task.steps')}</legend>
                {value.reproductionSteps.length === 0 ? <p className="field__hint">{t('task.steps.empty')}</p> : null}
                <ol>
                  {value.reproductionSteps.map((s, i) => (
                    // Steps are an ordered list with no stable identity beyond position.
                    <li key={i}>
                      <input
                        className="input"
                        aria-label={t('task.steps.n', { n: i + 1 })}
                        disabled={disabled}
                        value={s}
                        onChange={(e) => setStep(i, e.target.value)}
                      />
                      <div className="steps__btns">
                        <Button small variant="ghost" icon="chevronUp" aria-label={t('task.steps.up')} disabled={disabled || i === 0} onClick={() => moveStep(i, -1)} />
                        <Button small variant="ghost" icon="chevronDown" aria-label={t('task.steps.down')} disabled={disabled || i === value.reproductionSteps.length - 1} onClick={() => moveStep(i, 1)} />
                        <Button small variant="ghost" icon="trash" aria-label={t('task.steps.remove')} disabled={disabled} onClick={() => removeStep(i)} />
                      </div>
                    </li>
                  ))}
                </ol>
                {has('steps.empty') ? <p className="field__error">{t('task.err.stepEmpty')}</p> : null}
                <div className="steps__add">
                  <Button small icon="check" disabled={disabled || value.reproductionSteps.length >= TASK_LIMITS.stepCount} onClick={() => addStep()}>
                    {t('task.steps.add')}
                  </Button>
                  {pins.length > 0 ? (
                    <span className="steps__pins">
                      {pins.map((p) => (
                        <Button key={p.number} small variant="ghost" disabled={disabled} title={p.comment || undefined} onClick={() => addStep(t('task.steps.pinRef', { n: p.number, comment: p.comment }).trim())}>
                          {t('task.steps.fromPin', { n: p.number })}
                        </Button>
                      ))}
                    </span>
                  ) : null}
                </div>
              </fieldset>
            </>
          ) : null}
        </div>
      </details>
    </div>
  );
}
