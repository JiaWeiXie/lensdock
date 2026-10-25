import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import {
  artifactUrl,
  bundleUrl,
  cancelFeedback,
  createRevision,
  dispatchFeedback,
  getDelivery,
  getPublished,
  listFeedback,
  listRevisions,
  verifyFeedback,
} from '@/api/feedback';
import type { ApiError } from '@/api/http';
import type {
  DeliveryDetail,
  DeliveryState,
  FeedbackSummary,
  PublishedFeedback,
} from '@/api/types';
import { Button, Chip, Field, Modal, Notice, useToast } from '@/components/ui';
import { describeError } from '@/errors';
import { DELIVERY_LABEL, DELIVERY_ORDER, DeliveryChip, TERMINAL } from '@/feedback/status';
import { useI18n, type MessageKey } from '@/i18n';
import { canOperate, useSession } from '@/state/session';
import { useRoute, useUi } from '@/state/ui';
import { useWorkspace } from '@/state/workspace';
import { newKey } from '@/util/async';
import { formatBytes, formatDateTime, shortId } from '@/util/format';
import { codePointLength, truncateToCodePoints } from '@/util/text';

const FILTERS: readonly (DeliveryState | 'ALL')[] = ['ALL', 'READY', 'QUEUED', 'ACKNOWLEDGED', 'IN_PROGRESS', 'RESOLVED', 'VERIFIED', 'CANCELLED'];

export function FeedbackPanel(): ReactNode {
  const { selected } = useUi();
  return selected ? <FeedbackDetail feedbackId={selected.feedbackId} revision={selected.revision} /> : <FeedbackList />;
}

// ------------------------------------------------------------------- list

function FeedbackList(): ReactNode {
  const { t, locale } = useI18n();
  const { workspaceId, eventTick } = useWorkspace();
  const { select } = useUi();
  const [filter, setFilter] = useState<DeliveryState | 'ALL'>('ALL');
  const [items, setItems] = useState<FeedbackSummary[]>([]);
  const [cursor, setCursor] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<ApiError | null>(null);

  const load = useCallback(
    async (after?: string): Promise<void> => {
      try {
        const res = await listFeedback(workspaceId, {
          ...(filter === 'ALL' ? {} : { state: filter }),
          ...(after ? { cursor: after } : {}),
          limit: 50,
        });
        setItems((prev) => (after ? [...prev, ...res.items] : res.items));
        setCursor(res.nextCursor);
        setError(null);
      } catch (err) {
        setError(err as ApiError);
      } finally {
        setLoading(false);
      }
    },
    [workspaceId, filter],
  );

  useEffect(() => {
    void load();
  }, [load, eventTick]);

  return (
    <div className="panel-stack">
      <section className="panel">
        <h3>{t('fb.title')}</h3>
        <p className="panel__desc">{t('fb.desc')}</p>
        <Field label={t('fb.filter')} htmlFor="fb-filter">
          <select id="fb-filter" className="input" value={filter} onChange={(e) => setFilter(e.target.value as DeliveryState | 'ALL')}>
            {FILTERS.map((f) => (
              <option key={f} value={f}>
                {f === 'ALL' ? t('fb.filter.all') : t(DELIVERY_LABEL[f])}
              </option>
            ))}
          </select>
        </Field>
        {loading ? <p>{t('common.loading')}</p> : null}
        {error ? <Notice tone="danger">{describeError(error, t)}</Notice> : null}
        {!loading && items.length === 0 && !error ? <p className="empty">{t('fb.empty')}</p> : null}
        <ul className="rows">
          {items.map((i) => (
            <li key={`${i.feedbackId}:${i.revision}`} className="row row--click">
              <button type="button" className="row__open" onClick={() => select({ feedbackId: i.feedbackId, revision: i.revision })}>
                <span className="row__main">
                  <strong>{i.title}</strong>
                  <span className="row__meta mono">
                    r{i.revision} · {shortId(i.feedbackId, 8)}
                  </span>
                  <span className="row__meta">{formatDateTime(i.updatedAt ?? i.publishedAt, locale)}</span>
                </span>
                <DeliveryChip state={i.deliveryState} />
              </button>
            </li>
          ))}
        </ul>
        {cursor ? (
          <Button small onClick={() => void load(cursor)}>
            {t('common.loadMore')}
          </Button>
        ) : null}
      </section>
    </div>
  );
}

