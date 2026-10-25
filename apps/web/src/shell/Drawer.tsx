import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import type { ApiError } from '@/api/http';
import type { AuditEvent, FeedbackStats, RuntimeHealth, WorkspaceResources } from '@/api/types';
import { getSystem, listAudit } from '@/api/system';
import { getHealth } from '@/api/workspace';
import { Button, Chip, KeyValueRows, Notice, Tabs, tabPanelId, type Tone } from '@/components/ui';
import { describeError } from '@/errors';
import { useI18n } from '@/i18n';
import { useSession } from '@/state/session';
import { useTargets } from '@/state/targets';
import { useUi, type DrawerTab } from '@/state/ui';
import { useWorkspace } from '@/state/workspace';
import { formatBytes, formatDateTime, formatTimeOnly, shortId } from '@/util/format';
import { useClock, useRfb } from '@/vnc/DesktopProvider';

/** Bottom drawer: health, live events, and the audit trail (administrators). */
export function Drawer(): ReactNode {
  const { t } = useI18n();
  const { drawerOpen, drawerTab, setDrawer } = useUi();
  return (
    <section className={`drawer ${drawerOpen ? 'is-open' : ''}`} aria-label={t('drawer.label')}>
      <div className="drawer__bar">
        <Button
          small
          variant="ghost"
          icon={drawerOpen ? 'chevronDown' : 'chevronUp'}
          aria-expanded={drawerOpen}
          onClick={() => setDrawer(!drawerOpen)}
        >
          {t('drawer.toggle')}
        </Button>
        {drawerOpen ? (
          <Tabs<DrawerTab>
            group="drawer"
            label={t('drawer.tabs')}
            value={drawerTab}
            onChange={(tab) => setDrawer(true, tab)}
            items={[
              { id: 'health', label: t('drawer.tab.health') },
              { id: 'events', label: t('drawer.tab.events') },
              { id: 'audit', label: t('drawer.tab.audit') },
            ]}
          />
        ) : null}
      </div>
      {drawerOpen ? (
        <div className="drawer__body" role="tabpanel" id={tabPanelId('drawer', drawerTab)} aria-labelledby={`drawer-tab-${drawerTab}`}>
          {drawerTab === 'health' ? <HealthPanel /> : null}
          {drawerTab === 'events' ? <EventsPanel /> : null}
          {drawerTab === 'audit' ? <AuditPanel /> : null}
        </div>
      ) : null}
    </section>
  );
}

/** Health view and diagnosis: which layer is failing, using only facts the Gateway and runtime report. */
export function HealthPanel(): ReactNode {
  const { t } = useI18n();
  const { workspaceId, workspace, error, lastOkAt, feedStatus } = useWorkspace();
  const rfb = useRfb();
  const { targets, error: targetError } = useTargets();
  const now = useClock();
  const [health, setHealth] = useState<RuntimeHealth | null>(null);
  const [healthError, setHealthError] = useState<ApiError | null>(null);

  const load = useCallback(async (): Promise<void> => {
    try {
      setHealth(await getHealth(workspaceId));
      setHealthError(null);
    } catch (err) {
      setHealthError(err as ApiError);
    }
  }, [workspaceId]);

  useEffect(() => {
    void load();
    const id = window.setInterval(() => void load(), 10000);
    return () => window.clearInterval(id);
  }, [load]);

  const rows: { label: string; tone: Tone; value: string }[] = [
    {
      label: t('health.gateway'),
      tone: error ? 'danger' : 'ok',
      value: error ? describeError(error, t) : t('health.gateway.ok', { s: lastOkAt ? Math.round((now - lastOkAt) / 1000) : 0 }),
    },
    {
      label: t('health.runtime'),
      tone: !workspace ? 'amber' : workspace.ready ? 'ok' : 'danger',
      value: !workspace ? t('health.unknown') : workspace.ready ? t('top.health.ready') : workspace.lockCode ? `${t('top.health.notReady')} (${workspace.lockCode})` : t('top.health.notReady'),
    },
    {
      label: t('health.stream'),
      tone: rfb.phase === 'connected' ? 'ok' : rfb.phase === 'failed' ? 'danger' : 'amber',
      value: `${rfb.phase}${rfb.mode ? ` / ${rfb.mode}` : ''}${rfb.width ? ` · ${rfb.width}×${rfb.height}` : ''}`,
    },
    {
      label: t('health.events'),
      tone: feedStatus === 'open' ? 'ok' : 'amber',
      value: feedStatus === 'open' ? t('health.events.open') : t('health.events.polling'),
    },
    {
      label: t('health.targets'),
      tone: targetError ? 'danger' : 'neutral',
      value: targetError ? describeError(targetError, t) : t('health.targets.count', { n: targets.length }),
    },
  ];

  return (
    <div className="health">
      <ul className="health__rows">
        {rows.map((r) => (
          <li key={r.label}>
            <span>{r.label}</span>
            <Chip tone={r.tone} icon={r.tone === 'ok' ? 'check' : r.tone === 'neutral' ? 'activity' : 'warning'}>
              {r.value}
            </Chip>
          </li>
        ))}
      </ul>
      <div className="health__detail">
        <h4>{t('health.runtimeDetail')}</h4>
        {healthError ? <Notice tone="danger">{describeError(healthError, t)}</Notice> : null}
        {health ? <KeyValueRows data={flatten(health)} /> : !healthError ? <p>{t('common.loading')}</p> : null}
        <DiagnosticsSection />
        <p className="panel__desc">{t('health.note')}</p>
      </div>
    </div>
  );
}

