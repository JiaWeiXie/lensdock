import RFB, { type RFBClipboardEventDetail } from '@novnc/novnc';
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
 *
 * Stream handoff (view <-> control, same workspace and runtime generation):
 *
 * - Every RFB renders into its own layer (a host element owned by this
 *   adapter). noVNC removes its canvas when a connection ends, so the layer of
 *   a stream that had painted is kept as a *frozen* layer: the very canvas
 *   element, last pixels included, re-attached inert and on top. No pixels are
 *   read or copied. The old RFB is disconnected immediately, so no input can
 *   travel over the superseded stream; the frozen layer has no handlers.
 * - The frozen layer is a presentation only. It is dropped as soon as the next
 *   stream has painted (probed on the DOM canvas with an 8x8 downscale, because
 *   `connect` fires before the first framebuffer update), when the target moves
 *   to another workspace or runtime generation, when a connection attempt
 *   fails, and after HANDOFF_MAX_MS at the latest. `RfbSnapshot.handoff`
 *   reports it, the phase stays truthful (never `connected` for the frozen
 *   image), and captures only ever read the live RFB, and only once it painted.
 * - A server fence (4001/4002) normally precedes the new target by one
 *   reconcile round trip. The frozen layer bridges that gap; when no target
 *   arrives within HANDOFF_MAX_MS the stream reports the `fenced` failure.
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
  /**
   * True while the last frame of the previous stream of this workspace is still shown during a handoff.
   * That image is not live: never present it as current, never capture it.
   */
  handoff: boolean;
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
/** Longest the previous frame stays on screen while the next stream is not painting yet. */
export const HANDOFF_MAX_MS = 4_000;
const PAINT_POLL_MS = 50;
const PAINT_SLOW_AFTER_MS = 5_000;
const PAINT_SLOW_POLL_MS = 500;
const PROBE_SIZE = 8;

export const DEFAULT_VIEW_OPTIONS: ViewOptions = { fit: true, quality: 6, compression: 2, dotCursor: false };

const FENCE_CODES: readonly number[] = [4001, 4002];

/** One RFB's presentation: the host element it renders into and the screen element noVNC created there. */
interface Layer {
  host: HTMLElement;
  screen: Element | null;
  /** True once the canvas holds painted pixels. */
  painted: boolean;
  /** Workspace + runtime generation; a frozen layer may only bridge to a stream of the same scope. */
  scope: string;
}

