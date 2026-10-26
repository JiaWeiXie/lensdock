import { useEffect, useRef, useState, type ReactNode } from 'react';
import { putClipboard } from '@/api/workspace';
import { Button, Field, Notice } from '@/components/ui';
import { describeError } from '@/errors';
import { useI18n } from '@/i18n';
import { useControl } from '@/state/control';
import { useWorkspace } from '@/state/workspace';
import { graphemeLength, utf8ByteLength } from '@/util/text';
import { XK } from '@/vnc/keys';
import { useClock, useRfb, useRfbController } from '@/vnc/DesktopProvider';
import { REMOTE_CLIPBOARD_FAILURE_TEXT, useRemoteClipboard } from '@/vnc/RemoteClipboardProvider';

/** Selection lifetime (60 s). The server enforces it; text size is limited only when the administrator configures a limit. */
const SELECTION_TTL_S = 60;

/**
 * UTF-8 clipboard bridge. Local to remote is manual: VNC's own clipboard is
 * Latin-1 only, so text composed with a local IME is sent over the runtime X11
 * clipboard bridge, and nothing is read from the local clipboard unless the
 * person presses the explicit button. Remote to local is the opt-out sync
 * owned by RemoteClipboardProvider (it runs with this panel closed); this panel
 * only shows its toggle, status and the explicit copy fallback. Text is never
 * logged or stored by the UI.
 */
export function ClipboardBridge(): ReactNode {
  const { t } = useI18n();
  const { workspaceId } = useWorkspace();
  const { lease } = useControl();
  const controller = useRfbController();
  const rfb = useRfb();
  const now = useClock();
  const remote = useRemoteClipboard();
  const [text, setText] = useState('');
  const [clearAfter, setClearAfter] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [sentAt, setSentAt] = useState<number | null>(null);
  const [localNote, setLocalNote] = useState<string | null>(null);
  const pasteTimer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(pasteTimer.current), []);

  const inputLive = rfb.phase === 'connected' && rfb.mode === 'control';
  const bytes = utf8ByteLength(text);
  const disabledReason = inputLive ? (text.length === 0 ? t('clip.empty') : null) : t('clip.needControl');
  const remaining = sentAt !== null ? Math.max(0, SELECTION_TTL_S - Math.floor((now - sentAt) / 1000)) : null;

  const readLocal = async (): Promise<void> => {
    setLocalNote(null);
    try {
      const s = await navigator.clipboard.readText();
      setText(s);
    } catch {
      setLocalNote(t('clip.readDenied'));
    }
  };

  const send = async (thenPaste: boolean): Promise<void> => {
    setBusy(true);
    setError(null);
    try {
      if (!lease) throw new Error(t('clip.needControl'));
      await putClipboard(workspaceId, text, lease);
      setSentAt(Date.now());
      if (clearAfter) setText('');
      if (thenPaste) {
        // One explicit Ctrl+V into whatever remote field has focus; no text is replayed key by key.
        controller.focus();
        pasteTimer.current = window.setTimeout(() => controller.sendCombo([XK.Control_L, XK.v]), 120);
      }
    } catch (err) {
      setError(describeError(err, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="panel">
      <h3>{t('clip.title')}</h3>
      <p className="panel__desc">{t('clip.desc')}</p>
      <Field
        label={t('clip.label')}
        htmlFor="clip-text"
        hint={t('clip.count', { g: graphemeLength(text), b: bytes })}
      >
        <textarea
          id="clip-text"
          className="input input--mono"
          rows={4}
          lang="zh-TW"
          spellCheck={false}
          autoComplete="off"
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder={t('clip.placeholder')}
        />
      </Field>
      <label className="check">
        <input type="checkbox" checked={clearAfter} onChange={(e) => setClearAfter(e.target.checked)} />
        <span>{t('clip.clearAfter')}</span>
      </label>
      <div className="btn-row">
        <Button small icon="clipboard" onClick={() => void readLocal()}>
          {t('clip.readLocal')}
        </Button>
        <Button small icon="send" variant="secondary" busy={busy} disabledReason={disabledReason} onClick={() => void send(false)}>
          {t('clip.send')}
        </Button>
        <Button small icon="send" variant="primary" busy={busy} disabledReason={disabledReason} onClick={() => void send(true)}>
          {t('clip.sendPaste')}
        </Button>
        <Button small variant="ghost" onClick={() => setText('')} disabled={text.length === 0}>
          {t('clip.clear')}
        </Button>
      </div>
      {localNote ? <Notice tone="amber">{localNote}</Notice> : null}
      {error ? <Notice tone="danger">{error}</Notice> : null}
      {remaining !== null && remaining > 0 ? (
        <Notice tone="teal">{t('clip.sent', { s: remaining })}</Notice>
      ) : sentAt !== null ? (
        <Notice tone="neutral">{t('clip.expired')}</Notice>
      ) : null}
      <h3>{t('clip.sync.title')}</h3>
      <label className="check">
        <input type="checkbox" checked={remote.enabled} onChange={(e) => remote.setEnabled(e.target.checked)} />
        <span>{t('clip.sync.toggle')}</span>
      </label>
      <p className="panel__desc">{remote.enabled ? t('clip.sync.hint') : t('clip.sync.off')}</p>
      {remote.status === 'copied' ? <Notice tone="teal">{t('clip.sync.copied')}</Notice> : null}
      {remote.status === 'unfocused' || remote.status === 'denied' || remote.status === 'unavailable' ? (
        <Notice tone="amber">{t(REMOTE_CLIPBOARD_FAILURE_TEXT[remote.status])}</Notice>
      ) : null}
      {remote.fallbackText !== null ? (
        <Field label={t('clip.sync.fallbackLabel')} htmlFor="clip-remote-text">
          <textarea id="clip-remote-text" className="input input--mono" rows={3} readOnly lang="zh-TW" spellCheck={false} value={remote.fallbackText} onFocus={(e) => e.currentTarget.select()} />
        </Field>
      ) : null}
      {remote.enabled && remote.hasLatest ? (
        <div className="btn-row">
          <Button small icon="clipboard" onClick={() => void remote.copyLatest()}>
            {t('clip.sync.copyLatest')}
          </Button>
        </div>
      ) : null}
      <p className="panel__desc">{t('clip.privacy')}</p>
    </section>
  );
}
