import { useCallback, useEffect, useRef, useState, type ReactNode, type RefObject } from 'react';
import { Button } from '@/components/ui';
import { Icon } from '@/components/icons';
import { Popover } from '@/components/Popover';
import { useI18n } from '@/i18n';
import { OwnerBadge } from '@/shell/OwnerRail';
import { useControl } from '@/state/control';
import { FONT_SIZES, useUi } from '@/state/ui';
import { useWorkspace } from '@/state/workspace';
import { KEY_COMBOS } from '@/vnc/keys';
import { DEFAULT_VIEW_OPTIONS, type ViewOptions } from '@/vnc/controller';
import { useClock, useRfb, useRfbController } from '@/vnc/DesktopProvider';
import { ResolutionPicker } from './ResolutionPicker';

const VIEW_KEY = 'lensdock.view';

function loadViewOptions(): ViewOptions {
  try {
    const raw = localStorage.getItem(VIEW_KEY);
    if (!raw) return DEFAULT_VIEW_OPTIONS;
    const p = JSON.parse(raw) as Partial<ViewOptions>;
    const int09 = (v: unknown, d: number): number => (typeof v === 'number' && Number.isInteger(v) && v >= 0 && v <= 9 ? v : d);
    return {
      fit: typeof p.fit === 'boolean' ? p.fit : DEFAULT_VIEW_OPTIONS.fit,
      quality: int09(p.quality, DEFAULT_VIEW_OPTIONS.quality),
      compression: int09(p.compression, DEFAULT_VIEW_OPTIONS.compression),
      dotCursor: typeof p.dotCursor === 'boolean' ? p.dotCursor : DEFAULT_VIEW_OPTIONS.dotCursor,
    };
  } catch {
    return DEFAULT_VIEW_OPTIONS;
  }
}

