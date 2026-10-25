import { useEffect, useRef, useState, type ReactNode } from 'react';
import { createDraft } from '@/api/feedback';
import type { CaptureState } from '@/api/types';
import { CAPTURE_STEP_KEYS, useCapture, warningText, type CaptureWarning } from '@/capture/CaptureProvider';
import { DrawingButton } from '@/capture/DrawingButton';
import { Button, Chip, Modal, Notice, type Tone } from '@/components/ui';
import { describeError } from '@/errors';
import { TaskForm } from '@/feedback/TaskForm';
import { emptyTask, sanitizeTaskForSave } from '@/feedback/task';
import { useDrafts } from '@/feedback/useDrafts';
import { useI18n } from '@/i18n';
import { useControl } from '@/state/control';
import { useTargets } from '@/state/targets';
import { useRoute } from '@/state/ui';
import { useWorkspace } from '@/state/workspace';
import { newKey } from '@/util/async';
import { formatDateTime, shortId } from '@/util/format';

const CONSISTENCY_TONE: Record<string, Tone> = { 'stable-navigation': 'ok', 'best-effort': 'amber', unknown: 'amber' };

export function CapturePanel(): ReactNode {
  const { t } = useI18n();
  const capture = useCapture();
  const { workspace, workspaceId } = useWorkspace();
  const { lease } = useControl();
  const { targets } = useTargets();
  const [targetId, setTargetId] = useState('');
  const [confirmInterrupt, setConfirmInterrupt] = useState(false);

  useEffect(() => {
    if (!targets.some((x) => x.id === targetId)) {
      setTargetId(targets.find((x) => x.visible === true)?.id ?? targets[0]?.id ?? '');
    }
  }, [targets, targetId]);

  const state = workspace?.controlState;
  const agentHolds = state === 'AGENT';
  const pageBlocked: string | null = !workspace?.ready
    ? t('capture.gate.notReady')
    : state === 'MAINTENANCE' || state === 'ERROR_LOCKED' || state === 'CAPTURE' || state === 'HANDOFF_PENDING'
      ? t('capture.gate.stateBlocks')
      : targetId === ''
        ? t('capture.gate.noTarget')
        : null;

  const startPage = (): void => {
    if (agentHolds && !lease) setConfirmInterrupt(true);
    else void capture.capturePage(targetId, { interruptAgent: false });
  };

  const stepKey = CAPTURE_STEP_KEYS[capture.step];

  return (
    <div className="panel-stack">
      <section className="panel">
        <h3>{t('drawing.title')}</h3>
        <p className="panel__desc">{t('drawing.description')}</p>
        <DrawingButton />
      </section>

      <section className="panel">
        <h3>{t('capture.desktop.title')}</h3>
        <p className="panel__desc">{t('capture.desktop.desc')}</p>
        {agentHolds ? <Notice tone="teal">{t('capture.desktop.agentOk')}</Notice> : null}
        <Button
          variant="primary"
          icon="camera"
          busy={capture.busy}
          disabledReason={capture.desktopBlocked ? t(capture.desktopBlocked) : null}
          onClick={() => void capture.annotateDesktop()}
        >
          {t('top.captureDesktop')}
        </Button>
      </section>

      <section className="panel">
        <h3>{t('capture.page.title')}</h3>
        <p className="panel__desc">{t('capture.page.desc')}</p>
        <label className="field__label" htmlFor="capture-target">
          {t('capture.page.target')}
        </label>
        <select id="capture-target" className="input" value={targetId} onChange={(e) => setTargetId(e.target.value)} disabled={targets.length === 0}>
          {targets.length === 0 ? <option value="">{t('target.empty')}</option> : null}
          {targets.map((x) => (
            <option key={x.id} value={x.id}>
              {x.visible === true ? '● ' : ''}
              {x.title || x.url}
            </option>
          ))}
        </select>
        {lease ? (
          <Notice tone="neutral">{t('capture.page.holdingHuman')}</Notice>
        ) : state === 'IDLE' ? (
          <Notice tone="neutral">{t('capture.page.willAcquire')}</Notice>
        ) : agentHolds ? (
          <Notice tone="amber" title={t('capture.page.agentTitle')}>
            {t('capture.page.agentBody')}
          </Notice>
        ) : null}
        <Button icon="file" busy={capture.busy} disabledReason={pageBlocked} onClick={startPage}>
          {t('top.capturePage')}
        </Button>
      </section>

      {capture.busy && stepKey ? (
        <p className="panel__progress" role="status">
          {t(stepKey)}
        </p>
      ) : null}
      {capture.error && capture.errorSource === 'page' ? (
        <Notice tone="danger" title={t('capture.failed')} actions={<Button small onClick={capture.clear}>{t('common.dismiss')}</Button>}>
          {capture.error}
        </Notice>
      ) : null}

      {capture.result ? (
        <CaptureResultCard
          capture={capture.result.capture}
          warnings={capture.result.warnings}
          targetTitle={capture.result.targetTitle}
          workspaceId={workspaceId}
          onDone={capture.clear}
        />
      ) : null}

      <DraftsSection />

      <Modal
        open={confirmInterrupt}
        onClose={() => setConfirmInterrupt(false)}
        title={t('capture.interrupt.title')}
        footer={
          <>
            <Button onClick={() => setConfirmInterrupt(false)}>{t('common.cancel')}</Button>
            <Button
              icon="camera"
              onClick={() => {
                setConfirmInterrupt(false);
                void capture.annotateDesktop();
              }}
            >
              {t('capture.interrupt.useDesktop')}
            </Button>
            <Button
              variant="danger"
              onClick={() => {
                setConfirmInterrupt(false);
                void capture.capturePage(targetId, { interruptAgent: true });
              }}
            >
              {t('capture.interrupt.confirm')}
            </Button>
          </>
        }
      >
        <Notice tone="amber">{t('capture.interrupt.body')}</Notice>
        <p>{t('capture.interrupt.advice')}</p>
      </Modal>
    </div>
  );
}

