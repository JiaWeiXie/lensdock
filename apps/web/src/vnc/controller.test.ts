import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

/** Minimal DOM + noVNC + WebSocket fakes: just enough surface for the controller's layer handling. */
const fakes = vi.hoisted(() => {
  class FakeElement {
    children: FakeElement[] = [];
    parentNode: FakeElement | null = null;
    className = '';
    attributes = new Map<string, string>();
    constructor(readonly tag: string) {}
    get firstElementChild(): FakeElement | null {
      return this.children[0] ?? null;
    }
    appendChild(child: FakeElement): FakeElement {
      child.parentNode?.removeChild(child);
      child.parentNode = this;
      this.children.push(child);
      return child;
    }
    removeChild(child: FakeElement): FakeElement {
      this.children = this.children.filter((c) => c !== child);
      child.parentNode = null;
      return child;
    }
    remove(): void {
      this.parentNode?.removeChild(this);
    }
    setAttribute(name: string, value: string): void {
      this.attributes.set(name, value);
    }
    querySelector(tag: string): FakeElement | null {
      for (const c of this.children) {
        if (c.tag === tag) return c;
        const nested = c.querySelector(tag);
        if (nested) return nested;
      }
      return null;
    }
  }

  class FakeCanvas extends FakeElement {
    width = 800;
    height = 600;
    clientWidth = 800;
    painted = false;
    constructor() {
      super('canvas');
    }
    getContext(): { clearRect(): void; drawImage(src: FakeCanvas): void; getImageData(): { data: Uint8ClampedArray } } {
      let alpha = 0;
      return {
        clearRect: () => {
          alpha = 0;
        },
        drawImage: (src) => {
          alpha = src.painted ? 255 : 0;
        },
        getImageData: () => ({ data: new Uint8ClampedArray([0, 0, 0, alpha]) }),
      };
    }
  }

  class FakeWebSocket extends EventTarget {
    static readonly CONNECTING = 0;
    static readonly OPEN = 1;
    static readonly CLOSING = 2;
    static readonly CLOSED = 3;
    static instances: FakeWebSocket[] = [];
    readyState = 0;
    constructor(readonly url: string) {
      super();
      FakeWebSocket.instances.push(this);
    }
    close(): void {
      this.readyState = 3;
    }
    /** The server closes the socket with `code`. */
    serverClose(code: number): void {
      this.readyState = 3;
      this.dispatchEvent(Object.assign(new Event('close'), { code }));
    }
  }

  class FakeRfb extends EventTarget {
    static instances: FakeRfb[] = [];
    viewOnly = false;
    focusOnClick = false;
    clipViewport = false;
    dragViewport = false;
    scaleViewport = false;
    resizeSession = false;
    showDotCursor = false;
    background = '';
    qualityLevel = 0;
    compressionLevel = 0;
    capabilities = { power: false };
    disconnected = false;
    keysSent = 0;
    readonly screen = new FakeElement('div');
    readonly canvas = new FakeCanvas();
    constructor(
      readonly host: FakeElement,
      readonly socket: FakeWebSocket,
    ) {
      super();
      this.screen.appendChild(this.canvas);
      host.appendChild(this.screen);
      FakeRfb.instances.push(this);
    }
    disconnect(): void {
      if (this.disconnected) return;
      this.disconnected = true;
      this.host.removeChild(this.screen);
      this.socket.close();
    }
    sendKey(): void {
      this.keysSent += 1;
    }
    sendCtrlAltDel(): void {
      this.keysSent += 1;
    }
    focus(): void {}
    blur(): void {}
    toBlob(callback: (b: Blob | null) => void): void {
      callback(new Blob(['png']));
    }
    /** Handshake finished (before any framebuffer update). */
    connect(): void {
      this.socket.readyState = 1;
      this.dispatchEvent(new CustomEvent('connect', { detail: {} }));
    }
    /** First framebuffer update drawn. */
    paint(): void {
      this.canvas.painted = true;
    }
    clipboard(text: string): void {
      this.dispatchEvent(new CustomEvent('clipboard', { detail: { text } }));
    }
    /** Server closed the stream: noVNC removes its screen, then reports `disconnect`. */
    serverClose(code: number): void {
      this.disconnected = true;
      this.host.removeChild(this.screen);
      this.socket.serverClose(code);
      this.dispatchEvent(new CustomEvent('disconnect', { detail: { clean: false } }));
    }
  }

  return { FakeElement, FakeCanvas, FakeWebSocket, FakeRfb };
});

