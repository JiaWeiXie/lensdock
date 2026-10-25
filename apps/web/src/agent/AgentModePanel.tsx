import { useCallback, useEffect, useId, useRef, useState, type ReactNode } from 'react';
import type { ApiError } from '@/api/http';
import { getAgentSetup } from '@/api/workspace';
import { Button, Chip, Notice, useToast, type Tone } from '@/components/ui';
import { describeError } from '@/errors';
import { useI18n } from '@/i18n';
import { AgentControlButton, AgentStateBanner } from './AgentControlButton';
import { useWorkspace } from '@/state/workspace';
import { formatDateTime } from '@/util/format';
import { useAgentControl } from './AgentControlProvider';
import { useAgentText } from './messages';

/**
 * Settings: the usage-mode switch, a read-only state viewer for everyone, and, in Agent mode, the
 * English onboarding prompt. The switch acts immediately (no modal) and says what it disconnects.
 */
export function AgentModePanel(): ReactNode {
  const agent = useAgentControl();
  const at = useAgentText();
  const { locale } = useI18n();
  const hintId = useId();
  const labelId = useId();
  const control = agent.policy.kind === 'known' ? agent.policy.control : null;
  const on = control?.mode === 'AGENT';

  const switchReason = !control
    ? at('switch.unknownReason')
    : !agent.canControl
      ? at('switch.roleReason')
      : agent.busy !== null && agent.busy !== 'mode'
        ? at('switch.otherBusy')
        : null;
  const switchBusy = agent.busy === 'mode';

  const stateText = !control
    ? at('state.unknown')
    : control.mode === 'NOVNC'
      ? at('state.off')
      : control.paused
        ? at('state.paused')
        : at('state.running');
  const stateTone: Tone = !control ? 'amber' : control.mode === 'NOVNC' ? 'neutral' : control.paused ? 'amber' : 'ok';

  return (
    <>
      <section className="panel agent-mode" aria-labelledby={`${labelId}-title`}>
        <h2 id={`${labelId}-title`}>{at('panel.title')}</h2>
        <p className="panel__desc">{at('panel.intro')}</p>

        {agent.policy.kind === 'unknown' ? <Notice tone="amber" title={at('banner.unknown.title')}>{at('panel.unknown')}</Notice> : null}
        {agent.policy.kind === 'loading' ? <p role="status">{at('panel.loading')}</p> : null}

        <dl className="kv">
          <div className="kv__row">
            <dt>{at('panel.current')}</dt>
            <dd>{control ? at(`mode.${control.mode}`) : at('state.unknown')}</dd>
          </div>
          <div className="kv__row">
            <dt>{at('panel.agentState')}</dt>
            <dd>
              <Chip tone={stateTone} icon={control?.mode === 'AGENT' && !control.paused ? 'check' : 'warning'}>
                {stateText}
              </Chip>
            </dd>
          </div>
          {control ? (
            <>
              <div className="kv__row">
                <dt>{at('panel.updated')}</dt>
                <dd className="mono">{formatDateTime(control.updatedAt, locale)}</dd>
              </div>
              <div className="kv__row">
                <dt>{at('panel.revision')}</dt>
                <dd className="mono">{control.revision}</dd>
              </div>
            </>
          ) : null}
        </dl>

        {agent.canControl ? (
          <div className="agent-switch-row">
            <div className="agent-switch-row__text">
              <span id={labelId} className="agent-switch-row__label">
                {at('switch.label')}
              </span>
              <p id={hintId} className="field__hint">
                {!control ? at('panel.unknown') : on ? at('switch.hintOn') : at('switch.hintOff')}
              </p>
            </div>
            <button
              type="button"
              role="switch"
              aria-checked={control ? on : false}
              aria-labelledby={labelId}
              aria-describedby={hintId}
              className={`agent-switch${on ? ' is-on' : ''}${switchBusy ? ' is-busy' : ''}`}
              disabled={switchReason !== null || switchBusy}
              title={switchReason ?? undefined}
              onClick={() => void agent.setMode(on ? 'NOVNC' : 'AGENT')}
            >
              <span className="agent-switch__track" aria-hidden="true">
                <span className="agent-switch__thumb" />
              </span>
              <span className="agent-switch__state">
                {switchBusy ? (on ? at('switch.busyOff') : at('switch.busyOn')) : on ? at('switch.on') : at('switch.off')}
              </span>
            </button>
          </div>
        ) : control ? (
          <p className="panel__desc">{at('panel.viewerOnly')}</p>
        ) : null}

        {on ? (
          <div className="agent-mode__pause">
            <AgentControlButton variant="panel" />
          </div>
        ) : null}
        <AgentStateBanner context="settings" />
      </section>

      {on ? <AgentSetupPrompt /> : null}
    </>
  );
}

/** The Gateway-generated English prompt, fetched on demand, shown read-only with one-click copy. */
function AgentSetupPrompt(): ReactNode {
  const { t } = useI18n();
  const at = useAgentText();
  const toast = useToast();
  const { workspaceId } = useWorkspace();
  const areaId = useId();
  const area = useRef<HTMLTextAreaElement | null>(null);
  const [prompt, setPrompt] = useState<string | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(
    async (signal?: AbortSignal): Promise<void> => {
      setLoading(true);
      try {
        const res = await getAgentSetup(workspaceId, signal);
        if (signal?.aborted) return;
        setPrompt(res.prompt);
        setError(null);
      } catch (err) {
        if (signal?.aborted) return;
        setError(err as ApiError);
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [workspaceId],
  );

  useEffect(() => {
    const ctl = new AbortController();
    void load(ctl.signal);
    return () => ctl.abort();
  }, [load]);

  const copy = (): void => {
    if (prompt === null) return;
    const fallback = (): void => {
      area.current?.focus();
      area.current?.select();
      toast.push('warn', at('setup.copyFailed'));
    };
    if (!navigator.clipboard) {
      fallback();
      return;
    }
    navigator.clipboard.writeText(prompt).then(() => toast.push('ok', at('setup.copied')), fallback);
  };

  return (
    <section className="panel agent-setup" aria-labelledby={`${areaId}-title`}>
      <h2 id={`${areaId}-title`}>{at('setup.title')}</h2>
      <p className="panel__desc">{at('setup.intro')}</p>
      <ul className="agent-setup__facts">
        <li>{at('setup.needs')}</li>
        <li>{at('setup.noSecrets')}</li>
      </ul>

      {error ? (
        <Notice
          tone="danger"
          title={at('setup.failed')}
          actions={
            <Button small icon="refresh" busy={loading} onClick={() => void load()}>
              {at('setup.reload')}
            </Button>
          }
        >
          {describeError(error, t)}
        </Notice>
      ) : null}
      {loading && prompt === null && !error ? <p role="status">{at('setup.loading')}</p> : null}

      {prompt !== null ? (
        <>
          <label className="field" htmlFor={areaId}>
            <span className="field__label">{at('setup.label')}</span>
          </label>
          <textarea
            id={areaId}
            ref={area}
            className="input agent-setup__text mono"
            readOnly
            spellCheck={false}
            lang="en"
            dir="ltr"
            value={prompt}
            rows={14}
            onFocus={(e) => e.currentTarget.select()}
          />
          <div className="agent-setup__actions">
            <Button variant="primary" icon="copy" onClick={copy}>
              {at('setup.copy')}
            </Button>
            <Button small icon="refresh" busy={loading} onClick={() => void load()}>
              {at('setup.reload')}
            </Button>
          </div>
        </>
      ) : null}
    </section>
  );
}
