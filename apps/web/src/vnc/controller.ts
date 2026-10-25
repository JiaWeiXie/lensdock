import RFB from '@novnc/novnc';
import { backoffDelay } from '@/util/async';
import { MODIFIER_KEYSYMS } from './keys';

/**
 * Client adapter around the public noVNC RFB API.
 *
 * - Uses only documented members of RFB (see novnc.d.ts); never `_display` & co.
 * - The WebSocket is created here and handed to RFB, which lets the adapter
 *   observe close codes (4001/4002 fence a control stream) and data activity
 *   without touching RFB internals.
 * - Connections are bounded: at most MAX_ATTEMPTS reconnects with exponential
 *   backoff, then `failed` until the user retries.
 * - Every connection carries a token. Events from a superseded RFB/WebSocket
 *   are ignored, so a stale socket can never update state or be captured.
 * - A disconnected RFB removes its canvas, so no stale frame can be captured.
 */

export type RfbPhase = 'idle' | 'connecting' | 'connected' | 'retrying' | 'failed';
export type StreamMode = 'view' | 'control';

export interface StreamTarget {
  /** Identity of the stream. A different key tears down and reconnects. */
  key: string;
  url: string;
  mode: StreamMode;
  /** Runtime generation this stream was opened for. */
  generation: string;
}

export type FailureKind = 'closed' | 'fenced' | 'security' | 'credentials' | 'exception';

export interface ViewOptions {
  /** Scale the remote desktop to fit the stage (true) or show it 1:1 with scrollbars (false). */
  fit: boolean;
  /** JPEG quality hint 0-9. */
  quality: number;
  /** Compression hint 0-9. */
  compression: number;
  dotCursor: boolean;
}

export interface RfbSnapshot {
  phase: RfbPhase;
  mode: StreamMode | null;
  attempt: number;
  maxAttempts: number;
  nextRetryAt: number | null;
  failure: FailureKind | null;
  closeCode: number | null;
  desktopName: string | null;
  /** Actual remote framebuffer size in remote pixels (0 until connected). */
  width: number;
  height: number;
  /** Displayed CSS width / remote width (1 = native). */
  scale: number;
  connectedAt: number | null;
  options: ViewOptions;
}

export interface CapturedFrame {
  blob: Blob;
  width: number;
  height: number;
  /** RFC 3339 client capture time. */
  capturedAt: string;
}

const MAX_ATTEMPTS = 5;
const STABLE_AFTER_MS = 10_000;

export const DEFAULT_VIEW_OPTIONS: ViewOptions = { fit: true, quality: 6, compression: 2, dotCursor: false };

const FENCE_CODES: readonly number[] = [4001, 4002];

export class RfbController {
  private readonly listeners = new Set<() => void>();
  private snap: RfbSnapshot = {
    phase: 'idle',
    mode: null,
    attempt: 0,
    maxAttempts: MAX_ATTEMPTS,
    nextRetryAt: null,
    failure: null,
    closeCode: null,
    desktopName: null,
    width: 0,
    height: 0,
    scale: 1,
    connectedAt: null,
    options: DEFAULT_VIEW_OPTIONS,
  };

  private container: HTMLElement | null = null;
  private target: StreamTarget | null = null;
  private rfb: RFB | null = null;
  private ws: WebSocket | null = null;
  private token = 0;
  private retryTimer: number | undefined;
  private stableTimer: number | undefined;
  private measureTimer: number | undefined;
  private lastActivity = 0;
  private wsCloseCode: number | null = null;

  /** Called when a control stream is closed by the server fence (lease ended / generation changed). */
  onFenced: ((code: number) => void) | null = null;

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  getSnapshot = (): RfbSnapshot => this.snap;

  /** Wall-clock ms of the last data frame received on the current socket (0 = none). */
  getLastActivityAt(): number {
    return this.lastActivity;
  }

  /** Runtime generation of the stream being shown (null when none). */
  getGeneration(): string | null {
    return this.target?.generation ?? null;
  }

  /** True only while an RFB session is established on an open socket. */
  isLive(): boolean {
    return this.snap.phase === 'connected' && this.rfb !== null && this.ws?.readyState === WebSocket.OPEN;
  }

  /** True when keystrokes/pointer reach the remote desktop. */
  isInputLive(): boolean {
    return this.isLive() && this.snap.mode === 'control';
  }

