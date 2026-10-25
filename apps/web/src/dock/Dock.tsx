import { useEffect, type ReactNode } from 'react';
import { Button, Tabs, tabPanelId } from '@/components/ui';
import { useCapture } from '@/capture/CaptureProvider';
import { useI18n } from '@/i18n';
import { useAgentControl } from '@/agent/AgentControlProvider';
import { useUi, type DockTab } from '@/state/ui';
import { CapturePanel } from './CapturePanel';
import { ClipboardBridge } from './ClipboardBridge';
import { FeedbackPanel } from './FeedbackPanel';
import { FilesPanel } from './FilesPanel';

/** Feedback dock: capture and drafts, published feedback with delivery state, and transfer tools. */
export function Dock(): ReactNode {
  const { t } = useI18n();
  const { dockTab: storedTab, setDockTab, dockCollapsed, setDockCollapsed, selected } = useUi();
  const capture = useCapture();
  const { collaborationVisible } = useAgentControl();
  // Without collaboration only ordinary clipboard/file transfer remains; the stored tab is not rewritten.
  const dockTab: DockTab = collaborationVisible ? storedTab : 'transfer';

  // A finished capture always brings the person to its next step.
  useEffect(() => {
    if (capture.result && collaborationVisible) setDockTab('capture');
  }, [capture.result, collaborationVisible, setDockTab]);

  // Opening a feedback from anywhere switches to the feedback tab.
  useEffect(() => {
    if (selected && collaborationVisible) setDockTab('feedback');
  }, [selected, collaborationVisible, setDockTab]);

  if (dockCollapsed) {
    return (
      <aside className="dock dock--collapsed" aria-label={t('dock.label')}>
        <Button small icon="chevronUp" aria-label={t('dock.expand')} title={t('dock.expand')} onClick={() => setDockCollapsed(false)} />
        {collaborationVisible ? (
          <>
            <Button small icon="camera" aria-label={t('dock.tab.capture')} title={t('dock.tab.capture')} onClick={() => setDockTab('capture')} />
            <Button small icon="file" aria-label={t('dock.tab.feedback')} title={t('dock.tab.feedback')} onClick={() => setDockTab('feedback')} />
          </>
        ) : null}
        <Button small icon="clipboard" aria-label={t('dock.tab.transfer')} title={t('dock.tab.transfer')} onClick={() => setDockTab('transfer')} />
      </aside>
    );
  }

  return (
    <aside className="dock" aria-label={t('dock.label')}>
      <div className="dock__head">
        <Tabs<DockTab>
          group="dock"
          label={t('dock.tabs')}
          value={dockTab}
          onChange={setDockTab}
          items={
            collaborationVisible
              ? [
                  { id: 'capture', label: t('dock.tab.capture') },
                  { id: 'feedback', label: t('dock.tab.feedback') },
                  { id: 'transfer', label: t('dock.tab.transfer') },
                ]
              : [{ id: 'transfer', label: t('dock.tab.transfer') }]
          }
        />
        <Button small variant="ghost" icon="chevronDown" aria-label={t('dock.collapse')} onClick={() => setDockCollapsed(true)} />
      </div>
      <div className="dock__body" role="tabpanel" id={tabPanelId('dock', dockTab)} aria-labelledby={`dock-tab-${dockTab}`} tabIndex={-1}>
        {dockTab === 'capture' ? <CapturePanel /> : null}
        {dockTab === 'feedback' ? <FeedbackPanel /> : null}
        {dockTab === 'transfer' ? (
          <div className="panel-stack">
            <ClipboardBridge />
            <FilesPanel />
          </div>
        ) : null}
      </div>
    </aside>
  );
}