/** The live desktop zone: RFB canvas, view controls, honest connection/freshness state. */
export function DesktopStage({ stageRef }: { stageRef: RefObject<HTMLDivElement | null> }): ReactNode {
  const { t } = useI18n();
  const { fontSize, setFontSize } = useUi();
  const controller = useRfbController();
  const rfb = useRfb();
  const { lease, release, busy } = useControl();
  const { error: pollError, lastOkAt } = useWorkspace();
  const now = useClock();
  const screenRef = useRef<HTMLDivElement>(null);
  const [fullscreen, setFullscreen] = useState(false);

  useEffect(() => {
    controller.setOptions(loadViewOptions());
  }, [controller]);

  useEffect(() => {
    controller.attach(screenRef.current);
    return () => controller.attach(null);
  }, [controller]);

  useEffect(() => {
    localStorage.setItem(VIEW_KEY, JSON.stringify(rfb.options));
  }, [rfb.options]);

  useEffect(() => {
    const onChange = (): void => setFullscreen(document.fullscreenElement === stageRef.current);
    document.addEventListener('fullscreenchange', onChange);
    return () => document.removeEventListener('fullscreenchange', onChange);
  }, [stageRef]);

  // Never leave a modifier latched on the remote desktop.
  useEffect(() => {
    const release = (): void => controller.releaseAllKeys();
    const onVisibility = (): void => {
      if (document.visibilityState === 'hidden') release();
    };
    window.addEventListener('blur', release);
    document.addEventListener('visibilitychange', onVisibility);
    return () => {
      window.removeEventListener('blur', release);
      document.removeEventListener('visibilitychange', onVisibility);
    };
  }, [controller]);

  const toggleFullscreen = useCallback(async (): Promise<void> => {
    if (document.fullscreenElement) await document.exitFullscreen();
    else await stageRef.current?.requestFullscreen();
  }, [stageRef]);

  const inputLive = rfb.phase === 'connected' && rfb.mode === 'control';
  const keysBlocked = inputLive ? null : t('stage.keys.needControl');
  const lastFrame = controller.getLastActivityAt();
  const frameAge = lastFrame > 0 ? Math.max(0, Math.round((now - lastFrame) / 1000)) : null;
  const retrySeconds = rfb.nextRetryAt !== null ? Math.max(0, Math.ceil((rfb.nextRetryAt - now) / 1000)) : null;
  const gatewayStale = pollError !== null || (lastOkAt !== null && now - lastOkAt > 15000);

  const viewControls = (
    <>
      <div className="seg" role="group" aria-label={t('stage.scale')}>
        <button type="button" className={`seg__btn ${rfb.options.fit ? 'is-on' : ''}`} aria-pressed={rfb.options.fit} onClick={() => controller.setOptions({ fit: true })}>
          <Icon name="fit" size={15} /> {t('stage.fit')}
        </button>
        <button type="button" className={`seg__btn ${!rfb.options.fit ? 'is-on' : ''}`} aria-pressed={!rfb.options.fit} onClick={() => controller.setOptions({ fit: false })}>
          <Icon name="native" size={15} /> {t('stage.native')}
        </button>
      </div>
      <Button small icon={fullscreen ? 'exitFullscreen' : 'fullscreen'} onClick={() => void toggleFullscreen()} aria-pressed={fullscreen}>
        {fullscreen ? t('stage.exitFullscreen') : t('stage.fullscreen')}
      </Button>
    </>
  );

  return (
    <section className="stage" ref={stageRef} aria-label={t('stage.label')}>
      <div className="stage__bar" role="toolbar" aria-label={t('stage.toolbar')}>
        <div className="stage__view-controls">{viewControls}</div>

        <Popover label={t('stage.display')} icon="settings">
          <div className="menu-form">
            <div className="stage__mobile-controls">{viewControls}</div>
            <label className="menu-form__row">
              <span>{t('settings.fontSize')}</span>
              <select className="input input--sm" value={fontSize} onChange={(e) => setFontSize(Number(e.target.value))}>
                {FONT_SIZES.map((size) => <option key={size} value={size}>{size} px</option>)}
              </select>
            </label>
            <p className="menu-form__note">{t('settings.fontSizeNote')}</p>
            <hr />
            <label className="menu-form__row">
              <span>{t('stage.quality')}</span>
              <input type="range" min={0} max={9} step={1} value={rfb.options.quality} onChange={(e) => controller.setOptions({ quality: Number(e.target.value) })} />
              <output className="mono">{rfb.options.quality}</output>
            </label>
            <label className="menu-form__row">
              <span>{t('stage.compression')}</span>
              <input type="range" min={0} max={9} step={1} value={rfb.options.compression} onChange={(e) => controller.setOptions({ compression: Number(e.target.value) })} />
              <output className="mono">{rfb.options.compression}</output>
            </label>
            <label className="menu-form__check">
              <input type="checkbox" checked={rfb.options.dotCursor} onChange={(e) => controller.setOptions({ dotCursor: e.target.checked })} />
              <span>{t('stage.dotCursor')}</span>
            </label>
            <p className="menu-form__note">{t('stage.qualityNote')}</p>
            <hr />
            <ResolutionPicker />
            <p className="menu-form__note">{t('stage.resolutionNote')}</p>
          </div>
        </Popover>

        <Popover label={t('stage.keys')} icon="keyboard" disabledReason={keysBlocked}>
          <div className="menu-list" role="menu">
            {KEY_COMBOS.map((c) => (
              <button key={c.id} type="button" role="menuitem" className="menu-list__item" onClick={() => controller.sendCombo(c.keys)}>
                <span>{t(c.labelKey)}</span>
                <kbd>{c.label}</kbd>
              </button>
            ))}
            <button type="button" role="menuitem" className="menu-list__item" onClick={() => controller.sendCtrlAltDel()}>
              <span>{t('combo.ctrlAltDel')}</span>
              <kbd>Ctrl+Alt+Del</kbd>
            </button>
            <hr />
            <button type="button" role="menuitem" className="menu-list__item" onClick={() => controller.releaseAllKeys()}>
              <span>{t('stage.releaseKeys')}</span>
            </button>
          </div>
        </Popover>

        <div className="stage__readout" aria-live="off">
          {rfb.phase === 'connected' && rfb.width > 0 ? (
            <span className="mono" title={t('stage.resolutionTitle')}>
              {rfb.width}×{rfb.height} · {Math.round(rfb.scale * 100)}%
            </span>
          ) : null}
          {frameAge !== null && rfb.phase === 'connected' ? (
            <span className="mono" title={t('stage.frameAgeTitle')}>
              {t('stage.frameAge', { s: frameAge })}
            </span>
          ) : null}
        </div>
      </div>

      {gatewayStale ? (
        <div className="stage__banner stage__banner--amber" role="status">
          <Icon name="warning" size={16} /> {t('stage.gatewayStale')}
        </div>
      ) : null}

      <div className="stage__frame">
        <div className="stage__screen" ref={screenRef} />

        {rfb.phase === 'connected' && rfb.mode === 'view' ? (
          <div className="stage__chip">
            <Icon name="eye" size={14} /> {t('stage.viewOnly')}
          </div>
        ) : null}

        {rfb.phase !== 'connected' ? (
          <div className="stage__overlay" role="status">
            {rfb.phase === 'connecting' ? <p>{t('stage.connecting')}</p> : null}
            {rfb.phase === 'idle' ? <p>{t('stage.idle')}</p> : null}
            {rfb.phase === 'retrying' ? (
              <>
                <p>{t('stage.retrying', { n: rfb.attempt, max: rfb.maxAttempts, s: retrySeconds ?? 0 })}</p>
                <Button variant="primary" icon="refresh" onClick={() => controller.retryNow()}>
                  {t('stage.retryNow')}
                </Button>
              </>
            ) : null}
            {rfb.phase === 'failed' ? (
              <>
                <p>{t(`stage.failure.${rfb.failure ?? 'closed'}`)}</p>
                {rfb.closeCode !== null ? <p className="mono">code {rfb.closeCode}</p> : null}
                <Button variant="primary" icon="refresh" onClick={() => controller.retryNow()}>
                  {t('stage.reconnect')}
                </Button>
              </>
            ) : null}
          </div>
        ) : null}

        {fullscreen ? (
          <div className="stage__fsbar">
            <OwnerBadge />
            {lease ? (
              <Button small variant="warn" icon="release" busy={busy} onClick={() => void release()}>
                {t('top.release')}
              </Button>
            ) : null}
            <Button small icon="exitFullscreen" onClick={() => void toggleFullscreen()}>
              {t('stage.exitFullscreen')}
            </Button>
          </div>
        ) : null}
      </div>
    </section>
  );
}
