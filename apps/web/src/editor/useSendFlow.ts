import { useCallback, useRef, useState } from 'react';
import { publishDraft, uploadDraftPreview } from '@/api/feedback';
import { ApiError } from '@/api/http';
import type { PublishedFeedback } from '@/api/types';
import { describeError } from '@/errors';
import { validateTask } from '@/feedback/task';
import { useI18n, type MessageKey } from '@/i18n';
import { newKey } from '@/util/async';
import { toWire, validateAnnotations } from './model';
import { ExportError, FontNotReadyError, renderAnnotatedPng } from './raster';
import type { DraftEditor } from './useDraftEditor';

/** Where a send is. Everything except `idle` locks the editor; `done` stays locked while the route changes. */
export type SendPhase = 'idle' | 'saving' | 'rendering' | 'uploading' | 'publishing' | 'done';

export interface SendProblem {
  /** `stale`: the server draft moved on; local edits are untouched until the human reloads. */
  kind: 'failed' | 'stale';
  message: string;
}

export interface SendFlow {
  phase: SendPhase;
  /** True from the click until navigation; the canvas, tools and notes accept no edits. */
  locked: boolean;
  problem: SendProblem | null;
  /** Validation problems that stopped a send before anything left the browser. */
  issues: MessageKey[];
  send: () => Promise<void>;
  dismissProblem: () => void;
  /** Replaces local content with the server's current draft; only ever a human choice after a stale error. */
  reloadLatest: () => Promise<void>;
}

/**
 * One approved attempt. It binds the exact draft ETag, the exact PNG bytes and the two idempotency keys,
 * so a retry after an unknown response repeats the same requests instead of creating a second publish.
 */
interface Attempt {
  baseEtag: string;
  approvedEtag: string | null;
  png: Blob | null;
  previewKey: string;
  publishKey: string;
}

type Stage = 'save' | 'preview' | 'publish';

/** A failure with a ready-made message that is not an HTTP answer. */
class SendFailure extends Error {}

function issueKeysOf(ed: DraftEditor): MessageKey[] {
  const task = ed.task ? validateTask(ed.task) : [];
  const ann = validateAnnotations(toWire(ed.annotations));
  return [
    ...task.map((i) => `publish.issue.task.${i}` as MessageKey),
    ...ann.map((i) => `publish.issue.ann.${i.code}` as MessageKey),
  ];
}

/**
 * Entry, arrow drag, Send: a single click saves, renders, uploads the preview, checks it is ready and
 * publishes with the same ETag. Clicking is the human approval of what is on the canvas, so there is no
 * second dialog. Edits are refused for the whole run and the approved snapshot is re-checked before publish.
 */
