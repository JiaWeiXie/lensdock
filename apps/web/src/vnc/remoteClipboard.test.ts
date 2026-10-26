import { describe, expect, it } from 'vitest';
import { RemoteClipboardSync, recoverUtf8, type RemoteClipboardEnv, type RemoteClipboardFailure } from './remoteClipboard';

/** Latin-1 view of UTF-8 bytes, which is how noVNC hands over x11vnc's ServerCutText. */
function asLatin1(text: string): string {
  return String.fromCharCode(...new TextEncoder().encode(text));
}

function setup(options: { enabled?: boolean; focused?: boolean; fail?: boolean } = {}) {
  const writes: string[] = [];
  const gates: Array<PromiseWithResolvers<void>> = [];
  const env: RemoteClipboardEnv & { focused: boolean; fail: boolean; hold: boolean } = {
    focused: options.focused ?? true,
    fail: options.fail ?? false,
    hold: false,
    hasFocus() {
      return this.focused;
    },
    writeText: async (text: string) => {
      writes.push(text);
      if (env.hold) {
        const gate = Promise.withResolvers<void>();
        gates.push(gate);
        await gate.promise;
      }
      if (env.fail) throw new DOMException('blocked', 'NotAllowedError');
    },
  };
  const sync = new RemoteClipboardSync(env, options.enabled ?? true);
  const failures: RemoteClipboardFailure[] = [];
  sync.onFailure = (reason) => failures.push(reason);
  return { sync, env, writes, gates, failures };
}

/** Lets the sync's pending promise chains settle; no wall-clock time is involved. */
async function flush(): Promise<void> {
  for (let i = 0; i < 10; i += 1) await Promise.resolve();
}

describe('recoverUtf8', () => {
  it('restores CJK that x11vnc forwarded as Latin-1 bytes, exactly', () => {
    const text = '繁體中文 \u{1F600} e\u0301\r\n tail ';
    expect(recoverUtf8(asLatin1(text))).toBe(text);
  });

  it('leaves ASCII, real Latin-1 and text that is already Unicode alone', () => {
    expect(recoverUtf8('plain')).toBe('plain');
    expect(recoverUtf8('caf\u00e9')).toBe('caf\u00e9');
    expect(recoverUtf8('繁體')).toBe('繁體');
  });

  it('keeps truncated UTF-8 as received instead of inventing text', () => {
    const cut = asLatin1('中').slice(0, 2);
    expect(recoverUtf8(cut)).toBe(cut);
  });
});

describe('RemoteClipboardSync', () => {
  it('writes the exact text while enabled, focused and bound to a control session', async () => {
    const { sync, writes } = setup();
    sync.bind('control:a');
    sync.receive(asLatin1('中文 text'));
    await flush();
    expect(writes).toEqual(['中文 text']);
    expect(sync.getSnapshot().status).toBe('copied');
    expect(sync.getSnapshot().fallbackText).toBeNull();
  });

  it('ignores events without a control session, such as the view-only stream or a lost lease', async () => {
    const { sync, writes } = setup();
    sync.receive('x');
    sync.bind('control:a');
    sync.bind(null);
    sync.receive('y');
    await flush();
    expect(writes).toEqual([]);
    expect(sync.getSnapshot().hasLatest).toBe(false);
  });

  it('stops writing when disabled and never replays older text when re-enabled', async () => {
    const { sync, writes } = setup();
    sync.bind('control:a');
    sync.receive('one');
    await flush();
    sync.setEnabled(false);
    sync.receive('two');
    await flush();
    sync.setEnabled(true);
    await flush();
    expect(writes).toEqual(['one']);
    expect(sync.getSnapshot().hasLatest).toBe(false);
  });

  it('writes serially and only the newest text that queued behind an in-flight write', async () => {
    const { sync, env, writes, gates } = setup();
    env.hold = true;
    sync.bind('control:a');
    sync.receive('a');
    await flush();
    sync.receive('b');
    sync.receive('c');
    expect(writes).toEqual(['a']);
    gates[0]?.resolve();
    await flush();
    expect(writes).toEqual(['a', 'c']);
    gates[1]?.resolve();
    await flush();
    expect(sync.getSnapshot().status).toBe('copied');
  });

  it('drops the result and queued text of a session that ended mid-write', async () => {
    const { sync, env, writes, gates } = setup();
    env.hold = true;
    sync.bind('control:a');
    sync.receive('old');
    await flush();
    sync.receive('queued');
    sync.bind('control:b');
    gates[0]?.resolve();
    await flush();
    expect(writes).toEqual(['old']);
    expect(sync.getSnapshot().status).toBe('idle');
    expect(sync.getSnapshot().hasLatest).toBe(false);
  });

  it('keeps the text for manual copy and reports once when the page is not focused', async () => {
    const { sync, env, writes, failures } = setup({ focused: false });
    sync.bind('control:a');
    sync.receive('first');
    await flush();
    sync.receive('second');
    await flush();
    expect(writes).toEqual([]);
    expect(failures).toEqual(['unfocused']);
    expect(sync.getSnapshot()).toMatchObject({ status: 'unfocused', fallbackText: 'second', hasLatest: true });

    env.focused = true;
    await expect(sync.copyLatest()).resolves.toBe(true);
    expect(writes).toEqual(['second']);
    expect(sync.getSnapshot()).toMatchObject({ status: 'copied', fallbackText: null });
  });

  it('reports a denied write with the text available for manual copying', async () => {
    const { sync, writes, failures } = setup({ fail: true });
    sync.bind('control:a');
    sync.receive('secret');
    await flush();
    expect(writes).toEqual(['secret']);
    expect(failures).toEqual(['denied']);
    expect(sync.getSnapshot()).toMatchObject({ status: 'denied', fallbackText: 'secret' });
  });

  it('reports an unavailable clipboard API instead of pretending success', async () => {
    const sync = new RemoteClipboardSync({ writeText: undefined, hasFocus: () => true }, true);
    sync.bind('control:a');
    sync.receive('x');
    await flush();
    expect(sync.getSnapshot()).toMatchObject({ status: 'unavailable', fallbackText: 'x' });
  });
});
