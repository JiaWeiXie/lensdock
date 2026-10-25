import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from 'react';
import { createViewportCapture, reserveDesktopCapture, uploadDesktopCapture } from '@/api/capture';
import { createDraft } from '@/api/feedback';
import type { CaptureState, Consistency } from '@/api/types';
import { useToast } from '@/components/ui';
import { describeError } from '@/errors';
import { emptyTask } from '@/feedback/task';
import { useI18n, type MessageKey, type MessageParams } from '@/i18n';
import { useControl } from '@/state/control';
import { useTargets } from '@/state/targets';
import { useRoute } from '@/state/ui';
import { useWorkspace } from '@/state/workspace';
import { newKey } from '@/util/async';
import type { CapturedFrame } from '@/vnc/controller';
import { useRfb, useRfbController } from '@/vnc/DesktopProvider';

/** Product limits from SPEC section 18; the server enforces them again. */
export const MAX_CAPTURE_BYTES = 32 * 1024 * 1024;
export const MAX_CAPTURE_PIXELS = 24_000_000;
/** A workspace read older than this means the Gateway is not answering: frames cannot be called current. */
const MAX_WORKSPACE_AGE_MS = 15_000;
/**
 * A retry may re-upload the frame it already grabbed (same idempotency keys) only while that frame is
 * still fresh. Past this age a new click grabs a new frame instead of silently saving an old one.
 */
const MAX_FRAME_REUSE_MS = 30_000;

export type CaptureStep = 'idle' | 'grabbing' | 'reserving' | 'uploading' | 'capturing' | 'drafting' | 'done' | 'failed';

/** Progress wording for the steps that have one; shared by the top bar and the capture panel. */
export const CAPTURE_STEP_KEYS: Partial<Record<CaptureStep, MessageKey>> = {
  grabbing: 'capture.step.grabbing',
  reserving: 'capture.step.reserving',
  uploading: 'capture.step.uploading',
  capturing: 'capture.step.capturing',
  drafting: 'capture.step.drafting',
};

export type CaptureWarning =
  | { kind: 'consistency'; value: Consistency }
  | { kind: 'foreground'; title: string | null }
  | { kind: 'agentTargetUnknown' }
  | { kind: 'frozenNotRemote' }
  | { kind: 'userSideCapture' };

/** Result of an explicit page-viewport capture, confirmed by the person before a draft exists. */
export interface CaptureResult {
  capture: CaptureState;
  warnings: CaptureWarning[];
  targetTitle: string | null;
}

/** Which workflow the visible error belongs to: the desktop route is retried from the top bar, a page capture from the dock. */
export type CaptureErrorSource = 'desktop' | 'page';

interface CaptureCtx {
  step: CaptureStep;
  busy: boolean;
  result: CaptureResult | null;
  error: string | null;
  errorSource: CaptureErrorSource | null;
  /** True once a desktop PNG is stored on the Gateway but its draft is not created yet; retry then only creates the draft. */
  pendingDraft: boolean;
  /** Why a desktop capture cannot start now, or null. */
  desktopBlocked: MessageKey | null;
  /**
   * The single desktop route: grabs the current frame, stores it, creates a blank draft and opens the
   * editor. Calling it again after a failure resumes the same attempt (same idempotency keys) and never
   * stores a second PNG once the first upload succeeded. Resolves true when the editor was opened.
   */
  annotateDesktop: () => Promise<boolean>;
  /** Drops the unfinished desktop attempt and its error. A stored but undrafted capture is abandoned. */
  discardDesktop: () => void;
  /**
   * Captures a target's viewport. Needs a lease that holds control: this
   * session's HUMAN lease, or the AGENT lease id when the user accepted the
   * interruption. When idle it takes a HUMAN lease first.
   */
  capturePage: (targetId: string, opts: { interruptAgent: boolean }) => Promise<CaptureResult | null>;
  clear: () => void;
}

const Ctx = createContext<CaptureCtx | null>(null);

class GateError extends Error {
  constructor(
    readonly key: MessageKey,
    readonly params?: MessageParams,
  ) {
    super(key);
    this.name = 'GateError';
  }
}

/** One desktop capture in flight. Keys and server answers survive failures so a retry never duplicates work. */
interface DesktopAttempt {
  startedAt: number;
  generation: string;
  frame: CapturedFrame;
  reserveKey: string;
  uploadKey: string;
  draftKey: string;
  reserved: CaptureState | null;
  capture: CaptureState | null;
  warnings: CaptureWarning[];
}

