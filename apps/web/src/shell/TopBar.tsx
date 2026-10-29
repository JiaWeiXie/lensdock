import { useState, type ReactNode } from 'react';
import { Chip, Button, type Tone } from '@/components/ui';
import { Icon } from '@/components/icons';
import { Popover } from '@/components/Popover';
import { CAPTURE_STEP_KEYS, useCapture } from '@/capture/CaptureProvider';
import { DrawingButton } from '@/capture/DrawingButton';
import { AgentControlButton } from '@/agent/AgentControlButton';
import { useAgentControl } from '@/agent/AgentControlProvider';
import { describeError } from '@/errors';
import { useI18n, type Lang } from '@/i18n';
import { useControl } from '@/state/control';
import { useSession } from '@/state/session';
import { useTargets } from '@/state/targets';
import { useRoute, useUi } from '@/state/ui';
import { useWorkspace } from '@/state/workspace';
import { shortId } from '@/util/format';
import { useRfb } from '@/vnc/DesktopProvider';
import { OwnerGlyph, useOwner } from './owner';
import { WorkspaceSwitcher } from './WorkspaceSwitcher';

/** Top control zone: identity, health, controller, target, and the primary actions. */
export function TopBar(): ReactNode {
  const { t, lang, setLang } = useI18n();
  const { actor, logout } = useSession();
  const { workspace, feedStatus } = useWorkspace();
  const { lease, release, busy, error: controlError } = useControl();
  const owner = useOwner();
  const rfb = useRfb();
  const capture = useCapture();
  const { collaborationVisible: collaboration } = useAgentControl();
  const { setDockTab } = useUi();
  const [route, navigate] = useRoute();
  const [toolsExpanded, setToolsExpanded] = useState(false);

  const connection: { tone: Tone; text: string } = (() => {
    switch (rfb.phase) {
      case 'connected':
        return rfb.mode === 'control'
          ? { tone: 'teal', text: t('top.conn.control') }
          : { tone: 'neutral', text: t('top.conn.view') };
      case 'connecting':
        return { tone: 'amber', text: t('top.conn.connecting') };
      case 'retrying':
        return { tone: 'amber', text: t('top.conn.retrying', { n: rfb.attempt, max: rfb.maxAttempts }) };
      case 'failed':
        return { tone: 'danger', text: t('top.conn.failed') };
      default:
        return { tone: 'neutral', text: t('top.conn.none') };
    }
  })();

  const health: { tone: Tone; text: string } = !workspace
    ? { tone: 'amber', text: t('top.health.unknown') }
    : workspace.locked || workspace.controlState === 'ERROR_LOCKED'
      ? { tone: 'danger', text: t('top.health.locked') }
      : workspace.ready
        ? { tone: 'ok', text: t('top.health.ready') }
        : { tone: 'amber', text: t('top.health.notReady') };

  const stepKey = CAPTURE_STEP_KEYS[capture.step];

  return (
    <header className={`top${toolsExpanded ? ' top--expanded' : ' top--compact'}`}>
      <div className="top__row top__row--identity">
        <div className="top__identity">
          <div className="top__id">
            <span className="top__brand">LensDock</span>
            <span className="top__browser" title={workspace?.browserVersion ?? undefined}>
              {workspace?.browserVersion ?? t('top.browserUnknown')}
            </span>
          </div>
          <WorkspaceSwitcher />
        </div>

        <div className="top__status" role="group" aria-label={t('top.status')}>
          <Chip tone={health.tone} icon={health.tone === 'ok' ? 'check' : 'warning'}>
            {health.text}
          </Chip>
          <Chip tone={connection.tone} icon={rfb.phase === 'connected' ? 'link' : 'plug'}>
            {connection.text}
          </Chip>
          <span className="chip chip--owner" data-state={owner.state}>
            <OwnerGlyph state={owner.state} size={16} />
            {t(owner.labelKey)}
          </span>
          {collaboration && feedStatus === 'reconnecting' ? <Chip tone="amber" icon="warning">{t('top.events.polling')}</Chip> : null}
        </div>
      </div>
      <div className="top__row top__row--commands">
        <div className="top__primary">
          {lease ? (
            <Button small variant="warn" icon="release" busy={busy} onClick={() => void release()}>
              {t('top.release')}
            </Button>
          ) : null}
          <AgentControlButton variant="toolbar" />
          {collaboration ? (
            <>
              <DrawingButton />
              <Button
                className="screenshot-launch"
                icon="camera"
                busy={capture.busy}
                disabledReason={capture.desktopBlocked ? t(capture.desktopBlocked) : null}
                onClick={() => void capture.annotateDesktop()}
              >
                {t('top.captureAnnotate')}
              </Button>
            </>
          ) : null}
        </div>

        <div className="top__tools">
          <Button small icon={toolsExpanded ? 'chevronUp' : 'chevronDown'} aria-expanded={toolsExpanded} aria-controls="workspace-tools" onClick={() => setToolsExpanded((value) => !value)}>
            {t(toolsExpanded ? 'top.collapseTools' : 'top.expandTools')}
          </Button>
          <label className="lang">
            <span className="sr-only">{t('settings.language')}</span>
            <select className="input input--sm" value={lang} onChange={(e) => setLang(e.target.value as Lang)}>
              <option value="zh-TW">繁體中文</option>
              <option value="en-US">English</option>
            </select>
          </label>
          <Button small icon={route.name === 'settings' ? 'back' : 'settings'} onClick={() => navigate(route.name === 'settings' ? { name: 'workspace' } : { name: 'settings' })}>
            {route.name === 'settings' ? t('top.backToWorkspace') : t('top.settings')}
          </Button>
          <Popover label={actor ? shortId(actor.id, 14) : t('top.account')} icon="lock" align="end">
            <div className="menu-form">
              <p>
                <strong className="mono">{actor?.id}</strong>
              </p>
              <p className="menu-form__note">
                {t('top.role')}: {actor ? t(`role.${actor.role}`) : '—'}
              </p>
              <Button small variant="secondary" onClick={() => void logout()}>
                {t('top.logout')}
              </Button>
            </div>
          </Popover>
        </div>
      </div>

      <div id="workspace-tools" className="top__row top__row--actions" hidden={!toolsExpanded}>
        {collaboration ? (
        <div className="top__actions">
          <Button
            icon="camera"
            busy={capture.busy}
            disabledReason={capture.desktopBlocked ? t(capture.desktopBlocked) : null}
            onClick={() => void capture.annotateDesktop()}
          >
            {t('top.captureDesktop')}
          </Button>
          <Button
            icon="file"
            onClick={() => {
              setDockTab('capture');
              navigate({ name: 'workspace' });
            }}
          >
            {t('top.capturePage')}
          </Button>
        </div>
        ) : null}
        <TargetStrip />
      </div>
      {capture.busy && stepKey ? (
        <p className="sr-only" role="status">
          {t(stepKey)}
        </p>
      ) : null}
      {collaboration && capture.error && capture.errorSource === 'desktop' ? (
        <div className="capture-error capture-error--top" role="alert">
          <span className="capture-error__text">
            <strong>{t('capture.failed')}</strong> {capture.error}
          </span>
          <Button
            small
            icon="refresh"
            busy={capture.busy}
            disabledReason={capture.desktopBlocked && !capture.pendingDraft ? t(capture.desktopBlocked) : null}
            onClick={() => void capture.annotateDesktop()}
          >
            {t('common.retry')}
          </Button>
          <Button small variant="ghost" disabled={capture.busy} onClick={capture.discardDesktop}>
            {capture.pendingDraft ? t('capture.result.discard') : t('common.dismiss')}
          </Button>
        </div>
      ) : null}
      {controlError ? <p className="top__error" role="alert">{describeError(controlError, t)}</p> : null}
    </header>
  );
}

