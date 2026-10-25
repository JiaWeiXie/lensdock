import { type ReactNode } from 'react';
import { ToastProvider } from '@/components/ui';
import { AgentControlProvider } from '@/agent/AgentControlProvider';
import { EditorRouteGuard } from '@/agent/EditorRouteGuard';
import { I18nProvider, useI18n } from '@/i18n';
import { SettingsView } from '@/settings/SettingsView';
import { OwnerRail } from '@/shell/OwnerRail';
import { SessionProvider, useSession } from '@/state/session';
import { useRoute } from '@/state/ui';
import { Login } from '@/views/Login';
import { WorkspaceView } from '@/views/WorkspaceView';
import { CaptureProvider } from '@/capture/CaptureProvider';
import { ControlProvider } from '@/state/control';
import { TargetsProvider } from '@/state/targets';
import { UiProvider } from '@/state/ui';
import { WorkspaceProvider } from '@/state/workspace';
import { DesktopProvider } from '@/vnc/DesktopProvider';

export function App(): ReactNode {
  return (
    <I18nProvider>
      <ToastProvider>
        <SessionProvider>
          <UiProvider>
            <Shell />
          </UiProvider>
        </SessionProvider>
      </ToastProvider>
    </I18nProvider>
  );
}

function Shell(): ReactNode {
  const { t } = useI18n();
  const { status, actor } = useSession();

  if (status === 'loading') {
    return (
      <main className="boot" id="main">
        <p role="status">{t('boot.loading')}</p>
      </main>
    );
  }
  if (status === 'anonymous' || status === 'unreachable' || !actor) return <Login />;

  return (
    <WorkspaceProvider workspaceId={actor.workspaceId}>
      <AgentControlProvider>
        <TargetsProvider>
          <ControlProvider>
            <DesktopProvider>
              <CaptureProvider>
                <a className="skip-link" href="#main">
                  {t('a11y.skip')}
                </a>
                <div className="app">
                  <OwnerRail />
                  <Routed />
                </div>
              </CaptureProvider>
            </DesktopProvider>
          </ControlProvider>
        </TargetsProvider>
      </AgentControlProvider>
    </WorkspaceProvider>
  );
}

function Routed(): ReactNode {
  const [route] = useRoute();
  const { t } = useI18n();
  return (
    <div className={`app__main${route.name === 'workspace' ? ' app__main--workspace' : ''}`} id="main">
      {route.name === 'workspace' ? <WorkspaceView /> : null}
      {route.name === 'settings' ? <SettingsView /> : null}
      {route.name === 'editor' ? <EditorRouteGuard feedbackId={route.feedbackId} /> : null}
      {route.name === 'workspace' ? null : (
        <p className="route-note">
          <a href="#/">{t('nav.backToWorkspace')}</a>
        </p>
      )}
    </div>
  );
}

