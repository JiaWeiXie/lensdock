import { useState, type ReactNode } from 'react';
import { Button, Modal, Notice } from '@/components/ui';
import { describeError } from '@/errors';
import { useI18n } from '@/i18n';
import { useControl } from '@/state/control';
import { useWorkspace } from '@/state/workspace';

/**
 * Status of a takeover that a desktop gesture started; there is no form to fill in first.
 * The wait is real: the Gateway fences the old holder, drains in-flight work for up to 5 s, and
 * only then issues the HUMAN lease. The dialog appears only while that handshake runs or needs the
 * user. Forcing after a drain timeout needs an explicit acknowledgement because earlier page
 * scripts and network requests may still take effect.
 */
export function TakeoverDialog(): ReactNode {
  const { t } = useI18n();
  const { workspace } = useWorkspace();
  const { takeover, take, forceTake, cancelTakeover } = useControl();
  const [accepted, setAccepted] = useState(false);

  const phase = takeover.phase;
  const inFlight = phase === 'requesting' || phase === 'draining' || phase === 'acquiring';
  const holder = workspace?.holderId ?? null;

  const cancel = (): void => {
    setAccepted(false);
    void cancelTakeover();
  };

  const force = (): void => {
    setAccepted(false);
    void forceTake(t('takeover.forceReason'));
  };

  const footer = (
    <>
      {inFlight ? (
        <Button variant="warn" onClick={cancel}>
          {t('takeover.cancel')}
        </Button>
      ) : null}
      {phase === 'failed' ? (
        <>
          <Button onClick={cancel}>{t('common.close')}</Button>
          <Button variant="primary" icon="hand" onClick={() => void take()}>
            {t('takeover.retry')}
          </Button>
        </>
      ) : null}
      {phase === 'drain-timeout' ? (
        <>
          <Button onClick={cancel}>{t('takeover.cancel')}</Button>
          <Button variant="danger" icon="hand" disabled={!accepted} onClick={force}>
            {t('takeover.force')}
          </Button>
        </>
      ) : null}
    </>
  );

  return (
    <Modal open={phase !== 'idle'} onClose={cancel} title={t('takeover.title')} footer={footer} dismissible={!inFlight}>
      {inFlight ? (
        <div className="progress-steps" role="status">
          {holder ? <p>{t('takeover.intro', { holder })}</p> : null}
          <p>{phase === 'requesting' ? t('takeover.requesting') : phase === 'acquiring' ? t('takeover.acquiring') : t('takeover.draining')}</p>
          {takeover.handoff ? (
            <p className="mono">
              {takeover.handoff.status} · {takeover.handoff.drained ? t('takeover.drained') : t('takeover.notDrained')}
            </p>
          ) : null}
          <Notice tone="amber" title={t('takeover.cautionTitle')}>
            {t('takeover.caution')}
          </Notice>
        </div>
      ) : null}

      {phase === 'failed' && takeover.error ? <Notice tone="danger">{describeError(takeover.error, t)}</Notice> : null}

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
