import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { fetchCaptureImage, getCapture } from '@/api/capture';
import { getDraft, patchDraft } from '@/api/feedback';
import { ApiError } from '@/api/http';
import type { Annotation, CaptureState, DraftDetail, TaskFields } from '@/api/types';
import { describeError } from '@/errors';
import { sanitizeTaskForSave } from '@/feedback/task';
import { useI18n } from '@/i18n';
import { backoffDelay, newKey, sleep } from '@/util/async';
import { canRedo, canUndo, commit, createHistory, redo, undo, type History } from './history';
import { toWire, validateAnnotations, type ImageSize } from './model';

export type SaveState = 'saved' | 'dirty' | 'saving' | 'conflict' | 'error';

const AUTOSAVE_MS = 800;
const MAX_AUTO_RETRIES = 4;

export type LoadState =
  | { status: 'loading' }
  | { status: 'error'; message: string }
  | { status: 'ready' };

function normalizeServer(list: readonly Annotation[]): Annotation[] {
  return list.map((a) => ({ ...a, comment: a.comment ?? '' }));
}

export interface DraftEditor {
  load: LoadState;
  draft: DraftDetail | null;
  capture: CaptureState | null;
  imageUrl: string | null;
  bitmap: ImageBitmap | null;
  size: ImageSize | null;
  annotations: Annotation[];
  setAnnotations: (next: Annotation[], coalesceKey?: string) => void;
  undo: () => void;
  redo: () => void;
  canUndo: boolean;
  canRedo: boolean;
  task: TaskFields | null;
  setTask: (next: TaskFields) => void;
  saveState: SaveState;
  saveError: string | null;
  lastSavedAt: number | null;
  etag: string | null;
  previewReady: boolean;
  /** Saves pending changes now. Resolves true when the server holds the latest local state. */
  flush: () => Promise<boolean>;
  retrySave: () => void;
  reloadFromServer: () => Promise<void>;
  overwriteWithLocal: () => Promise<void>;
  /** Records the server's answer after a preview upload (same ETag, previewReady true). */
  applyPreview: (d: DraftDetail) => void;
  /** Current ETag read at call time (use after awaiting a save). */
  getEtag: () => string | null;
}

/**
 * Loads one private draft with its immutable capture image and owns every
 * edit to it: annotation history, task text, and ETag-guarded autosave.
 * A saved change always invalidates the server-side preview.
 */
