import { useCallback, useEffect, useState } from 'react';
import { deleteDraft, listDrafts } from '@/api/feedback';
import type { ApiError } from '@/api/http';
import type { DraftDetail } from '@/api/types';
import { useWorkspace } from '@/state/workspace';

/** Own private drafts, refreshed on workspace events. Drafts expire after a TTL, so the list is never cached long. */
export function useDrafts(): {
  drafts: DraftDetail[];
  loading: boolean;
  error: ApiError | null;
  refresh: () => Promise<void>;
  remove: (feedbackId: string) => Promise<void>;
} {
  const { workspaceId, eventTick } = useWorkspace();
  const [drafts, setDrafts] = useState<DraftDetail[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<ApiError | null>(null);

  const refresh = useCallback(async (): Promise<void> => {
    try {
      const res = await listDrafts(workspaceId);
      setDrafts(res.items);
      setError(null);
    } catch (err) {
      setError(err as ApiError);
    } finally {
      setLoading(false);
    }
  }, [workspaceId]);

  useEffect(() => {
    void refresh();
  }, [refresh, eventTick]);

  const remove = useCallback(
    async (feedbackId: string): Promise<void> => {
      await deleteDraft(feedbackId);
      await refresh();
    },
    [refresh],
  );

  return { drafts, loading, error, refresh, remove };
}