export function warningText(w: CaptureWarning, t: (k: MessageKey, p?: MessageParams) => string): string {
  switch (w.kind) {
    case 'consistency':
      return t(`capture.warn.consistency.${w.value}`);
    case 'foreground':
      return w.title ? t('capture.warn.foreground', { title: w.title }) : t('capture.warn.foregroundUnknown');
    case 'agentTargetUnknown':
      return t('capture.warn.agentTarget');
    case 'frozenNotRemote':
      return t('capture.warn.frozen');
    case 'userSideCapture':
      return t('capture.warn.clientSide');
  }
}

export function CaptureProvider({ children }: { children: ReactNode }): ReactNode {
  const { t } = useI18n();
  const toast = useToast();
  const [, navigate] = useRoute();
  const rfb = useRfbController();
  const rfbState = useRfb();
  const { workspaceId, workspace, error: pollError, lastOkAt } = useWorkspace();
  const { lease, take, reconcile } = useControl();
  const { targets, refresh: refreshTargets } = useTargets();
  const [step, setStep] = useState<CaptureStep>('idle');
  const [result, setResult] = useState<CaptureResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [errorSource, setErrorSource] = useState<CaptureErrorSource | null>(null);
  const [pendingDraft, setPendingDraft] = useState(false);
  const running = useRef(false);
  const desktopAttempt = useRef<DesktopAttempt | null>(null);

  const gate = useCallback((): MessageKey | null => {
    if (!workspace) return 'capture.gate.unknown';
    if (!workspace.ready) return 'capture.gate.notReady';
    if (pollError !== null || lastOkAt === null || Date.now() - lastOkAt > MAX_WORKSPACE_AGE_MS) return 'capture.gate.stale';
    if (!rfb.isLive()) return 'capture.gate.notLive';
    if (rfb.getGeneration() !== workspace.runtimeGeneration) return 'capture.gate.generation';
    return null;
    // `phase` is a dependency so the gate is re-evaluated when the stream goes up or down.
  }, [workspace, pollError, lastOkAt, rfb, rfbState.phase]);

  const desktopBlocked = gate();

  const finish = useCallback((r: CaptureResult): CaptureResult => {
    setResult(r);
    setStep('done');
    return r;
  }, []);

  const fail = useCallback(
    (err: unknown, source: CaptureErrorSource): null => {
      setStep('failed');
      setErrorSource(source);
      setError(err instanceof GateError ? t(err.key, err.params) : describeError(err, t));
      return null;
    },
    [t],
  );

  const annotateDesktop = useCallback(async (): Promise<boolean> => {
    if (running.current) return false;
    running.current = true;
    setError(null);
    setErrorSource(null);
    setResult(null);
    try {
      let attempt = desktopAttempt.current;
      // A stored capture is always resumed. A frame that was never stored is reused only while fresh and
      // still from the same runtime generation; otherwise this click starts over with a new frame.
      if (attempt && !attempt.capture && (Date.now() - attempt.startedAt > MAX_FRAME_REUSE_MS || attempt.generation !== workspace?.runtimeGeneration)) {
        attempt = null;
        desktopAttempt.current = null;
      }

      if (!attempt?.capture) {
        const blocked = gate();
        if (blocked) throw new GateError(blocked);
      }

      if (!attempt) {
        setStep('grabbing');
        const frame = await rfb.captureFrame();
        // The frame must still belong to the generation we validated.
        const generation = workspace?.runtimeGeneration;
        if (!generation || rfb.getGeneration() !== generation) throw new GateError('capture.gate.generation');
        if (frame.blob.size > MAX_CAPTURE_BYTES) throw new GateError('capture.gate.tooLarge');
        if (frame.width * frame.height > MAX_CAPTURE_PIXELS) throw new GateError('capture.gate.tooManyPixels');

        const visible = targets.filter((x) => x.visible === true);
        const warnings: CaptureWarning[] = [];
        if (workspace?.controlState === 'AGENT') warnings.push({ kind: 'agentTargetUnknown' });
        if (targets.length > 1) warnings.push({ kind: 'foreground', title: visible.length === 1 ? (visible[0]?.title ?? null) : null });
        attempt = {
          startedAt: Date.now(),
          generation,
          frame,
          reserveKey: newKey(),
          uploadKey: newKey(),
          draftKey: newKey(),
          reserved: null,
          capture: null,
          warnings,
        };
        desktopAttempt.current = attempt;
      }

      if (!attempt.capture) {
        if (!attempt.reserved) {
          setStep('reserving');
          attempt.reserved = await reserveDesktopCapture(workspaceId, attempt.reserveKey);
        }
        setStep('uploading');
        attempt.capture = await uploadDesktopCapture(attempt.reserved.captureId, attempt.frame.blob, attempt.frame.capturedAt, attempt.uploadKey);
        setPendingDraft(true);
      }

      const capture = attempt.capture;
      if (capture.status !== 'ready') {
        // A capture that is not editable cannot be resumed; the next click captures again.
        desktopAttempt.current = null;
        setPendingDraft(false);
        throw new Error(t('capture.result.notReady', { status: capture.status }));
      }

      setStep('drafting');
      const draft = await createDraft(workspaceId, capture.captureId, emptyTask(), attempt.draftKey);
      desktopAttempt.current = null;
      setPendingDraft(false);
      setStep('idle');

      // The frozen/client-side/foreground facts stay visible after the editor opens, without a click.
      const warnings: CaptureWarning[] = [...attempt.warnings];
      if (capture.consistency !== 'stable-navigation') warnings.push({ kind: 'consistency', value: capture.consistency });
      for (const w of warnings) toast.push('warn', warningText(w, t));
      toast.push('info', `${warningText({ kind: 'frozenNotRemote' }, t)} ${warningText({ kind: 'userSideCapture' }, t)}`);
      navigate({ name: 'editor', feedbackId: draft.feedbackId });
      return true;
    } catch (err) {
      if (err instanceof Error && err.message === 'RFB_NOT_LIVE') fail(new GateError('capture.gate.notLive'), 'desktop');
      else fail(err, 'desktop');
      return false;
    } finally {
      running.current = false;
    }
  }, [gate, rfb, workspace, workspaceId, targets, fail, navigate, toast, t]);

  const discardDesktop = useCallback(() => {
    if (running.current) return;
    desktopAttempt.current = null;
    setPendingDraft(false);
    setError(null);
    setErrorSource(null);
    setStep('idle');
  }, []);

  const capturePage = useCallback(
    async (targetId: string, opts: { interruptAgent: boolean }): Promise<CaptureResult | null> => {
      if (running.current) return null;
      running.current = true;
      setError(null);
      setErrorSource(null);
      setResult(null);
      try {
        if (!workspace?.ready) throw new GateError('capture.gate.notReady');
        const target = targets.find((x) => x.id === targetId);
        if (!target) throw new GateError('capture.gate.targetGone');
        setStep('capturing');

        let leaseId = lease?.leaseId ?? null;
        let interrupt = false;
        if (!leaseId) {
          if (workspace.controlState === 'AGENT') {
            if (!opts.interruptAgent) throw new GateError('capture.gate.agentHolds');
            leaseId = workspace.lease?.leaseId ?? null;
            if (!leaseId) throw new GateError('capture.gate.leaseUnknown');
            interrupt = true;
          } else if (workspace.controlState === 'IDLE') {
            const granted = await take('');
            if (!granted) throw new GateError('capture.gate.noLease');
            leaseId = granted.leaseId;
          } else {
            throw new GateError('capture.gate.stateBlocks');
          }
        }

        const capture = await createViewportCapture(
          workspaceId,
          { targetId, leaseId, ...(interrupt ? { allowInterrupt: true } : {}) },
          newKey(),
        );
        // Control is restored under a new lease id/epoch. Re-read it before returning so the next
        // lease-bound request (clipboard, resolution) carries the restored epoch, not a stale one.
        await reconcile();
        void refreshTargets();

        const warnings: CaptureWarning[] = [{ kind: 'frozenNotRemote' }];
        if (capture.consistency !== 'stable-navigation') warnings.push({ kind: 'consistency', value: capture.consistency });
        return finish({ capture, warnings, targetTitle: target.title });
      } catch (err) {
        return fail(err, 'page');
      } finally {
        running.current = false;
      }
    },
    [workspace, workspaceId, targets, lease, take, reconcile, refreshTargets, finish, fail],
  );

  /** Closes the page-capture result card or its error; an unfinished desktop attempt is left alone. */
  const clear = useCallback(() => {
    setResult(null);
    setError(null);
    setErrorSource(null);
    setStep('idle');
  }, []);

  const busy = step === 'grabbing' || step === 'reserving' || step === 'uploading' || step === 'capturing' || step === 'drafting';
  const value = useMemo<CaptureCtx>(
    () => ({ step, busy, result, error, errorSource, pendingDraft, desktopBlocked, annotateDesktop, discardDesktop, capturePage, clear }),
    [step, busy, result, error, errorSource, pendingDraft, desktopBlocked, annotateDesktop, discardDesktop, capturePage, clear],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useCapture(): CaptureCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error('useCapture outside CaptureProvider');
  return v;
}
