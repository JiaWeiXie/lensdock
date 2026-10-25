import { useEffect, useState, type ReactNode } from 'react';
import type { ApiError } from '@/api/http';
import type { ResolutionInfo } from '@/api/types';
import { getResolution, putResolution } from '@/api/workspace';
import { Button } from '@/components/ui';
import { describeError } from '@/errors';
import { useI18n } from '@/i18n';
import { useControl } from '@/state/control';
import { useWorkspace } from '@/state/workspace';
import { useRfb } from '@/vnc/DesktopProvider';

const key = (s: { width: number; height: number }): string => `${s.width}x${s.height}`;

/**
 * Remote resolution request. The list comes from the runtime's X server probe;
 * the displayed "actual" size is always the RFB framebuffer, never the request.
 */
export function ResolutionPicker(): ReactNode {
  const { t } = useI18n();
  const { workspaceId } = useWorkspace();
  const { lease } = useControl();
  const rfb = useRfb();
  const [info, setInfo] = useState<ResolutionInfo | null>(null);
  const [loadError, setLoadError] = useState<ApiError | null>(null);
  const [choice, setChoice] = useState('');
  const [busy, setBusy] = useState(false);
  const [applyError, setApplyError] = useState<unknown>(null);

  useEffect(() => {
    const controller = new AbortController();
    getResolution(workspaceId, controller.signal)
      .then((r) => {
        setInfo(r);
        setChoice(key(r.current));
      })
      .catch((err: unknown) => {
        if (!controller.signal.aborted) setLoadError(err as ApiError);
      });
    return () => controller.abort();
  }, [workspaceId]);

  const inputLive = rfb.phase === 'connected' && rfb.mode === 'control';

  const apply = async (): Promise<void> => {
    const size = info?.supported.find((s) => key(s) === choice);
    if (!size) return;
    setBusy(true);
    setApplyError(null);
    try {
      if (!lease) throw new Error(t('stage.keys.needControl'));
      await putResolution(workspaceId, size, lease);
    } catch (err) {
      setApplyError(err);
    } finally {
      setBusy(false);
    }
  };

  if (loadError) return <p className="menu-form__note">{describeError(loadError, t)}</p>;
  if (!info) return <p className="menu-form__note">{t('common.loading')}</p>;
  if (info.supported.length <= 1) return <p className="menu-form__note">{t('stage.resolution.unavailable')}</p>;

  return (
    <div className="menu-form">
      <label className="menu-form__row">
        <span>{t('stage.resolution.request')}</span>
        <select className="input input--sm" value={choice} onChange={(e) => setChoice(e.target.value)}>
          {info.supported.map((s) => (
            <option key={key(s)} value={key(s)}>
              {s.width}×{s.height}
            </option>
          ))}
        </select>
      </label>
      <Button small variant="primary" busy={busy} disabledReason={inputLive ? null : t('stage.keys.needControl')} onClick={() => void apply()}>
        {t('stage.resolution.apply')}
      </Button>
      {applyError ? (
        <p className="field__error" role="alert">
          {describeError(applyError, t)}
        </p>
      ) : null}
    </div>
  );
}