/** Which page the human sees vs. the Agent's, stated only as far as the runtime can actually tell. */
function TargetStrip(): ReactNode {
  const { t } = useI18n();
  const { targets, error, loaded } = useTargets();
  const { workspace } = useWorkspace();
  const visible = targets.filter((x) => x.visible === true);
  const agent = workspace?.controlState === 'AGENT';

  let warning: string | null = null;
  if (loaded && !error && workspace?.ready) {
    if (targets.length > 0 && visible.length === 0) warning = t('target.noneVisible');
    else if (visible.length > 1) warning = t('target.multipleVisible', { n: visible.length });
    else if (agent && targets.length > 1) warning = t('target.agentUnknown', { title: visible[0]?.title || visible[0]?.url || '—' });
  }

  return (
    <div className="targets">
      <Popover label={t('target.summary', { n: targets.length })} icon="file">
        <div className="menu-form targets__list">
          {error ? <p className="field__error">{error.message}</p> : null}
          {targets.length === 0 && !error ? <p className="menu-form__note">{t('target.empty')}</p> : null}
          <ul>
            {targets.map((x) => (
              <li key={x.id}>
                <span className={`pill ${x.visible ? 'pill--on' : ''}`}>{x.visible === true ? t('target.foreground') : x.visible === false ? t('target.background') : t('target.unknownVisible')}</span>
                <span className="targets__title">{x.title || '—'}</span>
                <span className="targets__url mono" title={x.url}>
                  {x.url}
                </span>
              </li>
            ))}
          </ul>
        </div>
      </Popover>
      {visible.length === 1 ? (
        <span className="targets__fg" title={visible[0]?.url}>
          <Icon name="eye" size={14} /> {visible[0]?.title || visible[0]?.url}
        </span>
      ) : null}
      {warning ? (
        <span className="targets__warn" role="status">
          <Icon name="warning" size={14} /> {warning}
        </span>
      ) : null}
    </div>
  );
}