export function useSendFlow(editor: DraftEditor, onSent: (p: PublishedFeedback) => void, onBlocked: () => void): SendFlow {
  const { t } = useI18n();
  const [phase, setPhase] = useState<SendPhase>('idle');
  const [problem, setProblem] = useState<SendProblem | null>(null);
  const [showIssues, setShowIssues] = useState(false);

  const editorRef = useRef(editor);
  editorRef.current = editor;
  const tRef = useRef(t);
  tRef.current = t;
  const onSentRef = useRef(onSent);
  onSentRef.current = onSent;
  const onBlockedRef = useRef(onBlocked);
  onBlockedRef.current = onBlocked;

  /** Synchronous single-flight latch: a double click never reaches a second request. */
  const inflight = useRef(false);
  const finished = useRef(false);
  const attemptRef = useRef<Attempt | null>(null);

  const send = useCallback(async (): Promise<void> => {
    if (inflight.current || finished.current) return;
    const start = editorRef.current;
    if (start.load.status !== 'ready' || !start.draft || !start.bitmap || !start.size) return;
    if (issueKeysOf(start).length > 0) {
      setProblem(null);
      setShowIssues(true);
      onBlockedRef.current();
      return;
    }

    inflight.current = true;
    setShowIssues(false);
    setProblem(null);
    setPhase('saving');
    const frozen = {
      feedbackId: start.draft.feedbackId,
      bitmap: start.bitmap,
      size: start.size,
      annotations: start.annotations,
      task: start.task,
    };
    let stage: Stage = 'save';
    let attempt = attemptRef.current;
    try {
      if (!(await editorRef.current.flush())) throw new SendFailure(tRef.current('publish.err.unsaved'));
      const saved = editorRef.current;
      // The click approved this exact content; anything that moved while saving is not approved.
      if (saved.annotations !== frozen.annotations || saved.task !== frozen.task) throw new SendFailure(tRef.current('send.err.changed'));
      const etag = saved.getEtag();
      if (!etag) throw new SendFailure(tRef.current('publish.err.notLoaded'));
      if (!attempt || (attempt.baseEtag !== etag && attempt.approvedEtag !== etag)) {
        attempt = { baseEtag: etag, approvedEtag: null, png: null, previewKey: newKey(), publishKey: newKey() };
        attemptRef.current = attempt;
      }

      if (!attempt.approvedEtag) {
        stage = 'preview';
        setPhase('rendering');
        attempt.png ??= (await renderAnnotatedPng(frozen.bitmap, frozen.size, toWire(frozen.annotations))).blob;
        setPhase('uploading');
        const detail = await uploadDraftPreview(frozen.feedbackId, attempt.baseEtag, attempt.png, attempt.previewKey);
        editorRef.current.applyPreview(detail);
        if (!detail.previewReady) {
          attempt.previewKey = newKey();
          throw new SendFailure(tRef.current('publish.err.previewNotReady'));
        }
        attempt.approvedEtag = detail.etag;
      }

      stage = 'publish';
      setPhase('publishing');
      const result = await publishDraft(frozen.feedbackId, attempt.approvedEtag, attempt.publishKey);
      attemptRef.current = null;
      finished.current = true;
      setPhase('done');
      onSentRef.current(result);
    } catch (err) {
      const text = tRef.current;
      if (err instanceof SendFailure) {
        setProblem({ kind: 'failed', message: err.message });
      } else if (err instanceof FontNotReadyError) {
        setProblem({ kind: 'failed', message: text('publish.err.fonts') });
      } else if (err instanceof ExportError) {
        setProblem({ kind: 'failed', message: text(err.reason === 'too-large' ? 'publish.err.tooLarge' : 'publish.err.encode') });
      } else if (err instanceof ApiError && (err.status === 412 || err.code === 'PRECONDITION_FAILED')) {
        attemptRef.current = null;
        setProblem({ kind: 'stale', message: text('send.err.stale') });
      } else if (err instanceof ApiError && err.code === 'PREVIEW_REQUIRED' && attempt) {
        // The server dropped the preview; the next click renders and uploads again under fresh keys.
        attempt.approvedEtag = null;
        attempt.previewKey = newKey();
        attempt.publishKey = newKey();
        setProblem({ kind: 'failed', message: text('publish.err.previewNotReady') });
      } else if (err instanceof ApiError && err.status >= 400 && err.status < 500 && err.status !== 408 && err.status !== 429) {
        // A definitive 4xx never stored a result worth replaying; the next try uses a fresh key.
        if (attempt) {
          if (stage === 'publish') attempt.publishKey = newKey();
          else attempt.previewKey = newKey();
        }
        setProblem({ kind: 'failed', message: describeError(err, text) });
      } else {
        // Unknown outcome: keep the keys, ETag and PNG so the retry repeats the same requests.
        const base = describeError(err, text);
        setProblem({ kind: 'failed', message: stage === 'publish' ? `${base} ${text('send.err.unknown')}` : base });
      }
    } finally {
      inflight.current = false;
      if (!finished.current) setPhase('idle');
    }
  }, []);

  const reloadLatest = useCallback(async (): Promise<void> => {
    if (inflight.current || finished.current) return;
    inflight.current = true;
    try {
      await editorRef.current.reloadFromServer();
      attemptRef.current = null;
      setProblem(null);
    } catch (err) {
      setProblem({ kind: 'stale', message: describeError(err, tRef.current) });
    } finally {
      inflight.current = false;
    }
  }, []);

  const dismissProblem = useCallback((): void => setProblem(null), []);

  return {
    phase,
    locked: phase !== 'idle',
    problem,
    issues: showIssues ? issueKeysOf(editor) : [],
    send,
    dismissProblem,
    reloadLatest,
  };
}