/**
 * Resource and delivery counters. `null` from the server is rendered as "unknown",
 * never as 0. CPU arrives as a cumulative counter, so utilization is a delta
 * between two samples of this session, not an instantaneous reading.
 */
function DiagnosticsSection(): ReactNode {
  const { t, locale } = useI18n();
  const { workspace } = useWorkspace();
  const { actor } = useSession();
  const res = workspace?.resources ?? null;
  const [prev, setPrev] = useState<WorkspaceResources | null>(null);
  const [stats, setStats] = useState<FeedbackStats | null>(null);
  const [statsError, setStatsError] = useState<string | null>(null);
  const canReadSystem = actor?.role === 'human' || actor?.role === 'admin';

  const lastSample = useRef<WorkspaceResources | null>(null);
  useEffect(() => {
    if (!res) return;
    const previous = lastSample.current;
    if (!previous || previous.sampledAt !== res.sampledAt) {
      if (previous) setPrev(previous);
      lastSample.current = res;
    }
  }, [res]);

  useEffect(() => {
    if (!canReadSystem) return;
    let live = true;
    getSystem()
      .then((s) => {
        if (live) setStats(s.feedback ?? null);
      })
      .catch((err: unknown) => {
        if (live) setStatsError(describeError(err, t));
      });
    return () => {
      live = false;
    };
  }, [canReadSystem, t]);

  const cpuLine = ((): string => {
    if (!res) return t('health.unknown');
    const cores = res.cpu.cores;
    if (res.cpu.usageNanos === null) return t('health.unknown');
    if (prev && prev.cpu.usageNanos !== null) {
      const dUsage = (res.cpu.usageNanos - prev.cpu.usageNanos) / 1e9;
      const dSeconds = (Date.parse(res.sampledAt) - Date.parse(prev.sampledAt)) / 1000;
      if (dSeconds > 0 && dUsage >= 0) {
        const pct = cores ? Math.round((dUsage / (dSeconds * cores)) * 100) : null;
        if (pct !== null) return t('health.cpu.delta', { pct, s: Math.round(dSeconds) });
      }
    }
    return t('health.cpu.cumulative', { s: Math.round(res.cpu.usageNanos / 1e9) });
  })();

  return (
    <div className="diag">
      <h4>{t('drawer.tab.health')}</h4>
      <dl className="kv kv--tight">
        <div className="kv__row">
          <dt>{t('health.cpu')}</dt>
          <dd className="mono">
            {cpuLine}
            {res?.cpu.cores ? ` · ${t('health.cpu.cores')} ${res.cpu.cores}` : ''}
          </dd>
        </div>
        <div className="kv__row">
          <dt>{t('health.memory')}</dt>
          <dd className="mono">
            {res?.memory.currentBytes != null ? formatBytes(res.memory.currentBytes) : t('health.unknown')}
            {' / '}
            {res?.memory.maxBytes != null ? formatBytes(res.memory.maxBytes) : t('health.unknown')}
          </dd>
        </div>
        {(res?.disk ?? []).map((d) => (
          <div className="kv__row" key={d.name}>
            <dt>
              {t('health.disk')} · {d.name}
            </dt>
            <dd className="mono">
              {d.availableBytes != null ? `${formatBytes(d.availableBytes)} free / ${formatBytes(d.totalBytes)}` : t('health.disk.unknown')}
            </dd>
          </div>
        ))}
        {res ? (
          <div className="kv__row">
            <dt>{t('health.sampled')}</dt>
            <dd className="mono">{formatDateTime(res.sampledAt, locale)}</dd>
          </div>
        ) : null}
      </dl>

      <h4>{t('diag.feedback')}</h4>
      {!canReadSystem ? <p className="panel__desc">{t('diag.needHuman')}</p> : null}
      {statsError ? <p className="field__error">{statsError}</p> : null}
      {canReadSystem && !stats && !statsError ? <p className="panel__desc">{t('common.loading')}</p> : null}
      {stats ? (
        <dl className="kv kv--tight">
          <div className="kv__row">
            <dt>{t('diag.queue')}</dt>
            <dd className="mono">{stats.queueDepth}</dd>
          </div>
          <div className="kv__row">
            <dt>{t('diag.deliveries')}</dt>
            <dd className="mono">
              {Object.entries(stats.deliveries)
                .map(([k, v]) => `${k} ${v}`)
                .join(' · ')}
            </dd>
          </div>
          <div className="kv__row">
            <dt>{t('diag.drafts')}</dt>
            <dd className="mono">
              {stats.openDrafts} / {stats.limits.maxDrafts}
            </dd>
          </div>
          <div className="kv__row">
            <dt>{t('diag.captures')}</dt>
            <dd className="mono">
              {Object.entries(stats.captures)
                .map(([k, v]) => `${k} ${v}`)
                .join(' · ')}
            </dd>
          </div>
          <div className="kv__row">
            <dt>{t('diag.latency.desktop')}</dt>
            <dd className="mono">
              {stats.desktopCaptureLatency.count > 0
                ? `n=${stats.desktopCaptureLatency.count} avg ${Math.round(stats.desktopCaptureLatency.avgMs)}ms max ${Math.round(stats.desktopCaptureLatency.maxMs)}ms`
                : t('diag.noSamples')}
            </dd>
          </div>
          <div className="kv__row">
            <dt>{t('diag.latency.ack')}</dt>
            <dd className="mono">
              {stats.ackLatency.count > 0 ? `n=${stats.ackLatency.count} avg ${Math.round(stats.ackLatency.avgMs)}ms` : t('diag.noSamples')}
            </dd>
          </div>
          <div className="kv__row">
            <dt>{t('diag.artifactFailures')}</dt>
            <dd className={stats.artifactFailures > 0 ? 'mono field__error' : 'mono'}>{stats.artifactFailures}</dd>
          </div>
          <div className="kv__row">
            <dt>{t('diag.bytes')}</dt>
            <dd className="mono">
              private {formatBytes(stats.privateBytes)} / {formatBytes(stats.limits.maxPrivateBytes)} · shared {formatBytes(stats.sharedBytes)} /{' '}
              {formatBytes(stats.limits.maxSharedBytes)}
            </dd>
          </div>
          <div className="kv__row">
            <dt>{t('diag.events')}</dt>
            <dd className="mono">{stats.events}</dd>
          </div>
        </dl>
      ) : null}
    </div>
  );
}

