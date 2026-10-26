/**
 * Remote-to-local clipboard sync.
 *
 * Text copied on the remote desktop reaches the browser as an RFB ServerCutText
 * event (see RfbController.subscribeClipboard). This class decides what happens
 * to it: write it to the host clipboard when sync is on, the page has focus and
 * a control session is bound, otherwise remember it in memory only so the person
 * can copy it by hand.
 *
 * Rules that keep it honest:
 * - Nothing is persisted or logged; text lives in this object and is dropped when
 *   the session changes or sync is turned off. Turning sync back on never replays.
 * - The host clipboard is never read.
 * - Writes are serial and only the newest pending text is written, so a burst of
 *   copies cannot land out of order.
 * - Every bind/disable/dispose bumps a generation; results of writes started
 *   earlier cannot change state afterwards.
 */

export type RemoteClipboardStatus =
  /** Nothing received yet (or reset). */
  | 'idle'
  /** The latest remote text is on the host clipboard. */
  | 'copied'
  /** The page was not focused, so the browser would refuse the write. */
  | 'unfocused'
  /** The browser refused the write (permission or user-activation policy). */
  | 'denied'
  /** The browser exposes no clipboard write API here (for example a non-secure context). */
  | 'unavailable';

export type RemoteClipboardFailure = Exclude<RemoteClipboardStatus, 'idle' | 'copied'>;

export interface RemoteClipboardSnapshot {
  enabled: boolean;
  status: RemoteClipboardStatus;
  /** Latest remote text is held and can be copied explicitly. */
  hasLatest: boolean;
  /** The latest remote text, only while it could not be written automatically. */
  fallbackText: string | null;
}

export interface RemoteClipboardEnv {
  /** `navigator.clipboard.writeText`, or undefined when the browser has none. */
  writeText: ((text: string) => Promise<void>) | undefined;
  hasFocus: () => boolean;
}

/**
 * x11vnc asks the X selection owner for the Latin-1 `STRING` target and forwards
 * the bytes it gets; Chromium answers that target with its UTF-8 bytes. noVNC
 * decodes ServerCutText as Latin-1, so CJK text arrives as one char per UTF-8
 * byte. When every char is a byte value and the bytes form valid UTF-8, the
 * original text is recovered exactly. Anything else (pure ASCII, real Latin-1,
 * text from a server with the extended clipboard) is returned untouched.
 */
export function recoverUtf8(text: string): string {
  let ascii = true;
  for (let i = 0; i < text.length; i += 1) {
    const c = text.charCodeAt(i);
    if (c > 0xff) return text;
    if (c > 0x7f) ascii = false;
  }
  if (ascii) return text;
  const bytes = new Uint8Array(text.length);
  for (let i = 0; i < text.length; i += 1) bytes[i] = text.charCodeAt(i);
  try {
    return new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(bytes);
  } catch {
    return text;
  }
}

export class RemoteClipboardSync {
  private enabled: boolean;
  private session: string | null = null;
  private generation = 0;
  private latest: string | null = null;
  private queued: string | null = null;
  private writing = false;
  private status: RemoteClipboardStatus = 'idle';
  private fallbackText: string | null = null;
  private notified: RemoteClipboardFailure | null = null;
  private snapshot: RemoteClipboardSnapshot;
  private readonly listeners = new Set<() => void>();

  /** Called once when an episode of automatic writes starts failing (not for every event). */
  onFailure: ((reason: RemoteClipboardFailure) => void) | null = null;

  constructor(
    private readonly env: RemoteClipboardEnv,
    enabled: boolean,
  ) {
    this.enabled = enabled;
    this.snapshot = this.build();
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  readonly getSnapshot = (): RemoteClipboardSnapshot => this.snapshot;

  setEnabled(enabled: boolean): void {
    if (this.enabled === enabled) return;
    this.enabled = enabled;
    this.reset();
  }

  /** Binds sync to one control stream (null: no control stream, view-only or lost). A different key drops held text. */
  bind(session: string | null): void {
    if (this.session === session) return;
    this.session = session;
    this.reset();
  }

  /** Entry point for the controller's clipboard event. */
  receive(raw: string): void {
    if (!this.enabled || this.session === null) return;
    const text = recoverUtf8(raw);
    if (text.length === 0) return;
    this.latest = text;
    this.queued = text;
    this.publish();
    void this.pump();
  }

  /** Explicit copy of the latest remote text; call from a user gesture. Resolves to whether the host clipboard now holds it. */
  async copyLatest(): Promise<boolean> {
    const text = this.latest;
    if (text === null) return false;
    const generation = this.generation;
    const status = await this.write(text);
    if (generation !== this.generation) return false;
    this.settle(status, text);
    return status === 'copied';
  }

  private reset(): void {
    this.generation += 1;
    this.latest = null;
    this.queued = null;
    this.status = 'idle';
    this.fallbackText = null;
    this.notified = null;
    this.publish();
  }

  private async pump(): Promise<void> {
    if (this.writing) return;
    this.writing = true;
    try {
      while (this.queued !== null) {
        const text = this.queued;
        this.queued = null;
        const generation = this.generation;
        const status = await this.write(text);
        // Reset meanwhile, or a newer text is waiting: this result is already out of date.
        if (generation !== this.generation || this.queued !== null) continue;
        this.settle(status, text);
        if (status === 'copied') {
          this.notified = null;
        } else if (this.notified !== status) {
          this.notified = status;
          this.onFailure?.(status);
        }
      }
    } finally {
      this.writing = false;
    }
  }

  private async write(text: string): Promise<Exclude<RemoteClipboardStatus, 'idle'>> {
    if (!this.env.writeText) return 'unavailable';
    if (!this.env.hasFocus()) return 'unfocused';
    try {
      await this.env.writeText(text);
      return 'copied';
    } catch {
      return 'denied';
    }
  }

  private settle(status: RemoteClipboardStatus, text: string): void {
    this.status = status;
    this.fallbackText = status === 'copied' ? null : text;
    if (status === 'copied') this.notified = null;
    this.publish();
  }

  private build(): RemoteClipboardSnapshot {
    return { enabled: this.enabled, status: this.status, hasLatest: this.latest !== null, fallbackText: this.fallbackText };
  }

  private publish(): void {
    this.snapshot = this.build();
    for (const listener of this.listeners) listener();
  }
}