  attach(container: HTMLElement | null): void {
    if (container === this.container) return;
    this.teardown();
    this.container = container;
    if (container && this.target) this.connect();
  }

  setTarget(target: StreamTarget | null): void {
    if (target?.key === this.target?.key && target?.url === this.target?.url) return;
    this.teardown();
    this.target = target;
    this.update({
      phase: 'idle',
      mode: target?.mode ?? null,
      attempt: 0,
      nextRetryAt: null,
      failure: null,
      closeCode: null,
      desktopName: null,
      width: 0,
      height: 0,
      connectedAt: null,
    });
    if (target && this.container) this.connect();
  }

  retryNow(): void {
    if (!this.target || !this.container) return;
    this.teardown();
    this.update({ attempt: 0, failure: null, nextRetryAt: null, closeCode: null });
    this.connect();
  }

  setOptions(patch: Partial<ViewOptions>): void {
    const options = { ...this.snap.options, ...patch };
    this.update({ options });
    this.applyOptions();
  }

  /** Captures the live framebuffer as PNG through the public RFB.toBlob(). Rejects when not live. */
  async captureFrame(): Promise<CapturedFrame> {
    const rfb = this.rfb;
    const token = this.token;
    if (!rfb || !this.isLive()) throw new Error('RFB_NOT_LIVE');
    const capturedAtMs = Date.now();
    const pending = Promise.withResolvers<Blob | null>();
    rfb.toBlob(pending.resolve, 'image/png');
    const blob = await pending.promise;
    if (!blob || token !== this.token || !this.isLive()) throw new Error('RFB_NOT_LIVE');
    const bitmap = await createImageBitmap(blob);
    const frame: CapturedFrame = {
      blob,
      width: bitmap.width,
      height: bitmap.height,
      capturedAt: new Date(capturedAtMs).toISOString(),
    };
    bitmap.close();
    return frame;
  }

  /** Sends a key chord through RFB.sendKey: presses in order, releases in reverse. */
  sendCombo(keys: readonly number[]): void {
    if (!this.rfb || !this.isInputLive()) return;
    for (const k of keys) this.rfb.sendKey(k, null, true);
    for (const k of [...keys].reverse()) this.rfb.sendKey(k, null, false);
  }

  sendCtrlAltDel(): void {
    if (this.rfb && this.isInputLive()) this.rfb.sendCtrlAltDel();
  }

  /** Releases every modifier so none stays latched on the remote desktop. */
  releaseAllKeys(): void {
    if (!this.rfb || !this.isInputLive()) return;
    for (const k of MODIFIER_KEYSYMS) this.rfb.sendKey(k, null, false);
  }

  focus(): void {
    this.rfb?.focus({ preventScroll: true });
  }

  blur(): void {
    this.rfb?.blur();
  }

  dispose(): void {
    this.teardown();
    this.container = null;
    this.target = null;
    this.listeners.clear();
    this.onFenced = null;
  }

  // ---------------------------------------------------------------- internals

  private update(patch: Partial<RfbSnapshot>): void {
    this.snap = { ...this.snap, ...patch };
    for (const l of this.listeners) l();
  }

  private applyOptions(): void {
    const rfb = this.rfb;
    if (!rfb) return;
    const o = this.snap.options;
    rfb.scaleViewport = o.fit;
    // Native (1:1) size must stay pannable: a clipped viewport has no way to move (no drag pan is wired),
    // while an unclipped one shows scrollbars, and the canvas keeps the real framebuffer size for the readout.
    rfb.clipViewport = false;
    rfb.qualityLevel = o.quality;
    rfb.compressionLevel = o.compression;
    rfb.showDotCursor = o.dotCursor;
  }

