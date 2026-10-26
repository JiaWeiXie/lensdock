import { createContext, useCallback, useContext, useEffect, useMemo, useSyncExternalStore, type ReactNode } from 'react';
import { useToast } from '@/components/ui';
import { useI18n, type MessageKey } from '@/i18n';
import type { RfbController } from './controller';
import { RemoteClipboardSync, type RemoteClipboardFailure, type RemoteClipboardSnapshot } from './remoteClipboard';

const ENABLED_KEY = 'lensdock.remoteClipboardSync';

/** On unless the person turned it off. The preference is the only thing stored; clipboard text never is. */
function loadEnabled(): boolean {
  try {
    return localStorage.getItem(ENABLED_KEY) !== 'off';
  } catch {
    return true;
  }
}

export const REMOTE_CLIPBOARD_FAILURE_TEXT: Record<RemoteClipboardFailure, MessageKey> = {
  unfocused: 'clip.sync.unfocused',
  denied: 'clip.sync.denied',
  unavailable: 'clip.sync.unavailable',
};

interface RemoteClipboardApi extends RemoteClipboardSnapshot {
  setEnabled: (enabled: boolean) => void;
  copyLatest: () => Promise<boolean>;
}

const Ctx = createContext<RemoteClipboardSync | null>(null);

/**
 * Keeps remote-to-local clipboard sync alive for the whole DesktopProvider
 * lifetime, whether or not the clipboard panel is open. `session` is the key
 * of the current control stream, or null while only the view-only stream (which
 * carries no clipboard) or no stream is active.
 */
export function RemoteClipboardProvider({
  controller,
  session,
  children,
}: {
  controller: RfbController;
  session: string | null;
  children: ReactNode;
}): ReactNode {
  const { t } = useI18n();
  const toast = useToast();
  const sync = useMemo(
    () =>
      new RemoteClipboardSync(
        {
          writeText: typeof navigator !== 'undefined' && navigator.clipboard?.writeText ? (text) => navigator.clipboard.writeText(text) : undefined,
          hasFocus: () => document.hasFocus(),
        },
        loadEnabled(),
      ),
    [],
  );

  useEffect(() => {
    sync.onFailure = (reason) => toast.push('warn', t(REMOTE_CLIPBOARD_FAILURE_TEXT[reason]));
    return () => {
      sync.onFailure = null;
    };
  }, [sync, toast, t]);

  // Bind before the controller switches streams so no event of the new stream meets the old key.
  // Declared before the subscription effect: StrictMode runs every cleanup, then every setup in
  // order, so the unmount-time bind(null) is always followed by this bind(session) again.
  useEffect(() => {
    sync.bind(session);
  }, [sync, session]);

  useEffect(() => {
    const unsubscribe = controller.subscribeClipboard((text) => sync.receive(text));
    return () => {
      unsubscribe();
      sync.bind(null);
    };
  }, [controller, sync]);

  return <Ctx.Provider value={sync}>{children}</Ctx.Provider>;
}

export function useRemoteClipboard(): RemoteClipboardApi {
  const sync = useContext(Ctx);
  if (!sync) throw new Error('useRemoteClipboard outside RemoteClipboardProvider');
  const snapshot = useSyncExternalStore(sync.subscribe, sync.getSnapshot);
  const setEnabled = useCallback(
    (enabled: boolean) => {
      sync.setEnabled(enabled);
      try {
        localStorage.setItem(ENABLED_KEY, enabled ? 'on' : 'off');
      } catch {
        // The choice still holds for this page view when storage is unavailable.
      }
    },
    [sync],
  );
  const copyLatest = useCallback(() => sync.copyLatest(), [sync]);
  return useMemo(() => ({ ...snapshot, setEnabled, copyLatest }), [snapshot, setEnabled, copyLatest]);
}