function CaptureResultCard({
  capture,
  warnings,
  targetTitle,
  workspaceId,
  onDone,
}: {
  capture: CaptureState;
  warnings: CaptureWarning[];
  targetTitle: string | null;
  workspaceId: string;
  onDone: () => void;
}): ReactNode {
  const { t, locale } = useI18n();
  const [, navigate] = useRoute();
  const [task, setTask] = useState(emptyTask);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const width = capture.imageWidthPx;
  const height = capture.imageHeightPx;
  const ready = capture.status === 'ready';
  // The key survives a failed or double-clicked submit of the same body, so no second draft appears.
  const attempt = useRef<{ body: string; key: string } | null>(null);

  const create = async (): Promise<void> => {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      const body = sanitizeTaskForSave(task);
      const serialized = JSON.stringify(body);
      if (attempt.current?.body !== serialized) attempt.current = { body: serialized, key: newKey() };
      const draft = await createDraft(workspaceId, capture.captureId, body, attempt.current.key);
      onDone();
      navigate({ name: 'editor', feedbackId: draft.feedbackId });
    } catch (err) {
      setError(describeError(err, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="panel panel--result" aria-live="polite">
      <h3>{t('capture.result.title')}</h3>
      <dl className="kv kv--tight">
        <div className="kv__row">
          <dt>{t('capture.result.source')}</dt>
          <dd>{t('capture.source.page')}</dd>
        </div>
        <div className="kv__row">
          <dt>{t('capture.result.size')}</dt>
          <dd className="mono">{width && height ? `${width}×${height} px` : '—'}</dd>
        </div>
        <div className="kv__row">
          <dt>{t('capture.result.id')}</dt>
          <dd className="mono">{shortId(capture.captureId, 12)}</dd>
        </div>
        {targetTitle ? (
          <div className="kv__row">
            <dt>{t('capture.result.target')}</dt>
            <dd>{targetTitle}</dd>
          </div>
        ) : null}
        <div className="kv__row">
          <dt>{t('capture.result.clientTime')}</dt>
          <dd className="mono">{formatDateTime(capture.capturedAt, locale)}</dd>
        </div>
        <div className="kv__row">
          <dt>{t('capture.result.serverTime')}</dt>
          <dd className="mono">{formatDateTime(capture.serverReceivedAt, locale)}</dd>
        </div>
        <div className="kv__row">
          <dt>{t('capture.result.consistency')}</dt>
          <dd>
            <Chip tone={CONSISTENCY_TONE[capture.consistency] ?? 'amber'}>{t(`capture.consistency.${capture.consistency}`)}</Chip>
          </dd>
        </div>
      </dl>
      <ul className="warnlist">
        {warnings.map((w, i) => (
          // The list is derived once per capture; position is its identity.
          <li key={i} className={w.kind === 'consistency' || w.kind === 'agentTargetUnknown' || w.kind === 'foreground' ? 'warnlist__amber' : ''}>
            {warningText(w, t)}
          </li>
        ))}
      </ul>

      {ready ? (
        <>
          <details className="editor__disclosure">
            <summary>{t('editor.optionalNote')}</summary>
            <TaskForm value={task} onChange={setTask} compact disabled={busy} />
          </details>
          {error ? <Notice tone="danger">{error}</Notice> : null}
          <div className="btn-row">
            <Button variant="primary" icon="pen" busy={busy} onClick={() => void create()}>
              {t('capture.result.createDraft')}
            </Button>
            <Button variant="ghost" onClick={onDone}>
              {t('capture.result.discard')}
            </Button>
          </div>
        </>
      ) : (
        <Notice tone="danger">{t('capture.result.notReady', { status: capture.status })}</Notice>
      )}
    </section>
  );
}

function DraftsSection(): ReactNode {
  const { t, locale } = useI18n();
  const { drafts, loading, error, remove } = useDrafts();
  const [, navigate] = useRoute();
  const [confirmId, setConfirmId] = useState<string | null>(null);
  const [removeError, setRemoveError] = useState<string | null>(null);

  return (
    <section className="panel">
      <h3>{t('drafts.title')}</h3>
      <p className="panel__desc">{t('drafts.desc')}</p>
      {loading ? <p>{t('common.loading')}</p> : null}
      {error ? <Notice tone="danger">{describeError(error, t)}</Notice> : null}
      {!loading && drafts.length === 0 && !error ? <p className="empty">{t('drafts.empty')}</p> : null}
      <ul className="rows">
        {drafts.map((d) => {
          const soon = d.expiresAt ? Date.parse(d.expiresAt) - Date.now() < 6 * 3600 * 1000 : false;
          return (
            <li key={d.feedbackId} className="row">
              <div className="row__main">
                <strong>{d.task.title || t('drafts.untitled')}</strong>
                <span className="row__meta mono">
                  r{d.revision} · {shortId(d.feedbackId, 8)}
                </span>
                {d.expiresAt ? (
                  <span className={`row__meta ${soon ? 'row__meta--amber' : ''}`}>
                    {soon ? '⚠ ' : ''}
                    {t('drafts.expires', { when: formatDateTime(d.expiresAt, locale) })}
                  </span>
                ) : null}
                {d.warnings?.includes('DRAFT_EXPIRING') ? <span className="row__meta row__meta--amber">{t('warn.DRAFT_EXPIRING')}</span> : null}
              </div>
              <div className="row__btns">
                <Button small icon="pen" onClick={() => navigate({ name: 'editor', feedbackId: d.feedbackId })}>
                  {t('drafts.open')}
                </Button>
                <Button small variant="ghost" icon="trash" aria-label={t('drafts.delete')} onClick={() => setConfirmId(d.feedbackId)} />
              </div>
            </li>
          );
        })}
      </ul>
      <Modal
        open={confirmId !== null}
        onClose={() => setConfirmId(null)}
        title={t('drafts.deleteTitle')}
        footer={
          <>
            <Button onClick={() => setConfirmId(null)}>{t('common.cancel')}</Button>
            <Button
              variant="danger"
              icon="trash"
              onClick={() => {
                const id = confirmId;
                if (!id) return;
                setRemoveError(null);
                void remove(id)
                  .then(() => setConfirmId(null))
                  .catch((err: unknown) => setRemoveError(describeError(err, t)));
              }}
            >
              {t('drafts.delete')}
            </Button>
          </>
        }
      >
        <p>{t('drafts.deleteBody')}</p>
        {removeError ? <Notice tone="danger">{removeError}</Notice> : null}
      </Modal>
    </section>
  );
}