  private connect(): void {
    const container = this.container;
    const target = this.target;
    if (!container || !target) return;
    this.clearTimers();
    const token = ++this.token;
    this.wsCloseCode = null;
    this.lastActivity = 0;
    this.update({ phase: 'connecting', mode: target.mode, nextRetryAt: null, failure: null, closeCode: null });

    let ws: WebSocket;
    try {
      ws = new WebSocket(target.url);
    } catch {
      this.fail(token, 'exception');
      return;
    }
    this.ws = ws;
    ws.addEventListener('message', () => {
      if (token === this.token) this.lastActivity = Date.now();
    });
    ws.addEventListener('close', (e) => {
      if (token === this.token) this.wsCloseCode = e.code;
    });

    let rfb: RFB;
    try {
      rfb = new RFB(container, ws, { shared: true });
    } catch {
      ws.close();
      this.ws = null;
      this.fail(token, 'exception');
      return;
    }
    this.rfb = rfb;
    rfb.viewOnly = target.mode === 'view';
    rfb.focusOnClick = target.mode === 'control';
    rfb.background = '#0b1c2e';
    this.applyOptions();

    rfb.addEventListener('connect', () => {
      if (token !== this.token) return;
      this.update({ phase: 'connected', connectedAt: Date.now(), failure: null });
      this.stableTimer = window.setTimeout(() => {
        if (token === this.token) this.update({ attempt: 0 });
      }, STABLE_AFTER_MS);
      this.startMeasuring(token);
    });
    rfb.addEventListener('desktopname', (e) => {
      if (token !== this.token) return;
      this.update({ desktopName: (e as CustomEvent<{ name: string }>).detail.name });
    });
    rfb.addEventListener('credentialsrequired', () => {
      if (token !== this.token) return;
      this.fail(token, 'credentials');
    });
    rfb.addEventListener('securityfailure', () => {
      if (token !== this.token) return;
      this.fail(token, 'security');
    });
    rfb.addEventListener('disconnect', () => {
      this.handleDisconnect(token);
    });
  }

  private handleDisconnect(token: number): void {
    if (token !== this.token) return;
    const code = this.wsCloseCode;
    this.releaseConnection();
    if (code !== null && FENCE_CODES.includes(code) && this.snap.mode === 'control') {
      this.update({ phase: 'failed', failure: 'fenced', closeCode: code, connectedAt: null, width: 0, height: 0 });
      this.onFenced?.(code);
      return;
    }
    if (this.snap.attempt >= MAX_ATTEMPTS) {
      this.update({ phase: 'failed', failure: 'closed', closeCode: code, connectedAt: null, width: 0, height: 0 });
      return;
    }
    const delay = backoffDelay(this.snap.attempt);
    const attempt = this.snap.attempt + 1;
    this.update({
      phase: 'retrying',
      attempt,
      failure: 'closed',
      closeCode: code,
      nextRetryAt: Date.now() + delay,
      connectedAt: null,
      width: 0,
      height: 0,
    });
    this.retryTimer = window.setTimeout(() => {
      if (token === this.token) this.connect();
    }, delay);
  }

  private fail(token: number, failure: FailureKind): void {
    if (token !== this.token) return;
    this.releaseConnection();
    this.update({ phase: 'failed', failure, connectedAt: null, width: 0, height: 0, nextRetryAt: null });
  }

  private startMeasuring(token: number): void {
    const measure = (): void => {
      if (token !== this.token || !this.container) return;
      const canvas = this.container.querySelector('canvas');
      if (!(canvas instanceof HTMLCanvasElement)) return;
      const width = canvas.width;
      const height = canvas.height;
      const scale = width > 0 ? canvas.clientWidth / width : 1;
      if (width !== this.snap.width || height !== this.snap.height || Math.abs(scale - this.snap.scale) > 0.001) {
        this.update({ width, height, scale });
      }
    };
    measure();
    this.measureTimer = window.setInterval(measure, 500);
  }

  private clearTimers(): void {
    window.clearTimeout(this.retryTimer);
    window.clearTimeout(this.stableTimer);
    window.clearInterval(this.measureTimer);
    this.retryTimer = undefined;
    this.stableTimer = undefined;
    this.measureTimer = undefined;
  }

  /** Drops the current RFB/socket without notifying listeners about a phase change. */
  private releaseConnection(): void {
    window.clearTimeout(this.stableTimer);
    window.clearInterval(this.measureTimer);
    this.rfb = null;
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) this.ws.close();
    this.ws = null;
  }

  private teardown(): void {
    this.token += 1;
    this.clearTimers();
    const rfb = this.rfb;
    this.rfb = null;
    if (rfb) {
      try {
        rfb.disconnect();
      } catch {
        // already disconnected
      }
    }
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) this.ws.close();
    this.ws = null;
    if (this.snap.phase !== 'idle') {
      this.update({ phase: 'idle', nextRetryAt: null, connectedAt: null, width: 0, height: 0 });
    }
  }
}