/** The desktop a stream shows: everything in its URL before `/vnc/<mode>` plus the runtime generation. */
function streamScope(target: StreamTarget): string {
  const m = /^(.*)\/vnc\/(?:view|control)(?:[?#].*)?$/.exec(target.url);
  return `${m?.[1] ?? target.url}|${target.generation}`;
}

export class RfbController {
  private readonly listeners = new Set<() => void>();
  private readonly clipboardListeners = new Set<(text: string) => void>();
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
    handoff: false,
  };

  private container: HTMLElement | null = null;
  private target: StreamTarget | null = null;
  private rfb: RFB | null = null;
  private ws: WebSocket | null = null;
  /** Layer of the current connection attempt/stream. */
  private live: Layer | null = null;
  /** Retained last frame of the previous stream, shown until `live` paints. */
  private frozen: Layer | null = null;
  private probe: HTMLCanvasElement | null = null;
  private token = 0;
  private retryTimer: number | undefined;
  private stableTimer: number | undefined;
  private measureTimer: number | undefined;
  private paintTimer: number | undefined;
  private handoffTimer: number | undefined;
  private lastActivity = 0;
  private wsCloseCode: number | null = null;
  /** Fence code received while the frozen layer waits for the next target. */
  private pendingFence: number | null = null;

  /** Called when a control stream is closed by the server fence (lease ended / generation changed). */
  onFenced: ((code: number) => void) | null = null;

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  getSnapshot = (): RfbSnapshot => this.snap;

  /**
   * Remote clipboard text announced by the current control stream (public RFB `clipboard` event).
   * Events of view streams and of superseded or disposed connections never reach listeners.
   */
  subscribeClipboard(listener: (text: string) => void): () => void {
    this.clipboardListeners.add(listener);
    return () => {
      this.clipboardListeners.delete(listener);
    };
  }

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
    this.stop(null);
    this.container = container;
    if (container && this.target) this.connect();
    else this.update(this.idlePatch(this.target));
  }

  setTarget(target: StreamTarget | null): void {
    if (target?.key === this.target?.key && target?.url === this.target?.url) return;
    // The previous frame may bridge to the new stream only when it shows the same desktop.
    this.stop(target ? streamScope(target) : null);
    this.target = target;
    this.update(this.idlePatch(target));
    if (target && this.container) this.connect();
  }

  retryNow(): void {
    if (!this.target || !this.container) return;
    this.stop(null);
    this.update({ attempt: 0, failure: null, nextRetryAt: null, closeCode: null });
    this.connect();
  }

  setOptions(patch: Partial<ViewOptions>): void {
    const options = { ...this.snap.options, ...patch };
    this.update({ options });
    this.applyOptions();
  }

  /**
   * Captures the live framebuffer as PNG through the public RFB.toBlob(). Rejects when not live
   * or before the live stream painted; the retained handoff image is never a capture source.
   */
  async captureFrame(): Promise<CapturedFrame> {
    const rfb = this.rfb;
    const token = this.token;
    const layer = this.live;
    if (!rfb || !layer?.painted || !this.isLive()) throw new Error('RFB_NOT_LIVE');
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
    this.stop(null);
    this.container = null;
    this.target = null;
    this.probe = null;
    this.listeners.clear();
    this.clipboardListeners.clear();
    this.onFenced = null;
  }

  // ---------------------------------------------------------------- internals

  private update(patch: Partial<RfbSnapshot>): void {
    this.snap = { ...this.snap, ...patch, handoff: this.frozen !== null };
    for (const l of this.listeners) l();
  }

  private idlePatch(target: StreamTarget | null): Partial<RfbSnapshot> {
    return {
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
    };
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

    if (this.live) this.removeLayer(this.live);
    this.live = null;
    const host = document.createElement('div');
    host.className = 'rfb-layer';
    container.appendChild(host);

    let rfb: RFB;
    try {
      rfb = new RFB(host, ws, { shared: true });
    } catch {
      host.remove();
      ws.close();
      this.ws = null;
      this.fail(token, 'exception');
      return;
    }
    const layer: Layer = { host, screen: host.firstElementChild, painted: false, scope: streamScope(target) };
    this.live = layer;
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
      this.startMeasuring(token, layer);
      this.watchFirstFrame(token, layer);
    });
    rfb.addEventListener('desktopname', (e) => {
      if (token !== this.token) return;
      this.update({ desktopName: (e as CustomEvent<{ name: string }>).detail.name });
    });
    if (target.mode === 'control') {
      rfb.addEventListener('clipboard', (e) => {
        if (token !== this.token || this.rfb !== rfb) return;
        const text = (e as CustomEvent<RFBClipboardEventDetail>).detail.text;
        for (const listener of [...this.clipboardListeners]) listener(text);
      });
    }
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
    const fenced = code !== null && FENCE_CODES.includes(code) && this.snap.mode === 'control';
    const scope = this.live?.scope ?? null;
    this.releaseConnection();
    if (fenced) {
      // The fence normally precedes the next target by one reconcile round trip: bridge it with the last frame.
      this.retireLive(scope);
      if (this.frozen) {
        this.pendingFence = code;
        this.armHandoffTimer(false);
        this.update({ ...this.idlePatch(this.target), mode: 'control' });
      } else {
        this.update({ phase: 'failed', failure: 'fenced', closeCode: code, connectedAt: null, width: 0, height: 0 });
      }
      this.onFenced?.(code);
      return;
    }
    this.retireLive(null);
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
    this.retireLive(null);
    this.update({ phase: 'failed', failure, connectedAt: null, width: 0, height: 0, nextRetryAt: null });
  }

  private startMeasuring(token: number, layer: Layer): void {
    const measure = (): void => {
      if (token !== this.token || this.live !== layer) return;
      const canvas = layer.host.querySelector('canvas');
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

  /**
   * Waits for the first painted pixels of `layer`. noVNC fires `connect` after the handshake but before the
   * first framebuffer update, so connect is not a frame. A painted noVNC canvas is opaque; an unpainted one
   * is fully transparent, which an 8x8 downscale shows without reading the framebuffer.
   */
  private watchFirstFrame(token: number, layer: Layer): void {
    window.clearInterval(this.paintTimer);
    const startedAt = Date.now();
    let slow = false;
    const tick = (): void => {
      if (token !== this.token || this.live !== layer) {
        window.clearInterval(this.paintTimer);
        return;
      }
      if (this.probePainted(layer)) {
        window.clearInterval(this.paintTimer);
        this.paintTimer = undefined;
        layer.painted = true;
        this.dropFrozen();
        this.update({});
        return;
      }
      if (!slow && Date.now() - startedAt > PAINT_SLOW_AFTER_MS) {
        slow = true;
        window.clearInterval(this.paintTimer);
        this.paintTimer = window.setInterval(tick, PAINT_SLOW_POLL_MS);
      }
    };
    this.paintTimer = window.setInterval(tick, PAINT_POLL_MS);
  }

  private probePainted(layer: Layer): boolean {
    const canvas = layer.host.querySelector('canvas');
    if (!(canvas instanceof HTMLCanvasElement) || canvas.width === 0 || canvas.height === 0) return false;
    if (!this.probe) {
      this.probe = document.createElement('canvas');
      this.probe.width = PROBE_SIZE;
      this.probe.height = PROBE_SIZE;
    }
    const ctx = this.probe.getContext('2d', { willReadFrequently: true });
    if (!ctx) return false;
    ctx.clearRect(0, 0, PROBE_SIZE, PROBE_SIZE);
    ctx.drawImage(canvas, 0, 0, PROBE_SIZE, PROBE_SIZE);
    const data = ctx.getImageData(0, 0, PROBE_SIZE, PROBE_SIZE).data;
    for (let i = 3; i < data.length; i += 4) if ((data[i] ?? 0) > 0) return true;
    return false;
  }

  /**
   * Ends the current layer. A painted layer whose scope matches `nextScope` becomes the frozen
   * presentation (replacing an older one); every other layer is removed, and a frozen layer of a
   * different scope is dropped.
   */
  private retireLive(nextScope: string | null): void {
    const layer = this.live;
    this.live = null;
    if (layer && layer.painted && layer.screen && nextScope !== null && layer.scope === nextScope) {
      this.dropFrozen();
      // noVNC removed its screen element on disconnect; its canvas still holds the last pixels.
      if (layer.screen.parentNode !== layer.host) layer.host.appendChild(layer.screen);
      layer.host.className = 'rfb-layer rfb-layer--frozen';
      layer.host.setAttribute('inert', '');
      layer.host.setAttribute('aria-hidden', 'true');
      this.frozen = layer;
      this.armHandoffTimer(true);
      return;
    }
    if (layer) this.removeLayer(layer);
    if (!(nextScope !== null && this.frozen && this.frozen.scope === nextScope)) this.dropFrozen();
  }

  private armHandoffTimer(restart: boolean): void {
    if (this.handoffTimer !== undefined) {
      if (!restart) return;
      window.clearTimeout(this.handoffTimer);
    }
    this.handoffTimer = window.setTimeout(() => {
      this.handoffTimer = undefined;
      this.dropFrozen();
      const code = this.pendingFence;
      this.pendingFence = null;
      if (code !== null) this.update({ phase: 'failed', failure: 'fenced', closeCode: code });
      else this.update({});
    }, HANDOFF_MAX_MS);
  }

  private dropFrozen(): void {
    window.clearTimeout(this.handoffTimer);
    this.handoffTimer = undefined;
    if (this.frozen) this.removeLayer(this.frozen);
    this.frozen = null;
  }

  private removeLayer(layer: Layer): void {
    layer.host.remove();
  }

  private clearTimers(): void {
    window.clearTimeout(this.retryTimer);
    window.clearTimeout(this.stableTimer);
    window.clearInterval(this.measureTimer);
    window.clearInterval(this.paintTimer);
    this.retryTimer = undefined;
    this.stableTimer = undefined;
    this.measureTimer = undefined;
    this.paintTimer = undefined;
  }

  /** Drops the current RFB/socket without notifying listeners about a phase change. */
  private releaseConnection(): void {
    window.clearTimeout(this.stableTimer);
    window.clearInterval(this.measureTimer);
    window.clearInterval(this.paintTimer);
    this.rfb = null;
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) this.ws.close();
    this.ws = null;
  }

  /**
   * Immediately fences the current stream: invalidates its token, disconnects the RFB (which ungrabs
   * the keyboard and closes the socket) and retires its layer. Callers publish the next snapshot.
   */
  private stop(nextScope: string | null): void {
    this.token += 1;
    this.clearTimers();
    this.pendingFence = null;
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
    this.retireLive(nextScope);
  }
}
