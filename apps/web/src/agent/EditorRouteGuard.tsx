import { useEffect, useRef, type ReactNode } from 'react';
import { Notice } from '@/components/ui';
import { AnnotationEditorView } from '@/editor/AnnotationEditor';
import { useRoute } from '@/state/ui';
import { useAgentControl } from './AgentControlProvider';
import { useAgentText } from './messages';

/**
 * Only opens the annotation editor while Agent collaboration is confirmed on.
 *
 * - noVNC-only: the editor is unreachable, and a stale `#/editor/…` link goes back to the workspace.
 * - Unknown/loading on entry: a notice instead of the editor (fail closed).
 * - Unknown after the editor is already open (a transient Gateway blip): it stays mounted so unsent
 *   edits are not lost; the editor itself disables Send while the mode is unknown. A confirmed
 *   switch to noVNC-only closes it.
 */
export function EditorRouteGuard({ feedbackId }: { feedbackId: string }): ReactNode {
  const agent = useAgentControl();
  const at = useAgentText();
  const [, navigate] = useRoute();
  const opened = useRef(false);

  if (agent.collaboration === 'enabled') opened.current = true;
  if (agent.collaboration === 'disabled') opened.current = false;
  const open = agent.collaboration === 'enabled' || (agent.collaboration === 'unknown' && opened.current);

  useEffect(() => {
    if (agent.collaboration === 'disabled') navigate({ name: 'workspace' });
  }, [agent.collaboration, navigate]);

  if (open) return <AnnotationEditorView feedbackId={feedbackId} />;

  const disabled = agent.collaboration === 'disabled';
  return (
    <div className="agent-guard">
      <Notice tone={disabled ? 'neutral' : 'amber'} title={disabled ? at('guard.disabled.title') : at('guard.unknown.title')}>
        {disabled ? at('guard.disabled.body') : at('guard.unknown.body')}
      </Notice>
      <p>
        <a href="#/settings">{at('guard.openSettings')}</a>
        {' · '}
        <a href="#/">{at('guard.back')}</a>
      </p>
    </div>
  );
}
