import { useState, type ReactNode } from 'react';
import { Button, Field, Modal, Notice } from '@/components/ui';
import { describeError } from '@/errors';
import { useI18n } from '@/i18n';
import { useControl } from '@/state/control';
import { useWorkspace } from '@/state/workspace';
import { codePointLength, truncateToCodePoints } from '@/util/text';

const REASON_MAX = 1000;

/**
 * Takeover handshake UI. The wait is real: the Gateway fences the old holder,
 * drains in-flight work for up to 5 s, and only then issues the HUMAN lease.
 * Forcing after a drain timeout needs an explicit acknowledgement because
 * earlier page scripts and network requests may still take effect.
 */
export function TakeoverDialog({ open, onClose }: { open: boolean; onClose: () => void }): ReactNode {
  const { t } = useI18n();
  const { workspace } = useWorkspace();
  const { takeover, take, forceTake, cancelTakeover, lease } = useControl();
  const [reason, setReason] = useState('');
  const [accepted, setAccepted] = useState(false);

  const phase = takeover.phase;
  const inFlight = phase === 'requesting' || phase === 'draining' || phase === 'acquiring';
  const holder = workspace?.holderId ?? null;
  const reasonOk = reason.trim().length > 0 && codePointLength(reason) <= REASON_MAX;
  const done = lease !== null && phase === 'idle';

  const close = (): void => {
    setAccepted(false);
    onClose();
  };

  const footer = (
    <>
      {phase === 'idle' || phase === 'failed' ? (
        <>
          <Button onClick={close}>{t('common.cancel')}</Button>
          <Button variant="primary" icon="hand" disabled={!reasonOk} onClick={() => void take(reason.trim())}>
            {phase === 'failed' ? t('takeover.retry') : t('takeover.confirm')}
          </Button>
        </>
      ) : null}
      {inFlight ? (
        <Button variant="warn" onClick={() => void cancelTakeover().then(close)}>
          {t('takeover.cancel')}
        </Button>
      ) : null}
      {phase === 'drain-timeout' ? (
        <>
          <Button onClick={() => void cancelTakeover().then(close)}>{t('takeover.cancel')}</Button>
          <Button variant="danger" icon="hand" disabled={!accepted} onClick={() => void forceTake(reason.trim() || t('takeover.forceReason'))}>
            {t('takeover.force')}
          </Button>
        </>
      ) : null}
    </>
  );

  return (
    <Modal open={open && !done} onClose={close} title={t('takeover.title')} footer={footer} dismissible={!inFlight}>
      {phase === 'idle' || phase === 'failed' ? (
        <>
          <p>{holder ? t('takeover.intro', { holder }) : t('takeover.introNone')}</p>
          <Notice tone="amber" title={t('takeover.cautionTitle')}>
            {t('takeover.caution')}
          </Notice>
          <Field label={t('takeover.reason')} hint={t('takeover.reasonHint', { n: codePointLength(reason), max: REASON_MAX })} htmlFor="takeover-reason">
            <textarea
              id="takeover-reason"
              className="input"
              rows={3}
              value={reason}
              onChange={(e) => setReason(truncateToCodePoints(e.target.value, REASON_MAX))}
              autoFocus
            />
          </Field>
          {takeover.error ? <Notice tone="danger">{describeError(takeover.error, t)}</Notice> : null}
        </>
      ) : null}

      {inFlight ? (
        <div className="progress-steps" role="status">
          <p>{phase === 'requesting' ? t('takeover.requesting') : phase === 'acquiring' ? t('takeover.acquiring') : t('takeover.draining')}</p>
          {takeover.handoff ? (
            <p className="mono">
              {takeover.handoff.status} · {takeover.handoff.drained ? t('takeover.drained') : t('takeover.notDrained')}
            </p>
          ) : null}
        </div>
      ) : null}

      {phase === 'drain-timeout' ? (
        <>
          <Notice tone="amber" title={t('takeover.timeoutTitle')}>
            {t('takeover.timeoutBody')}
          </Notice>
          <label className="check">
            <input type="checkbox" checked={accepted} onChange={(e) => setAccepted(e.target.checked)} />
            <span>{t('takeover.acceptRisk')}</span>
          </label>
        </>
      ) : null}
    </Modal>
  );
}