// ----------------------------------------------------------------- detail

type PendingAction = null | 'verify' | 'reject' | 'cancel';

function FeedbackDetail({ feedbackId, revision }: { feedbackId: string; revision: number }): ReactNode {
  const { t, locale } = useI18n();
  const { actor } = useSession();
  const { eventTick } = useWorkspace();
  const { select } = useUi();
  const [, navigate] = useRoute();
  const toast = useToast();
  const operator = canOperate(actor?.role);

  const [published, setPublished] = useState<PublishedFeedback | null>(null);
  const [delivery, setDelivery] = useState<DeliveryDetail | null>(null);
  const [revisions, setRevisions] = useState<FeedbackSummary[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [pending, setPending] = useState<PendingAction>(null);
  const dispatchAttempt = useRef<{ feedbackId: string; revision: number; key: string } | null>(null);
  const [reason, setReason] = useState('');
  const [preview, setPreview] = useState<string | null>(null);

  const load = useCallback(async (): Promise<void> => {
    try {
      const [p, d, r] = await Promise.all([getPublished(feedbackId, revision), getDelivery(feedbackId, revision), listRevisions(feedbackId)]);
      setPublished(p);
      setDelivery(d);
      setRevisions(r.items);
      setError(null);
    } catch (err) {
      setError(describeError(err, t));
    }
  }, [feedbackId, revision, t]);

  useEffect(() => {
    void load();
  }, [load, eventTick]);

  const run = async (fn: () => Promise<unknown>, okText: MessageKey): Promise<void> => {
    setBusy(true);
    setActionError(null);
    try {
      await fn();
      toast.push('ok', t(okText));
      setPending(null);
      setReason('');
      await load();
    } catch (err) {
      setActionError(describeError(err, t));
    } finally {
      setBusy(false);
    }
  };

  const startRevision = async (): Promise<void> => {
    setBusy(true);
    setActionError(null);
    try {
      const draft = await createRevision(feedbackId, newKey());
      select(null);
      navigate({ name: 'editor', feedbackId: draft.feedbackId });
    } catch (err) {
      setActionError(describeError(err, t));
    } finally {
      setBusy(false);
    }
  };

  const state = delivery?.state ?? null;
  const terminal = state !== null && TERMINAL.includes(state);

  return (
    <div className="panel-stack">
      <section className="panel">
        <div className="panel__head">
          <Button small variant="ghost" icon="back" onClick={() => select(null)}>
            {t('fb.back')}
          </Button>
          {state ? <DeliveryChip state={state} /> : null}
        </div>
        {error ? <Notice tone="danger">{error}</Notice> : null}
        {published ? (
          <>
            <h3>{published.task.title}</h3>
            <p className="row__meta mono">
              r{published.revision} · {shortId(published.feedbackId, 12)} · {formatDateTime(published.publishedAt, locale)}
            </p>
            <Stepper delivery={delivery} />
            <StateExplainer state={state} />
          </>
        ) : !error ? (
          <p>{t('common.loading')}</p>
        ) : null}
      </section>

      {published && delivery && operator ? (
        <section className="panel">
          <h4>{t('fb.actions')}</h4>
          {actionError ? <Notice tone="danger">{actionError}</Notice> : null}
          <div className="btn-row">
            {state === 'READY' ? (
              <Button variant="primary" icon="send" busy={busy} onClick={() => {
                if (!dispatchAttempt.current || dispatchAttempt.current.feedbackId !== feedbackId || dispatchAttempt.current.revision !== revision) {
                  dispatchAttempt.current = { feedbackId, revision, key: newKey() };
                }
                const key = dispatchAttempt.current.key;
                void run(() => dispatchFeedback(feedbackId, revision, key), 'fb.dispatch.done');
              }}>
                {t('fb.dispatch')}
              </Button>
            ) : null}
            {state === 'RESOLVED' ? (
              <>
                <Button variant="primary" icon="check" onClick={() => setPending('verify')}>
                  {t('fb.verify')}
                </Button>
                <Button variant="warn" icon="refresh" onClick={() => setPending('reject')}>
                  {t('fb.reject')}
                </Button>
              </>
            ) : null}
            {!terminal ? (
              <Button variant="danger" icon="x" onClick={() => setPending('cancel')}>
                {t('fb.cancel')}
              </Button>
            ) : null}
            <Button icon="pen" busy={busy} onClick={() => void startRevision()}>
              {t('fb.newRevision')}
            </Button>
          </div>
          <p className="panel__desc">{t('fb.revisionNote')}</p>
        </section>
      ) : null}

      {delivery?.result ? (
        <section className="panel panel--result">
          <h4>{t('fb.result.title')}</h4>
          <Notice tone={state === 'VERIFIED' ? 'ok' : 'amber'}>{state === 'VERIFIED' ? t('fb.result.verified') : t('fb.result.unverified')}</Notice>
          <h5>{t('fb.result.summary')}</h5>
          <p className="prewrap">{delivery.result.summary}</p>
          <h5>{t('fb.result.verification')}</h5>
          <p className="prewrap">{delivery.result.verification}</p>
          {delivery.result.resultFeedbackIds.length > 0 ? (
            <>
              <h5>{t('fb.result.evidence')}</h5>
              <ul className="evidence">
                {delivery.result.resultFeedbackIds.map((id) => (
                  <ResultEvidence key={id} feedbackId={id} />
                ))}
              </ul>
            </>
          ) : (
            <p className="empty">{t('fb.result.noEvidence')}</p>
          )}
        </section>
      ) : null}

      {delivery?.lastReason ? (
        <Notice tone="amber" title={t('fb.lastReason', { n: delivery.rejectionCount ?? 0 })}>
          <span className="prewrap">{delivery.lastReason}</span>
        </Notice>
      ) : null}

      {published ? <PublishedContent p={published} onPreview={setPreview} /> : null}

      {revisions.length > 1 ? (
        <section className="panel">
          <h4>{t('fb.revisions')}</h4>
          <ul className="rows">
            {revisions.map((r) => (
              <li key={r.revision} className="row row--click">
                <button type="button" className="row__open" disabled={r.revision === revision} onClick={() => select({ feedbackId, revision: r.revision })}>
                  <span className="row__main">
                    <strong>r{r.revision}</strong>
                    <span className="row__meta">{r.title}</span>
                  </span>
                  <DeliveryChip state={r.deliveryState} />
                </button>
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {delivery?.history && delivery.history.length > 0 ? (
        <section className="panel">
          <h4>{t('fb.history')}</h4>
          <ol className="history">
            {delivery.history.map((h, i) => (
              // The audit trail is append-only; its order is its identity.
              <li key={i}>
                <span className="mono">{formatDateTime(h.at, locale)}</span>
                <span>
                  {h.from} → <strong>{h.to}</strong>
                </span>
                <span className="row__meta">
                  {h.actorId} ({h.actorRole})
                </span>
                {h.reason ? <span className="prewrap">{h.reason}</span> : null}
              </li>
            ))}
          </ol>
        </section>
      ) : null}


      <Modal
        open={pending === 'verify'}
        onClose={() => setPending(null)}
        title={t('fb.verify.title')}
        dismissible={!busy}
        footer={
          <>
            <Button onClick={() => setPending(null)}>{t('common.cancel')}</Button>
            <Button variant="primary" icon="check" busy={busy} onClick={() => void run(() => verifyFeedback(feedbackId, revision, 'verified', undefined, newKey()), 'fb.verify.done')}>
              {t('fb.verify')}
            </Button>
          </>
        }
      >
        <p>{t('fb.verify.body')}</p>
        {actionError ? <Notice tone="danger">{actionError}</Notice> : null}
      </Modal>

      <ReasonModal
        open={pending === 'reject'}
        title={t('fb.reject.title')}
        body={t('fb.reject.body')}
        confirm={t('fb.reject')}
        required
        busy={busy}
        reason={reason}
        setReason={setReason}
        error={actionError}
        onClose={() => setPending(null)}
        onConfirm={() => void run(() => verifyFeedback(feedbackId, revision, 'rejected', reason.trim(), newKey()), 'fb.reject.done')}
      />
      <ReasonModal
        open={pending === 'cancel'}
        title={t('fb.cancel.title')}
        body={t('fb.cancel.body')}
        confirm={t('fb.cancel')}
        required={false}
        busy={busy}
        reason={reason}
        setReason={setReason}
        error={actionError}
        onClose={() => setPending(null)}
        onConfirm={() => void run(() => cancelFeedback(feedbackId, revision, reason.trim() || undefined, newKey()), 'fb.cancel.done')}
      />

      <Modal open={preview !== null} onClose={() => setPreview(null)} title={t('fb.image')} wide>
        {preview ? <img className="bigimg" src={preview} alt={t('fb.image')} /> : null}
      </Modal>
    </div>
  );
}

function ReasonModal({
  open,
  title,
  body,
  confirm,
  required,
  busy,
  reason,
  setReason,
  error,
  onClose,
  onConfirm,
}: {
  open: boolean;
  title: string;
  body: string;
  confirm: string;
  required: boolean;
  busy: boolean;
  reason: string;
  setReason: (s: string) => void;
  error: string | null;
  onClose: () => void;
  onConfirm: () => void;
}): ReactNode {
  const { t } = useI18n();
  return (
    <Modal
      open={open}
      onClose={onClose}
      title={title}
      dismissible={!busy}
      footer={
        <>
          <Button onClick={onClose}>{t('common.cancel')}</Button>
          <Button variant="danger" busy={busy} disabled={required && reason.trim() === ''} onClick={onConfirm}>
            {confirm}
          </Button>
        </>
      }
    >
      <p>{body}</p>
      <Field label={required ? `${t('fb.reason')} *` : t('fb.reason')} hint={t('common.count', { n: codePointLength(reason), max: 12000 })} htmlFor="reason-text">
        <textarea id="reason-text" className="input" rows={3} value={reason} onChange={(e) => setReason(truncateToCodePoints(e.target.value, 12000))} autoFocus />
      </Field>
      {error ? <Notice tone="danger">{error}</Notice> : null}
    </Modal>
  );
}

// -------------------------------------------------------------- pieces

function Stepper({ delivery }: { delivery: DeliveryDetail | null }): ReactNode {
  const { t, locale } = useI18n();
  const state = delivery?.state ?? null;
  const stamps: Record<string, string | undefined> = {
    READY: undefined,
    QUEUED: delivery?.queuedAt,
    ACKNOWLEDGED: delivery?.acknowledgedAt,
    IN_PROGRESS: delivery?.inProgressAt,
    RESOLVED: delivery?.resolvedAt,
    VERIFIED: delivery?.verifiedAt,
  };
  const currentIndex = state === 'CANCELLED' || state === null ? -1 : DELIVERY_ORDER.indexOf(state);
  return (
    <ol className="stepper" aria-label={t('fb.stepper')}>
      {DELIVERY_ORDER.map((s, i) => {
        const status = state === 'CANCELLED' ? 'skipped' : i < currentIndex ? 'done' : i === currentIndex ? 'current' : 'todo';
        return (
          <li key={s} className={`stepper__step stepper__step--${status}`} aria-current={status === 'current' ? 'step' : undefined}>
            <span className="stepper__dot" aria-hidden="true">
              {status === 'done' ? '✓' : i + 1}
            </span>
            <span className="stepper__label">{t(DELIVERY_LABEL[s])}</span>
            <span className="stepper__by">{t(`fb.by.${s}`)}</span>
            {stamps[s] ? <span className="stepper__time mono">{formatDateTime(stamps[s], locale)}</span> : null}
            <span className="sr-only">{t(`fb.stepStatus.${status}`)}</span>
          </li>
        );
      })}
    </ol>
  );
}

function StateExplainer({ state }: { state: DeliveryState | null }): ReactNode {
  const { t } = useI18n();
  if (!state) return null;
  const tone = state === 'QUEUED' || state === 'RESOLVED' ? 'amber' : state === 'VERIFIED' ? 'ok' : state === 'CANCELLED' ? 'danger' : 'neutral';
  return (
    <Notice tone={tone}>
      {t(`fb.explain.${state}`)}
    </Notice>
  );
}

function ResultEvidence({ feedbackId }: { feedbackId: string }): ReactNode {
  const { t } = useI18n();
  const { select } = useUi();
  const [latest, setLatest] = useState<FeedbackSummary | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let live = true;
    listRevisions(feedbackId)
      .then((r) => {
        if (!live) return;
        const top = [...r.items].sort((a, b) => b.revision - a.revision)[0];
        if (top) setLatest(top);
        else setFailed(true);
      })
      .catch(() => live && setFailed(true));
    return () => {
      live = false;
    };
  }, [feedbackId]);

  if (failed) return <li className="evidence__item">{t('fb.result.evidenceMissing', { id: shortId(feedbackId, 10) })}</li>;
  if (!latest) return <li className="evidence__item">{t('common.loading')}</li>;
  return (
    <li className="evidence__item">
      <img src={artifactUrl(feedbackId, 'screenshot.redacted.png', latest.revision)} alt={latest.title} loading="lazy" />
      <div>
        <strong>{latest.title}</strong>
        <p className="row__meta mono">
          r{latest.revision} · {shortId(feedbackId, 10)}
        </p>
        <Button small onClick={() => select({ feedbackId, revision: latest.revision })}>
          {t('fb.result.open')}
        </Button>
      </div>
    </li>
  );
}

function PublishedContent({ p, onPreview }: { p: PublishedFeedback; onPreview: (url: string) => void }): ReactNode {
  const { t, locale } = useI18n();
  const art = (name: string): PublishedFeedback['artifacts'][number] | undefined => p.artifacts.find((a) => a.name === name);
  const images = ['screenshot.redacted.png', 'screenshot.annotated.png'] as const;
  const c = p.capture;
  const page = c.page;

  return (
    <>
      <section className="panel">
        <h4>{t('fb.content')}</h4>
        <dl className="kv kv--tight">
          <div className="kv__row">
            <dt>{t('task.kind')}</dt>
            <dd>{t(`task.kind.${p.task.kind}`)}</dd>
          </div>
          <div className="kv__row">
            <dt>{t('task.intent')}</dt>
            <dd>{t(`task.intent.${p.task.intent}`)}</dd>
          </div>
        </dl>
        <h5>{t('task.description')}</h5>
        <p className="prewrap">{p.task.description}</p>
        {p.task.expectedResult ? (
          <>
            <h5>{t('task.expected')}</h5>
            <p className="prewrap">{p.task.expectedResult}</p>
          </>
        ) : null}
        {p.task.actualResult ? (
          <>
            <h5>{t('task.actual')}</h5>
            <p className="prewrap">{p.task.actualResult}</p>
          </>
        ) : null}
        {p.task.reproductionSteps.length > 0 ? (
          <>
            <h5>{t('task.steps')}</h5>
            <ol>
              {p.task.reproductionSteps.map((s, i) => (
                <li key={i} className="prewrap">
                  {s}
                </li>
              ))}
            </ol>
          </>
        ) : null}
      </section>

      <section className="panel">
        <h4>{t('fb.images')}</h4>
        <div className="shots">
          {images.map((name) => {
            const a = art(name);
            const url = artifactUrl(p.feedbackId, name, p.revision);
            return (
              <figure key={name} className="shot">
                <button type="button" className="shot__btn" onClick={() => onPreview(url)} aria-label={`${t('fb.image')}: ${t(`fb.artifact.${name}`)}`}>
                  <img src={url} alt={t(`fb.artifact.${name}`)} loading="lazy" />
                </button>
                <figcaption>
                  <strong>{t(`fb.artifact.${name}`)}</strong>
                  {a ? (
                    <span className="mono row__meta">
                      {formatBytes(a.sizeBytes)} · sha256 {a.sha256.slice(0, 12)}…
                    </span>
                  ) : null}
                </figcaption>
              </figure>
            );
          })}
        </div>
        <div className="btn-row">
          <a className="btn btn--secondary btn--sm" href={bundleUrl(p.feedbackId, p.revision)} download>
            <span className="btn__label">{t('fb.bundle')}</span>
          </a>
        </div>
        <p className="panel__desc">{t('fb.noRaw')}</p>
      </section>

      <section className="panel">
        <h4>{t('fb.capture')}</h4>
        <dl className="kv kv--tight">
          <Row k={t('capture.result.source')} v={c.source === 'drawing' ? t('capture.source.drawing') : c.source === 'desktop' ? t('capture.source.desktop') : t('capture.source.page')} />
          <Row k={t('capture.result.size')} v={`${c.imageWidthPx}×${c.imageHeightPx} px`} mono />
          <Row k={t(c.source === 'drawing' ? 'drawing.createdAt' : 'capture.result.clientTime')} v={formatDateTime(c.capturedAt, locale)} mono />
          <Row k={t('capture.result.serverTime')} v={formatDateTime(c.serverReceivedAt, locale)} mono />
          {c.source !== 'drawing' ? (
            <div className="kv__row">
              <dt>{t('capture.result.consistency')}</dt>
              <dd>
                <Chip tone={c.consistency === 'stable-navigation' ? 'ok' : 'amber'}>{t(`capture.consistency.${c.consistency}`)}</Chip>
              </dd>
            </div>
          ) : null}
          {c.targetId ? <Row k={t('capture.result.target')} v={c.targetId} mono /> : null}
          {page ? <Row k="URL" v={page.urlSanitized} mono /> : null}
          {page ? <Row k={t('fb.viewport')} v={`${page.viewportWidthCss}×${page.viewportHeightCss} css · ${page.pixelsPerCssX}×${page.pixelsPerCssY} px/css`} mono /> : null}
          {page ? <Row k={t('fb.browser')} v={page.browserVersion} mono /> : null}
        </dl>
        <Chip tone="teal" icon="check">
          {t('fb.sharePolicy')}
        </Chip>
      </section>

      {p.annotations.length > 0 ? (
        <section className="panel">
          <h4>{t('fb.annotations', { n: p.annotations.length })}</h4>
          <ul className="rows">
            {p.annotations.map((a) => (
              <li key={a.id} className="row">
                <span className="row__main">
                  <strong>
                    {t(`ann.kind.${a.kind}`)}
                    {a.kind === 'pin' ? ` #${a.geometry.number}` : ''}
                  </strong>
                  {a.comment ? <span className="prewrap">{a.comment}</span> : null}
                </span>
              </li>
            ))}
          </ul>
        </section>
      ) : null}
    </>
  );
}

function Row({ k, v, mono = false }: { k: string; v: string; mono?: boolean }): ReactNode {
  return (
    <div className="kv__row">
      <dt>{k}</dt>
      <dd className={mono ? 'mono' : undefined}>{v}</dd>
    </div>
  );
}
