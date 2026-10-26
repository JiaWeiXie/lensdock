import { describe, expect, it } from 'vitest';
import { createSingleFlight } from './async';

describe('createSingleFlight', () => {
  it('collapses concurrent callers onto one call and shares its result', async () => {
    let calls = 0;
    const gate = Promise.withResolvers<string>();
    const flight = createSingleFlight(() => {
      calls += 1;
      return gate.promise;
    });

    const first = flight.run();
    const second = flight.run();
    expect(flight.pending).toBe(true);
    gate.resolve('lease');

    await expect(Promise.all([first, second])).resolves.toEqual(['lease', 'lease']);
    expect(calls).toBe(1);
    expect(flight.pending).toBe(false);
  });

  it('starts a fresh call after the previous one settled, including after a failure', async () => {
    let calls = 0;
    const flight = createSingleFlight(async () => {
      calls += 1;
      if (calls === 1) throw new Error('denied');
      return calls;
    });

    await expect(flight.run()).rejects.toThrow('denied');
    expect(flight.pending).toBe(false);
    await expect(flight.run()).resolves.toBe(2);
  });

  it('shares a failure with every joined caller', async () => {
    const gate = Promise.withResolvers<void>();
    const flight = createSingleFlight(async () => {
      await gate.promise;
      throw new Error('busy');
    });

    const a = flight.run();
    const b = flight.run();
    gate.resolve();
    await expect(a).rejects.toThrow('busy');
    await expect(b).rejects.toThrow('busy');
  });
});