export function useDraftEditor(feedbackId: string): DraftEditor {
  const { t } = useI18n();
  const tRef = useRef(t);
  tRef.current = t;
  const [load, setLoad] = useState<LoadState>({ status: 'loading' });
  const [draft, setDraft] = useState<DraftDetail | null>(null);
  const [capture, setCapture] = useState<CaptureState | null>(null);
  const [imageUrl, setImageUrl] = useState<string | null>(null);
  const [bitmap, setBitmap] = useState<ImageBitmap | null>(null);
  const [size, setSize] = useState<ImageSize | null>(null);
  const [history, setHistory] = useState<History<Annotation[]>>(() => createHistory<Annotation[]>([]));
  const [task, setTaskState] = useState<TaskFields | null>(null);
  const [saveState, setSaveState] = useState<SaveState>('saved');
  const [saveError, setSaveError] = useState<string | null>(null);
  const [lastSavedAt, setLastSavedAt] = useState<number | null>(null);
  const [etag, setEtag] = useState<string | null>(null);
  const [previewReady, setPreviewReady] = useState(false);

  const etagRef = useRef<string | null>(null);
  const annotationsRef = useRef<Annotation[]>([]);
  const taskRef = useRef<TaskFields | null>(null);
  const rev = useRef(0);
  const savedRev = useRef(0);
  const saving = useRef(false);
  const retries = useRef(0);
  const timer = useRef<number | undefined>(undefined);
  const attempt = useRef<{ json: string; key: string } | null>(null);
  const ready = useRef(false);

  annotationsRef.current = history.present;
  taskRef.current = task;

  // ----------------------------------------------------------------- loading
  useEffect(() => {
    const controller = new AbortController();
    let objectUrl: string | null = null;
    let bmp: ImageBitmap | null = null;
    ready.current = false;
    setLoad({ status: 'loading' });
    (async () => {
      const d = await getDraft(feedbackId, controller.signal);
      const c = await getCapture(d.captureId, controller.signal);
      const blob = await fetchCaptureImage(d.captureId, controller.signal);
      bmp = await createImageBitmap(blob);
      if (controller.signal.aborted) return;
      if ((c.imageWidthPx && c.imageWidthPx !== bmp.width) || (c.imageHeightPx && c.imageHeightPx !== bmp.height)) {
        throw new Error(tRef.current('editor.sizeMismatch', { a: `${c.imageWidthPx}×${c.imageHeightPx}`, b: `${bmp.width}×${bmp.height}` }));
      }
      objectUrl = URL.createObjectURL(blob);
      etagRef.current = d.etag;
      rev.current = 0;
      savedRev.current = 0;
      setDraft(d);
      setCapture(c);
      setImageUrl(objectUrl);
      setBitmap(bmp);
      setSize({ width: bmp.width, height: bmp.height });
      setHistory(createHistory(normalizeServer(d.annotations)));
      setTaskState(d.task);
      setEtag(d.etag);
      setPreviewReady(d.previewReady);
      setSaveState('saved');
      setLoad({ status: 'ready' });
      ready.current = true;
    })().catch((err: unknown) => {
      if (controller.signal.aborted) return;
      setLoad({ status: 'error', message: err instanceof Error && !(err instanceof ApiError) ? err.message : describeError(err, tRef.current) });
    });
    return () => {
      controller.abort();
      window.clearTimeout(timer.current);
      if (objectUrl) URL.revokeObjectURL(objectUrl);
      bmp?.close();
      ready.current = false;
    };
    // `t` is read through a ref: a language switch must never reload the draft and drop local edits.
  }, [feedbackId]);

  // ----------------------------------------------------------------- saving
  const saveNow = useCallback(async (): Promise<void> => {
    if (saving.current || !ready.current) return;
    for (;;) {
      const startRev = rev.current;
      if (startRev === savedRev.current) {
        setSaveState('saved');
        return;
      }
      const currentTask = taskRef.current;
      const wire = toWire(annotationsRef.current);
      if (validateAnnotations(wire).length > 0) {
        setSaveState('error');
        setSaveError(t('editor.invalid'));
        return;
      }
      const patch = { ...(currentTask ? { task: sanitizeTaskForSave(currentTask) } : {}), annotations: wire };
      const json = JSON.stringify(patch);
      const key = attempt.current?.json === json ? attempt.current.key : newKey();
      attempt.current = { json, key };
      const currentEtag = etagRef.current;
      if (!currentEtag) return;
      saving.current = true;
      setSaveState('saving');
      try {
        const res = await patchDraft(feedbackId, currentEtag, patch, key);
        etagRef.current = res.etag;
        setEtag(res.etag);
        setPreviewReady(false);
        savedRev.current = startRev;
        retries.current = 0;
        attempt.current = null;
        setSaveError(null);
        setLastSavedAt(Date.now());
      } catch (err) {
        saving.current = false;
        if (err instanceof ApiError && (err.status === 412 || err.code === 'PRECONDITION_FAILED')) {
          setSaveState('conflict');
          setSaveError(describeError(err, t));
          return;
        }
        const transient = !(err instanceof ApiError) || err.status === 0 || err.status >= 500 || err.status === 429;
        setSaveState('error');
        setSaveError(describeError(err, t));
        if (transient && retries.current < MAX_AUTO_RETRIES) {
          const delay = backoffDelay(retries.current, 1000, 15000);
          retries.current += 1;
          window.clearTimeout(timer.current);
          timer.current = window.setTimeout(() => void saveNow(), delay);
        }
        return;
      }
      saving.current = false;
    }
  }, [feedbackId, t]);

  const markDirty = useCallback((): void => {
    if (!ready.current) return;
    rev.current += 1;
    setSaveState((s) => (s === 'conflict' ? s : 'dirty'));
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => void saveNow(), AUTOSAVE_MS);
  }, [saveNow]);

  const setAnnotations = useCallback(
    (next: Annotation[], coalesceKey?: string): void => {
      setHistory((h) => commit(h, next, coalesceKey ? { coalesceKey } : {}));
      markDirty();
    },
    [markDirty],
  );

  const doUndo = useCallback((): void => {
    setHistory((h) => undo(h));
    markDirty();
  }, [markDirty]);

  const doRedo = useCallback((): void => {
    setHistory((h) => redo(h));
    markDirty();
  }, [markDirty]);

  const setTask = useCallback(
    (next: TaskFields): void => {
      taskRef.current = next;
      setTaskState(next);
      markDirty();
    },
    [markDirty],
  );

  const flush = useCallback(async (): Promise<boolean> => {
    window.clearTimeout(timer.current);
    // Wait for a save already in flight, then save whatever changed since.
    for (let i = 0; i < 50 && saving.current; i += 1) await sleep(100);
    await saveNow();
    return rev.current === savedRev.current;
  }, [saveNow]);

  const retrySave = useCallback((): void => {
    retries.current = 0;
    void saveNow();
  }, [saveNow]);

  const reloadFromServer = useCallback(async (): Promise<void> => {
    const d = await getDraft(feedbackId);
    window.clearTimeout(timer.current);
    etagRef.current = d.etag;
    rev.current = 0;
    savedRev.current = 0;
    attempt.current = null;
    setDraft(d);
    setHistory(createHistory(normalizeServer(d.annotations)));
    setTaskState(d.task);
    setEtag(d.etag);
    setPreviewReady(d.previewReady);
    setSaveError(null);
    setSaveState('saved');
  }, [feedbackId]);

  const overwriteWithLocal = useCallback(async (): Promise<void> => {
    const d = await getDraft(feedbackId);
    etagRef.current = d.etag;
    setEtag(d.etag);
    // Force a save of the local state against the server's current ETag.
    rev.current += 1;
    setSaveError(null);
    await saveNow();
  }, [feedbackId, saveNow]);

  /** Newest ETag at call time; render-time `etag` is stale across awaits. */
  const getEtag = useCallback((): string | null => etagRef.current, []);

  const applyPreview = useCallback((d: DraftDetail): void => {
    etagRef.current = d.etag;
    setEtag(d.etag);
    setPreviewReady(d.previewReady);
  }, []);

  // Never lose edits to a closed tab silently.
  useEffect(() => {
    const onBeforeUnload = (e: BeforeUnloadEvent): void => {
      if (rev.current !== savedRev.current || saving.current) e.preventDefault();
    };
    window.addEventListener('beforeunload', onBeforeUnload);
    return () => window.removeEventListener('beforeunload', onBeforeUnload);
  }, []);

  return useMemo<DraftEditor>(
    () => ({
      load,
      draft,
      capture,
      imageUrl,
      bitmap,
      size,
      annotations: history.present,
      setAnnotations,
      undo: doUndo,
      redo: doRedo,
      canUndo: canUndo(history),
      canRedo: canRedo(history),
      task,
      setTask,
      saveState,
      saveError,
      lastSavedAt,
      etag,
      previewReady,
      flush,
      retrySave,
      reloadFromServer,
      overwriteWithLocal,
      applyPreview,
      getEtag,
    }),
    [load, draft, capture, imageUrl, bitmap, size, history, setAnnotations, doUndo, doRedo, task, setTask, saveState, saveError, lastSavedAt, etag, getEtag, previewReady, flush, retrySave, reloadFromServer, overwriteWithLocal, applyPreview],
  );
}
