import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import type { BrowserDestination } from '@/api/types';
import { listBrowserDestinations } from '@/api/workspace';
import { useCapture } from '@/capture/CaptureProvider';
import { Button } from '@/components/ui';
import { describeError } from '@/errors';
import { useI18n } from '@/i18n';
import { useControl } from '@/state/control';
import { useWorkspace } from '@/state/workspace';

export function WorkspaceSwitcher(): ReactNode {
  const { t } = useI18n();
  const { workspaceId } = useWorkspace();
  const { release, busy, takeover } = useControl();
  const capture = useCapture();
  const [destinations, setDestinations] = useState<BrowserDestination[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>(null);
  const [switching, setSwitching] = useState(false);
  const request = useRef<AbortController | null>(null);
  const navigating = useRef(false);

  const refresh = useCallback(async (): Promise<void> => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setLoading(true);
    try {
      const next = await listBrowserDestinations(controller.signal);
      if (!controller.signal.aborted) {
        setDestinations(next);
        setError(null);
      }
    } catch (err) {
      if (!controller.signal.aborted) setError(err);
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
    return () => request.current?.abort();
  }, [refresh]);

  const current = destinations.find((destination) => destination.current);
  const blocked = busy || capture.busy || takeover.phase !== 'idle' || switching;

  const switchTo = async (id: string): Promise<void> => {
    const destination = destinations.find((item) => item.id === id);
    if (!destination || destination.current || blocked || navigating.current) return;
    navigating.current = true;
    setSwitching(true);
    try {
      if (await release()) {
        // Each Gateway bootstraps its own session. Never carry credentials or an editor route across origins.
        window.location.assign(`${destination.origin}/`);
        return;
      }
    } catch (err) {
      setError(err);
    }
    navigating.current = false;
    setSwitching(false);
  };

  return (
    <div className="workspace-switcher">
      <label className="workspace-switcher__field" title={t('top.workspaceSwitchNote')}>
        <span>{t('top.workspace')}</span>
        <select
          className="input input--sm"
          value={current?.id ?? ''}
          disabled={blocked || (loading && destinations.length === 0)}
          aria-busy={loading || switching}
          title={current ? `${current.workspaceId} · ${current.origin}` : undefined}
          onFocus={() => void refresh()}
          onChange={(event) => void switchTo(event.target.value)}
        >
          {!current ? <option value="">{workspaceId}</option> : null}
          {destinations.map((destination) => (
            <option key={destination.id} value={destination.id}>
              {destination.name}
            </option>
          ))}
        </select>
      </label>
      {loading || switching ? <span className="sr-only" role="status">{t(switching ? 'top.workspaceSwitching' : 'top.workspaceLoading')}</span> : null}
      {error ? (
        <div className="workspace-switcher__error" role="alert">
          <span title={describeError(error, t)}>{t('top.workspaceLoadFailed')} {describeError(error, t)}</span>
          <Button small variant="ghost" icon="refresh" disabled={loading || switching} onClick={() => void refresh()}>{t('common.retry')}</Button>
        </div>
      ) : null}
    </div>
  );
}