vi.mock('@novnc/novnc', () => ({ default: fakes.FakeRfb }));

import { HANDOFF_MAX_MS, RfbController, type StreamTarget } from './controller';

const { FakeElement, FakeCanvas, FakeWebSocket, FakeRfb } = fakes;

const view = (generation = 'g1', workspace = 'w1'): StreamTarget => ({
  key: `view:${workspace}:${generation}`,
  url: `ws://gw/api/workspaces/${workspace}/vnc/view`,
  mode: 'view',
  generation,
});
const control = (generation = 'g1', workspace = 'w1', lease = 'l1'): StreamTarget => ({
  key: `control:${lease}:${generation}`,
  url: `ws://gw/api/workspaces/${workspace}/vnc/control?epoch=1&runtimeGeneration=${generation}`,
  mode: 'control',
  generation,
});

function last(): InstanceType<typeof FakeRfb> {
  const rfb = FakeRfb.instances.at(-1);
  if (!rfb) throw new Error('no RFB created');
  return rfb;
}

/** Layers in the container as `live` / `frozen` class names. */
function layers(container: InstanceType<typeof FakeElement>): string[] {
  return container.children.map((c) => c.className);
}

async function tickPaint(): Promise<void> {
  await vi.advanceTimersByTimeAsync(60);
}

/** A controller attached to a fake container whose current stream is connected and painted. */
async function streaming(target: StreamTarget): Promise<{ controller: RfbController; container: InstanceType<typeof FakeElement> }> {
  const container = new FakeElement('div');
  const controller = new RfbController();
  controller.attach(container as unknown as HTMLElement);
  controller.setTarget(target);
  last().connect();
  last().paint();
  await tickPaint();
  return { controller, container };
}