function flatten(h: RuntimeHealth): Record<string, unknown> {
  // Rendered as returned; no field is interpreted or renamed.
  return { ...h } as Record<string, unknown>;
}

function EventsPanel(): ReactNode {
  const { t, locale } = useI18n();
  const { events } = useWorkspace();
  if (events.length === 0) return <p className="empty">{t('events.empty')}</p>;
  return (
    <ul className="events">
      {events.map((e) => (
        <li key={`${e.seq}:${e.id}:${e.time}`}>
          <span className="mono">{formatTimeOnly(e.time, locale)}</span>
          <strong>
            {e.category}.{e.action}
          </strong>
          {e.result ? <Chip tone={e.result === 'success' || e.result === 'ok' ? 'ok' : 'amber'}>{e.result}</Chip> : null}
          <span className="row__meta">{e.actorId}</span>
          {e.subjectId ? <span className="row__meta mono">{shortId(e.subjectId, 10)}{e.revision ? ` r${e.revision}` : ''}</span> : null}
        </li>
      ))}
    </ul>
  );
}

function AuditPanel(): ReactNode {
  const { t, locale } = useI18n();
  const { actor } = useSession();
  const { workspaceId, eventTick } = useWorkspace();
  const [items, setItems] = useState<AuditEvent[]>([]);
  const [error, setError] = useState<ApiError | null>(null);
  const admin = actor?.role === 'admin';

  const load = useCallback(async (): Promise<void> => {
    try {
      const merged: AuditEvent[] = [];
      let after = 0;
      for (let page = 0; page < 5; page += 1) {
        const res = await listAudit(workspaceId, { after, limit: 200 });
        merged.push(...res.items);
        if (res.items.length < 200 || res.nextSeq <= after) break;
        after = res.nextSeq;
      }
      setItems(merged.reverse());
      setError(null);
    } catch (err) {
      setError(err as ApiError);
    }
  }, [workspaceId]);

  useEffect(() => {
    if (admin) void load();
  }, [admin, load, eventTick]);

  if (!admin) return <Notice tone="neutral">{t('audit.needAdmin')}</Notice>;
  return (
    <div>
      <p className="panel__desc">{t('audit.note')}</p>
      {error ? <Notice tone="danger">{describeError(error, t)}</Notice> : null}
      {items.length === 0 && !error ? <p className="empty">{t('audit.empty')}</p> : null}
      <table className="table table--audit" role="table">
        <thead>
          <tr>
            <th scope="col">{t('audit.time')}</th>
            <th scope="col">{t('audit.action')}</th>
            <th scope="col">{t('audit.actor')}</th>
            <th scope="col">{t('audit.subject')}</th>
            <th scope="col">{t('audit.result')}</th>
          </tr>
        </thead>
        <tbody>
          {items.map((e) => (
            <tr key={e.seq}>
              <td className="mono" data-label={t('audit.time')}>{formatDateTime(e.time, locale)}</td>
              <td data-label={t('audit.action')}>
                {e.category}.{e.action}
              </td>
              <td data-label={t('audit.actor')}>{e.actorId}</td>
              <td className="mono" data-label={t('audit.subject')}>{e.subjectType ? `${e.subjectType}:${shortId(e.subjectId, 10)}` : '—'}</td>
              <td data-label={t('audit.result')}>{e.result}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
