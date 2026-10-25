import { useId, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { createDrawingCapture } from '@/api/capture';
import { createDraft } from '@/api/feedback';
import { Button, Field, Notice } from '@/components/ui';
import { Popover } from '@/components/Popover';
import { describeError } from '@/errors';
import { emptyTask, TASK_LIMITS } from '@/feedback/task';
import { useI18n } from '@/i18n';
import { useRoute } from '@/state/ui';
import { useWorkspace } from '@/state/workspace';
import { newKey } from '@/util/async';
import { truncateToCodePoints } from '@/util/text';

const DEFAULT_WIDTH = 1600;
const DEFAULT_HEIGHT = 900;

interface DrawingRequest {
  width: number;
  height: number;
  title: string;
}

interface CreationAttempt {
  fingerprint: string;
  captureKey: string;
  draftKey: string;
  captureId?: string;
}

const DEFAULT_REQUEST: DrawingRequest = { width: DEFAULT_WIDTH, height: DEFAULT_HEIGHT, title: '' };

/**
 * A drawing is private feedback data, independent of the remote browser's availability.
 * The main button creates a default blank canvas and opens the editor in one click; the
 * chevron opens optional title and size settings.
 */
export function DrawingButton(): ReactNode {
  const { t } = useI18n();
  const { workspaceId } = useWorkspace();
  const [, navigate] = useRoute();
  const uid = useId();
  const [title, setTitle] = useState('');
  const [width, setWidth] = useState(String(DEFAULT_WIDTH));
  const [height, setHeight] = useState(String(DEFAULT_HEIGHT));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const attemptRef = useRef<CreationAttempt | null>(null);
  const lastRequest = useRef<DrawingRequest>(DEFAULT_REQUEST);
  const submitting = useRef(false);
  const w = Number(width);
  const h = Number(height);
  const validSize = Number.isInteger(w) && Number.isInteger(h) && w >= 64 && w <= 4096 && h >= 64 && h <= 4096;

  const create = async (request: DrawingRequest): Promise<void> => {
    if (submitting.current) return;
    submitting.current = true;
    lastRequest.current = request;
    setBusy(true);
    setError(null);
    const fingerprint = JSON.stringify([workspaceId, request.width, request.height, request.title]);
    if (attemptRef.current?.fingerprint !== fingerprint) {
      attemptRef.current = { fingerprint, captureKey: newKey(), draftKey: newKey() };
    }
    const attempt = attemptRef.current;
    try {
      // Keep both idempotency keys across uncertain network responses so Retry does not
      // allocate a second capture or create a second feedback draft.
      if (!attempt.captureId) {
        const capture = await createDrawingCapture(workspaceId, request.width, request.height, attempt.captureKey);
        attempt.captureId = capture.captureId;
      }
      const draft = await createDraft(workspaceId, attempt.captureId, { ...emptyTask(), kind: 'task', title: request.title }, attempt.draftKey);
      attemptRef.current = null;
      setTitle('');
      navigate({ name: 'editor', feedbackId: draft.feedbackId });
    } catch (err) {
      setError(describeError(err, t));
    } finally {
      submitting.current = false;
      setBusy(false);
    }
  };

  const submitCustom = (event: FormEvent<HTMLFormElement>): void => {
    event.preventDefault();
    if (!validSize) return;
    void create({ width: w, height: h, title });
  };

  return (
    <>
      <div className="drawing-split">
        <Button className="drawing-launch" icon="drawing" busy={busy} onClick={() => void create(DEFAULT_REQUEST)}>
          {t('drawing.create')}
        </Button>
        <span className="drawing-split__options">
          <Popover label={t('drawing.options')} align="end">
            <form className="menu-form drawing-options" onSubmit={submitCustom}>
              <p className="menu-form__note">{t('drawing.description')}</p>
              <Field label={t('task.title.optional')} htmlFor={`${uid}-title`}>
                <input
                  id={`${uid}-title`}
                  className="input"
                  value={title}
                  disabled={busy}
                  onChange={(e) => setTitle(truncateToCodePoints(e.target.value, TASK_LIMITS.title))}
                />
              </Field>
              <div className="taskform__pair">
                <Field label={t('drawing.width')} htmlFor={`${uid}-width`} hint={t('drawing.sizeHint')}>
                  <input id={`${uid}-width`} className="input" type="number" min={64} max={4096} step={1} value={width} disabled={busy} onChange={(e) => setWidth(e.target.value)} />
                </Field>
                <Field label={t('drawing.height')} htmlFor={`${uid}-height`} hint={t('drawing.sizeHint')}>
                  <input id={`${uid}-height`} className="input" type="number" min={64} max={4096} step={1} value={height} disabled={busy} onChange={(e) => setHeight(e.target.value)} />
                </Field>
              </div>
              {!validSize ? <Notice tone="amber">{t('drawing.invalidSize')}</Notice> : null}
              <Button type="submit" variant="primary" icon="pen" busy={busy} disabled={!validSize}>
                {t('drawing.open')}
              </Button>
            </form>
          </Popover>
        </span>
      </div>
      {error ? (
        <div className="capture-error" role="alert">
          <span className="capture-error__text">{error}</span>
          <Button small icon="refresh" busy={busy} onClick={() => void create(lastRequest.current)}>
            {t('common.retry')}
          </Button>
          <Button small variant="ghost" disabled={busy} onClick={() => setError(null)}>
            {t('common.dismiss')}
          </Button>
        </div>
      ) : null}
    </>
  );
}
