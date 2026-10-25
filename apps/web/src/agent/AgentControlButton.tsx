import type { ReactNode } from 'react';
import { Button, Notice } from '@/components/ui';
import { useI18n } from '@/i18n';
import { useAgentControl } from './AgentControlProvider';
import { useAgentText, useDescribeAgentError } from './messages';

export type AgentControlVariant = 'toolbar' | 'compact' | 'panel';

/**
 * The one pause/resume switch, shared by the workspace top bar, Settings and the annotation editor.
 *
 * - Pause is immediate: one click, no confirmation.
 * - Resume is always an explicit click; closing a screenshot or editor never resumes.
 * - Renders nothing in noVNC-only mode, while the policy is still loading, and for roles that
 *   cannot change it (viewer, agent). While the policy is unknown it stays visible but disabled.
 */
export function AgentControlButton({ variant = 'toolbar' }: { variant?: AgentControlVariant }): ReactNode {
  const agent = useAgentControl();
  const at = useAgentText();

  if (agent.policy.kind === 'loading' || agent.collaboration === 'disabled' || !agent.canControl) return null;

  const small = variant !== 'panel';
  const cls = `agent-ctl agent-ctl--${variant}`;

  if (agent.collaboration === 'unknown') {
    return (
      <Button small={small} className={`${cls} agent-ctl--unknown`} icon="warning" disabledReason={at('ctl.unknownReason')}>
        {at('ctl.unknown')}
      </Button>
    );
  }

  const otherBusy = agent.busy !== null;
  if (agent.paused) {
    return (
      <Button
        small={small}
        variant="primary"
        icon="play"
        className={`${cls} agent-ctl--resume`}
        busy={agent.busy === 'resume'}
        disabledReason={otherBusy && agent.busy !== 'resume' ? at('ctl.otherBusy') : null}
        title={at('ctl.resumeHint')}
        onClick={() => void agent.resume()}
      >
        {agent.busy === 'resume' ? at('ctl.resuming') : at('ctl.resume')}
      </Button>
    );
  }
  return (
    <Button
      small={small}
      variant="danger"
      icon="pause"
      className={`${cls} agent-ctl--pause`}
      busy={agent.busy === 'pause'}
      disabledReason={otherBusy && agent.busy !== 'pause' ? at('ctl.otherBusy') : null}
      title={at('ctl.pauseHint')}
      onClick={() => void agent.pause()}
    >
      {agent.busy === 'pause' ? at('ctl.pausing') : at('ctl.pause')}
    </Button>
  );
}

/**
 * Banner for the paused state, the unknown-policy state and the last failed pause/resume/mode
 * change. Shows nothing in a healthy running or noVNC-only state, so noVNC has no agent chrome.
 */
export function AgentStateBanner({ context = 'workspace' }: { context?: 'workspace' | 'editor' | 'settings' }): ReactNode {
  const agent = useAgentControl();
  const { t } = useI18n();
  const at = useAgentText();
  const describe = useDescribeAgentError();
  const failure = agent.error ? (
    <Notice
      tone="danger"
      title={agent.error.action === 'pause' ? at('fail.pause') : agent.error.action === 'resume' ? at('fail.resume') : at('fail.mode')}
      actions={<Button small variant="ghost" icon="x" aria-label={t('common.dismiss')} onClick={agent.clearError} />}
    >
      {describe(agent.error.error)}
    </Notice>
  ) : null;

  if (agent.policy.kind === 'unknown') {
    // The settings page carries its own, fuller explanation.
    if (context === 'settings') return failure;
    return (
      <>
        <Notice tone="amber" title={at('banner.unknown.title')}>
          {context === 'editor' ? at('banner.unknown.editor') : at('banner.unknown.body')}
        </Notice>
        {failure}
      </>
    );
  }

  if (agent.collaboration === 'enabled' && agent.paused) {
    return (
      <>
        <div className="agent-banner agent-banner--paused">
          <Notice
            tone="amber"
            title={at('banner.paused.title')}
          >
            <p>{at('banner.paused.body')}</p>
            <p>{at('banner.paused.noConfirm')}</p>
            <p>{agent.canControl ? at('banner.paused.resumeNote') : at('banner.paused.viewer')}</p>
          </Notice>
        </div>
        {failure}
      </>
    );
  }
  return failure;
}
