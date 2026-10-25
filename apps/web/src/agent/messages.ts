import { useCallback } from 'react';
import { ApiError } from '@/api/http';
import { describeError } from '@/errors';
import { useI18n, type MessageKey, type MessageParams } from '@/i18n';

export type AgentMessageKey = Extract<MessageKey, `agent.${string}`> extends `agent.${infer Key}` ? Key : never;

export type AgentT = (key: AgentMessageKey, params?: MessageParams) => string;

/** Usage-mode messages use the same typed catalog as every other UI feature. */
export function useAgentText(): AgentT {
  const { t } = useI18n();
  return useCallback((key, params) => t(`agent.${key}`, params), [t]);
}


const OWN_ERRORS: Record<string, true> = { AGENT_MODE_DISABLED: true, AGENT_PAUSED: true, UPSTREAM_UNAVAILABLE: true };

/** Detail line for a failed mode/pause call: this feature's stable codes first, the shared catalog otherwise. */
export function useDescribeAgentError(): (err: unknown) => string {
  const { t } = useI18n();
  const at = useAgentText();
  return useCallback(
    (err) => {
      if (err instanceof ApiError && OWN_ERRORS[err.code] === true) {
        const base = at(`err.${err.code}` as AgentMessageKey);
        return err.requestId ? `${base} [${err.requestId}]` : base;
      }
      return describeError(err, t);
    },
    [t, at],
  );
}
