import { useMemo, useRef, type ReactNode } from 'react';
import { Notice } from '@/components/ui';
import { AgentStateBanner } from '@/agent/AgentControlButton';
import { useAgentControl } from '@/agent/AgentControlProvider';
import { useCapture } from '@/capture/CaptureProvider';
import { DesktopStage } from '@/desktop/DesktopStage';
import { Dock } from '@/dock/Dock';
import { describeError } from '@/errors';
import { useI18n } from '@/i18n';
import { Drawer } from '@/shell/Drawer';
import { TakeoverDialog } from '@/shell/TakeoverDialog';
import { TopBar } from '@/shell/TopBar';
import { useShortcuts } from '@/shell/useShortcuts';
import { useControl } from '@/state/control';
import { useSession } from '@/state/session';
import { useUi } from '@/state/ui';
import { useWorkspace } from '@/state/workspace';
import { useRfbController } from '@/vnc/DesktopProvider';

/** The three-zone workspace: top control bar, live desktop, feedback dock; rail on the left. */
export function WorkspaceView(): ReactNode {
  const { t } = useI18n();
  const { actor } = useSession();
  const { workspace, error } = useWorkspace();
  const { take, lease, release } = useControl();
  const capture = useCapture();
  const agent = useAgentControl();
  const { shortcuts, setDockCollapsed, dockCollapsed, takeoverOpen, setTakeoverOpen } = useUi();
  const controller = useRfbController();
  const stage = useRef<HTMLDivElement | null>(null);

  const actions = useMemo(
    () => ({
      toggleControl: (): void => {
        if (lease) void release();
        else if (workspace?.controlState === 'IDLE') void take('');
        else setTakeoverOpen(true);
      },
      captureDesktop: (): void => {
        if (agent.collaborationVisible) void capture.annotateDesktop();
      },
      toggleFullscreen: (): void => {
        if (document.fullscreenElement) void document.exitFullscreen();
        else void stage.current?.requestFullscreen();
      },
      toggleFit: (): void => controller.setOptions({ fit: !controller.getSnapshot().options.fit }),
      toggleDock: (): void => setDockCollapsed(!dockCollapsed),
    }),
    [lease, release, take, capture, controller, stage, dockCollapsed, setDockCollapsed, workspace, setTakeoverOpen, agent.collaborationVisible],
  );
  useShortcuts(shortcuts, actions);

  const agentOnly = actor?.role === 'agent';

  return (
    <>
      <TopBar />
      <div className="workspace">
        <div className="workspace__notices">
          {agentOnly && agent.collaborationVisible ? (
            <Notice tone="neutral" title={t('ws.agentRole')}>
              {t('ws.agentRoleBody')}
            </Notice>
          ) : null}
          <AgentStateBanner context="workspace" />
          {error ? (
            <Notice tone="danger" title={t('ws.gatewayError')}>
              {describeError(error, t)}
            </Notice>
          ) : null}
          {workspace?.locked || workspace?.controlState === 'ERROR_LOCKED' ? (
            <Notice tone="danger" title={t('ws.locked')}>
              {workspace.lockCode ? `${workspace.lockCode}. ` : ''}
              {t('ws.lockedBody')}
            </Notice>
          ) : null}
          {workspace?.controlState === 'MAINTENANCE' ? <Notice tone="amber">{t('ws.maintenance')}</Notice> : null}
          {workspace?.controlState === 'HANDOFF_PENDING' ? <Notice tone="amber">{t('ws.handoff')}</Notice> : null}
          {workspace && !workspace.ready ? <Notice tone="amber">{t('ws.notReady')}</Notice> : null}
        </div>

        <div className={`workspace__main${dockCollapsed ? ' workspace__main--dock-collapsed' : ''}`}>
          <DesktopStage stageRef={stage} />
          <Dock />
        </div>
        <Drawer />
        <TakeoverDialog open={takeoverOpen} onClose={() => setTakeoverOpen(false)} />
      </div>
    </>
  );
}