beforeEach(() => {
  vi.useFakeTimers();
  FakeRfb.instances = [];
  FakeWebSocket.instances = [];
  vi.stubGlobal('window', globalThis);
  vi.stubGlobal('WebSocket', FakeWebSocket);
  vi.stubGlobal('HTMLCanvasElement', FakeCanvas);
  vi.stubGlobal('document', { createElement: (tag: string) => (tag === 'canvas' ? new FakeCanvas() : new FakeElement(tag)) });
  vi.stubGlobal('createImageBitmap', async () => ({ width: 800, height: 600, close: () => {} }));
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('RfbController stream handoff', () => {
  it('keeps the last frame, not live, until the next stream has painted', async () => {
    const { controller, container } = await streaming(view());
    const old = last();
    expect(controller.getSnapshot().handoff).toBe(false);

    controller.setTarget(control());

    // Old stream is fenced at once: disconnected, no input path, no live phase.
    expect(old.disconnected).toBe(true);
    expect(controller.getSnapshot().phase).toBe('connecting');
    expect(controller.isInputLive()).toBe(false);
    controller.sendCombo([1, 2]);
    expect(old.keysSent).toBe(0);
    // Its canvas stays on screen (frozen, above the new layer) instead of a blank stage.
    expect(controller.getSnapshot().handoff).toBe(true);
    expect(layers(container)).toEqual(['rfb-layer rfb-layer--frozen', 'rfb-layer']);
    expect(container.children[0]?.querySelector('canvas')).toBe(old.canvas);
    expect(container.children[0]?.attributes.get('inert')).toBe('');

    // Connect is a handshake, not a frame: the frozen image must stay and nothing may be captured.
    const next = last();
    next.connect();
    expect(controller.getSnapshot().phase).toBe('connected');
    expect(controller.getSnapshot().handoff).toBe(true);
    await vi.advanceTimersByTimeAsync(200);
    expect(controller.getSnapshot().handoff).toBe(true);
    await expect(controller.captureFrame()).rejects.toThrow('RFB_NOT_LIVE');

    next.paint();
    await tickPaint();
    expect(controller.getSnapshot().handoff).toBe(false);
    expect(layers(container)).toEqual(['rfb-layer']);
    await expect(controller.captureFrame()).resolves.toMatchObject({ width: 800, height: 600 });
    controller.dispose();
  });

  it('bridges a server fence that arrives before the new target', async () => {
    const { controller, container } = await streaming(control());
    const fenced = vi.fn();
    controller.onFenced = fenced;
    const old = last();

    old.serverClose(4001);

    expect(fenced).toHaveBeenCalledWith(4001);
    expect(controller.getSnapshot()).toMatchObject({ phase: 'idle', failure: null, handoff: true });
    expect(controller.isInputLive()).toBe(false);
    expect(layers(container)).toEqual(['rfb-layer rfb-layer--frozen']);
    await expect(controller.captureFrame()).rejects.toThrow('RFB_NOT_LIVE');

    controller.setTarget(view());
    expect(layers(container)).toEqual(['rfb-layer rfb-layer--frozen', 'rfb-layer']);
    last().connect();
    last().paint();
    await tickPaint();
    expect(controller.getSnapshot()).toMatchObject({ phase: 'connected', handoff: false });
    expect(layers(container)).toEqual(['rfb-layer']);
    expect(old.keysSent).toBe(0);
    controller.dispose();
  });

  it('reports the fenced failure when no new target follows the fence', async () => {
    const { controller, container } = await streaming(control());
    last().serverClose(4002);
    expect(controller.getSnapshot().handoff).toBe(true);

    await vi.advanceTimersByTimeAsync(HANDOFF_MAX_MS + 1);

    expect(controller.getSnapshot()).toMatchObject({ phase: 'failed', failure: 'fenced', closeCode: 4002, handoff: false });
    expect(container.children).toHaveLength(0);
    controller.dispose();
  });

  it('shows no retained frame for a fence before the stream ever painted', () => {
    const container = new FakeElement('div');
    const controller = new RfbController();
    controller.attach(container as unknown as HTMLElement);
    controller.setTarget(control());
    last().connect();
    last().serverClose(4001);
    expect(controller.getSnapshot()).toMatchObject({ phase: 'failed', failure: 'fenced', handoff: false });
    expect(container.children).toHaveLength(0);
    controller.dispose();
  });

  it('drops the retained frame when the workspace or runtime generation changes', async () => {
    const generationChange = await streaming(view('g1'));
    generationChange.controller.setTarget(view('g2'));
    expect(generationChange.controller.getSnapshot().handoff).toBe(false);
    expect(layers(generationChange.container)).toEqual(['rfb-layer']);
    generationChange.controller.dispose();

    const workspaceChange = await streaming(view('g1', 'w1'));
    workspaceChange.controller.setTarget(control('g1', 'w2'));
    expect(workspaceChange.controller.getSnapshot().handoff).toBe(false);
    expect(layers(workspaceChange.container)).toEqual(['rfb-layer']);
    workspaceChange.controller.dispose();

    const cleared = await streaming(view());
    cleared.controller.setTarget(null);
    expect(cleared.controller.getSnapshot()).toMatchObject({ phase: 'idle', handoff: false });
    expect(cleared.container.children).toHaveLength(0);
    cleared.controller.dispose();
  });

  it('keeps the older painted frame across fast successive handoffs', async () => {
    const { controller, container } = await streaming(view());
    const first = last();

    controller.setTarget(control());
    const second = last();
    second.connect(); // never paints before the next handoff
    controller.setTarget(view());
    const third = last();

    expect(second.disconnected).toBe(true);
    expect(layers(container)).toEqual(['rfb-layer rfb-layer--frozen', 'rfb-layer']);
    expect(container.children[0]?.querySelector('canvas')).toBe(first.canvas);

    third.connect();
    third.paint();
    await tickPaint();
    expect(layers(container)).toEqual(['rfb-layer']);
    expect(controller.getSnapshot().handoff).toBe(false);
    controller.dispose();
  });

  it('replaces the frozen frame with the newer painted one', async () => {
    const { controller, container } = await streaming(view());
    controller.setTarget(control());
    last().connect();
    last().paint();
    await tickPaint();
    const controlRfb = last();

    controller.setTarget(view());
    expect(layers(container)).toEqual(['rfb-layer rfb-layer--frozen', 'rfb-layer']);
    expect(container.children[0]?.querySelector('canvas')).toBe(controlRfb.canvas);
    controller.dispose();
  });

  it('stops showing the retained frame when the next stream is slow, and when it fails', async () => {
    const slow = await streaming(view());
    slow.controller.setTarget(control());
    await vi.advanceTimersByTimeAsync(HANDOFF_MAX_MS + 1);
    expect(slow.controller.getSnapshot()).toMatchObject({ phase: 'connecting', handoff: false });
    expect(layers(slow.container)).toEqual(['rfb-layer']);
    slow.controller.dispose();

    const failing = await streaming(view());
    failing.controller.setTarget(control());
    last().serverClose(1006);
    expect(failing.controller.getSnapshot()).toMatchObject({ phase: 'retrying', failure: 'closed', handoff: false });
    expect(failing.container.children).toHaveLength(0);
    failing.controller.dispose();

    const denied = await streaming(view());
    denied.controller.setTarget(control());
    last().dispatchEvent(new CustomEvent('securityfailure', { detail: {} }));
    expect(denied.controller.getSnapshot()).toMatchObject({ phase: 'failed', failure: 'security', handoff: false });
    expect(denied.container.children).toHaveLength(0);
    denied.controller.dispose();
  });

  it('applies option changes to the live stream only and ignores a stale stream', async () => {
    const { controller } = await streaming(view());
    const old = last();
    controller.setTarget(control());
    const next = last();
    controller.setOptions({ quality: 9, fit: false });
    expect(next.qualityLevel).toBe(9);
    expect(next.scaleViewport).toBe(false);
    expect(old.qualityLevel).toBe(6);

    // A late event from the superseded stream must not touch state.
    old.dispatchEvent(new CustomEvent('disconnect', { detail: { clean: true } }));
    expect(controller.getSnapshot().phase).toBe('connecting');
    controller.dispose();
  });

  it('removes layers and timers on dispose and on detach', async () => {
    const { controller, container } = await streaming(view());
    controller.setTarget(control());
    controller.dispose();
    expect(container.children).toHaveLength(0);
    expect(vi.getTimerCount()).toBe(0);

    const detached = await streaming(view());
    detached.controller.attach(null);
    expect(detached.container.children).toHaveLength(0);
    expect(detached.controller.getSnapshot()).toMatchObject({ phase: 'idle', handoff: false });
    expect(vi.getTimerCount()).toBe(0);
  });
});

describe('RfbController.subscribeClipboard', () => {
  it('emits clipboard text of the current control stream only', async () => {
    const { controller } = await streaming(view());
    const received: string[] = [];
    controller.subscribeClipboard((text) => received.push(text));
    last().clipboard('from view');

    controller.setTarget(control());
    const old = last();
    last().connect();
    last().clipboard('hello wörld');
    expect(received).toEqual(['hello wörld']);

    controller.setTarget(control('g1', 'w1', 'l2'));
    old.clipboard('stale');
    last().clipboard('current');
    expect(received).toEqual(['hello wörld', 'current']);
    controller.dispose();
  });

  it('stops after unsubscribe and clears everything on dispose', async () => {
    const container = new FakeElement('div');
    const controller = new RfbController();
    controller.attach(container as unknown as HTMLElement);
    controller.setTarget(control());
    const a = vi.fn();
    const b = vi.fn();
    const unsubscribeA = controller.subscribeClipboard(a);
    controller.subscribeClipboard(b);

    last().clipboard('one');
    unsubscribeA();
    last().clipboard('two');
    expect(a.mock.calls).toEqual([['one']]);
    expect(b.mock.calls).toEqual([['one'], ['two']]);

    const rfb = last();
    controller.dispose();
    rfb.clipboard('after dispose');
    expect(b).toHaveBeenCalledTimes(2);
  });
});
