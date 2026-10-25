import { describe, expect, it } from 'vitest';
import { parseAgentControl } from './workspace';

const ok = { mode: 'AGENT', paused: false, revision: 3, updatedAt: '2026-10-25T00:00:00Z' };

describe('parseAgentControl', () => {
  it('accepts exactly the documented shape', () => {
    expect(parseAgentControl(ok)).toEqual(ok);
    expect(parseAgentControl({ ...ok, mode: 'NOVNC', paused: true })).toMatchObject({ mode: 'NOVNC', paused: true });
  });

  it('fails closed on missing or malformed policy instead of assuming AGENT', () => {
    expect(parseAgentControl(undefined)).toBeNull();
    expect(parseAgentControl(null)).toBeNull();
    expect(parseAgentControl('AGENT')).toBeNull();
    expect(parseAgentControl({})).toBeNull();
    expect(parseAgentControl({ ...ok, mode: 'agent' })).toBeNull();
    expect(parseAgentControl({ ...ok, mode: undefined })).toBeNull();
    expect(parseAgentControl({ ...ok, paused: 'false' })).toBeNull();
    expect(parseAgentControl({ ...ok, revision: 1.5 })).toBeNull();
    expect(parseAgentControl({ ...ok, revision: -1 })).toBeNull();
    expect(parseAgentControl({ ...ok, updatedAt: 0 })).toBeNull();
  });

  it('drops unknown extra fields', () => {
    expect(parseAgentControl({ ...ok, token: 'x' })).toEqual(ok);
  });
});
