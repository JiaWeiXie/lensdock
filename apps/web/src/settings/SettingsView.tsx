import { AgentModePanel } from '@/agent/AgentModePanel';
import { useCallback, useEffect, useState, type ReactNode } from 'react';
import type { ApiError } from '@/api/http';
import { getSystem } from '@/api/system';
import type { ReleaseInfo, StorageUsage, SystemInfo, UpdateCandidate, UpdatePhase, UpdateStatus, UpdateStorage } from '@/api/types';
import { Button, Chip, Notice, useToast, type Tone } from '@/components/ui';
import { describeError } from '@/errors';
import { useI18n, type Lang, type MessageKey } from '@/i18n';
import { useSession } from '@/state/session';
import { FONT_SIZES, useUi } from '@/state/ui';
import { useWorkspace } from '@/state/workspace';
import { formatBytes, formatDateTime } from '@/util/format';

const PHASE_TONE: Record<UpdatePhase, Tone> = {
  'not-configured': 'neutral',
  idle: 'ok',
  'candidate-available': 'teal',
  'awaiting-approval': 'amber',
  applying: 'amber',
  'rolling-back': 'amber',
  'failed-held': 'danger',
  'failed-reverted': 'danger',
  completed: 'ok',
  'rolled-back': 'amber',
};

export function SettingsView(): ReactNode {
  const { t, lang, setLang } = useI18n();
  const { shortcuts, setShortcuts, fontSize, setFontSize } = useUi();
  const { actor } = useSession();
  const { workspaceId, workspace } = useWorkspace();
  const [system, setSystem] = useState<SystemInfo | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async (): Promise<void> => {
    setLoading(true);
    try {
      setSystem(await getSystem());
      setError(null);
    } catch (err) {
      setError(err as ApiError);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <div className="settings">
      <header className="settings__head">
        <h1>{t('settings.title')}</h1>
        <Button small icon="refresh" busy={loading} onClick={() => void load()}>
          {t('common.refresh')}
        </Button>
      </header>

      <section className="panel">
        <h2>{t('settings.general')}</h2>
        <div className="settings__grid">
          <label className="field">
            <span className="field__label">{t('settings.language')}</span>
            <select className="input" value={lang} onChange={(e) => setLang(e.target.value as Lang)}>
              <option value="zh-TW">繁體中文 (zh-TW)</option>
              <option value="en-US">English (en-US)</option>
            </select>
          </label>
          <label className="field">
            <span className="field__label">{t('settings.fontSize')}</span>
            <select className="input" aria-label={t('settings.fontSize')} aria-describedby="settings-font-size-note" value={fontSize} onChange={(e) => setFontSize(Number(e.target.value))}>
              {FONT_SIZES.map((size) => <option key={size} value={size}>{size} px</option>)}
            </select>
            <span id="settings-font-size-note" className="field__hint">{t('settings.fontSizeNote')}</span>
          </label>
        </div>
        <label className="check">
          <input type="checkbox" checked={shortcuts} onChange={(e) => setShortcuts(e.target.checked)} />
          <span>{t('settings.shortcuts')}</span>
        </label>
        <p className="panel__desc">{t('settings.shortcutsNote')}</p>
        <ShortcutTable />
      </section>

      <section className="panel">
        <h2>{t('settings.workspace')}</h2>
        <dl className="kv">
          <div className="kv__row">
            <dt>{t('top.workspace')}</dt>
            <dd className="mono">{workspaceId}</dd>
          </div>
          <div className="kv__row">
            <dt>{t('settings.account')}</dt>
            <dd className="mono">
              {actor?.id} ({actor ? t(`role.${actor.role}`) : '—'})
            </dd>
          </div>
          <div className="kv__row">
            <dt>{t('settings.browserVersion')}</dt>
            <dd className="mono">{workspace?.browserVersion ?? t('top.browserUnknown')}</dd>
          </div>
          <div className="kv__row">
            <dt>{t('settings.profile')}</dt>
            <dd>{t('settings.profileNote')}</dd>
          </div>
        </dl>
      </section>

      <AgentModePanel />

      {error ? (
        <Notice tone="danger" title={t('settings.systemFailed')}>
          {describeError(error, t)}
        </Notice>
      ) : null}

      {system?.storage ? <StorageSection storage={system.storage} /> : null}
      {system?.gateway ? (
        <section className="panel">
          <h2>{t('settings.gateway')}</h2>
          <dl className="kv">
            {Object.entries(system.gateway).map(([k, v]) => (
              <div className="kv__row" key={k}>
                <dt>{k}</dt>
                <dd className="mono">{typeof v === 'object' && v !== null ? JSON.stringify(v) : String(v)}</dd>
              </div>
            ))}
          </dl>
        </section>
      ) : null}
      {system?.release ? <ReleaseSection release={system.release} /> : null}
      {system?.update ? <UpdateSection update={system.update} /> : null}
      {system && !system.release && !system.update ? <Notice tone="neutral">{t('settings.noRelease')}</Notice> : null}

      <section className="panel">
        <h2>{t('settings.licenses')}</h2>
        <p className="panel__desc">{t('settings.licensesDesc')}</p>
        <ul className="plain">
          <li>
            <a href="/licenses/NOTICE.txt" target="_blank" rel="noreferrer">
              {t('settings.licenses.notice')}
            </a>
          </li>
          <li>
            <a href="/licenses/noVNC-LICENSE.txt" target="_blank" rel="noreferrer">
              noVNC (MPL-2.0)
            </a>
          </li>
          <li>
            <a href="/licenses/OFL-noto-sans-tc.txt" target="_blank" rel="noreferrer">
              Noto Sans TC / SC / JP / KR, Noto Color Emoji, IBM Plex (SIL OFL 1.1)
            </a>
          </li>
        </ul>
      </section>
    </div>
  );
}

function ShortcutTable(): ReactNode {
  const { t } = useI18n();
  const rows: { keys: string; labelKey: MessageKey }[] = [
    { keys: 'Alt+Shift+K', labelKey: 'shortcut.control' },
    { keys: 'Alt+Shift+C', labelKey: 'shortcut.capture' },
    { keys: 'Alt+Shift+F', labelKey: 'shortcut.fullscreen' },
    { keys: 'Alt+Shift+M', labelKey: 'shortcut.fit' },
    { keys: 'Alt+Shift+D', labelKey: 'shortcut.dock' },
  ];
  return (
    <table className="table table--tight">
      <tbody>
        {rows.map((r) => (
          <tr key={r.keys}>
            <td>
              <kbd>{r.keys}</kbd>
            </td>
            <td>{t(r.labelKey)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function StorageSection({ storage }: { storage: NonNullable<SystemInfo['storage']> }): ReactNode {
  const { t } = useI18n();
  const entries: { key: 'database' | 'private' | 'artifacts'; labelKey: MessageKey; usage: StorageUsage | undefined }[] = [
    { key: 'database', labelKey: 'settings.storage.database', usage: storage.database },
    { key: 'private', labelKey: 'settings.storage.private', usage: storage.private },
    { key: 'artifacts', labelKey: 'settings.storage.artifacts', usage: storage.artifacts },
  ];
  return (
    <section className="panel">
      <h2>{t('settings.storage')}</h2>
      <table className="table">
        <thead>
          <tr>
            <th>{t('settings.storage.area')}</th>
            <th>{t('settings.storage.bytes')}</th>
            <th>{t('settings.storage.files')}</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <tr key={e.key}>
              <td>{t(e.labelKey)}</td>
              <td className="mono">{e.usage ? `${e.usage.complete ? '' : '≥ '}${formatBytes(e.usage.bytes)}` : '—'}</td>
              <td className="mono">{e.usage ? e.usage.files : '—'}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {entries.some((e) => e.usage && !e.usage.complete) ? <p className="panel__desc">{t('settings.storage.incomplete')}</p> : null}
      <p className="panel__desc">{t('settings.storage.privateTtl')}</p>
    </section>
  );
}

function ReleaseSection({ release }: { release: ReleaseInfo }): ReactNode {
  const { t, locale } = useI18n();
  const v = release.versions;
  return (
    <section className="panel">
      <h2>{t('settings.release')}</h2>
      <div className="chips">
        <Chip tone={release.verified ? 'ok' : 'amber'} icon={release.verified ? 'check' : 'warning'}>
          {release.verified ? t('release.verified') : t('release.unverified')}
        </Chip>
        {release.signed !== undefined ? <Chip tone={release.signed ? 'ok' : 'neutral'}>{release.signed ? t('release.signed') : t('release.unsigned')}</Chip> : null}
        {release.channel ? <Chip>{release.channel}</Chip> : null}
      </div>
      {!release.verified ? <p className="panel__desc">{t('release.unverifiedNote')}</p> : null}
      <dl className="kv">
        <KV k={t('release.id')} v={release.id} />
        {release.createdAt ? <KV k={t('release.created')} v={formatDateTime(release.createdAt, locale)} /> : null}
        {release.verifiedAt ? <KV k={t('release.verifiedAt')} v={formatDateTime(release.verifiedAt, locale)} /> : null}
        {release.gitCommit ? <KV k="Commit" v={release.gitCommit} /> : null}
        {release.manifestSha256 ? <KV k="Manifest SHA-256" v={release.manifestSha256} /> : null}
        {release.signatureKeyId ? <KV k={t('release.keyId')} v={release.signatureKeyId} /> : null}
        {release.generation !== undefined ? <KV k={t('release.generation')} v={String(release.generation)} /> : null}
        {release.schema ? <KV k={t('release.schema')} v={`gatewayDb ${release.schema.gatewayDb} (min ${release.schema.minGatewayDb})`} /> : null}
        {v
          ? Object.entries(v).map(([k, val]) => <KV key={k} k={k} v={val ?? '—'} />)
          : null}
        {release.images?.runtime ? <KV k="runtime image" v={`${release.images.runtime.repository}@${release.images.runtime.digest}`} /> : null}
        {release.images?.gateway ? <KV k="gateway image" v={`${release.images.gateway.repository}@${release.images.gateway.digest}`} /> : null}
        {release.evidence ? (
          <KV
            k={t('release.evidence')}
            v={[
              `SBOM ${release.evidence.sbom ? '✓' : '✗'}`,
              `provenance ${release.evidence.provenance ? '✓' : '✗'}`,
              `tests ${release.evidence.tests ? '✓' : '✗'}`,
              release.evidence.scan
                ? `scan ${release.evidence.scan.tool}: C${release.evidence.scan.critical} H${release.evidence.scan.high} M${release.evidence.scan.medium} L${release.evidence.scan.low}${release.evidence.scan.partial ? ` (${t('release.scanPartial')})` : ''}`
                : 'scan —',
            ].join(' · ')}
          />
        ) : null}
      </dl>
    </section>
  );
}

function KV({ k, v }: { k: string; v: string }): ReactNode {
  return (
    <div className="kv__row">
      <dt>{k}</dt>
      <dd className="mono">{v}</dd>
    </div>
  );
}

function CopyCommand({ command, label }: { command: string; label: string }): ReactNode {
  const { t } = useI18n();
  const toast = useToast();
  return (
    <div className="cmd">
      <span className="cmd__label">{label}</span>
      <code className="cmd__text">{command}</code>
      <Button
        small
        icon="clipboard"
        onClick={() => {
          navigator.clipboard.writeText(command).then(
            () => toast.push('ok', t('common.copied')),
            () => toast.push('error', t('common.copyFailed')),
          );
        }}
      >
        {t('common.copy')}
      </Button>
    </div>
  );
}

function UpdateSection({ update }: { update: UpdateStatus }): ReactNode {
  const { t, locale } = useI18n();
  const backup = update.backup;
  const rb = update.rollback;
  const op = update.lastOperation;
  return (
    <section className="panel">
      <h2>{t('settings.updates')}</h2>
      <Notice tone="neutral">{t('update.hostOnly')}</Notice>
      <div className="chips">
        <Chip tone={PHASE_TONE[update.phase]}>{t(`update.phase.${update.phase}`)}</Chip>
      </div>
      {update.storage ? <UpdateStorageRows storage={update.storage} /> : null}
      <dl className="kv">
        <KV k={t('update.lastBackup')} v={update.lastBackupAt ? formatDateTime(update.lastBackupAt, locale) : t('update.never')} />
        <KV k={t('update.lastVerified')} v={update.lastVerifiedAt ? formatDateTime(update.lastVerifiedAt, locale) : '—'} />
        {backup ? (
          <KV
            k={t('update.backups')}
            v={`${backup.count}/${backup.retain} · ${formatBytes(backup.totalBytes)}${backup.freeBytes != null ? ` · ${t('update.free', { size: formatBytes(backup.freeBytes) })}` : ''}`}
          />
        ) : null}
        {backup?.lastCheckpoint ? (
          <KV
            k={t('update.checkpoint')}
            v={[
              backup.lastCheckpoint.id,
              backup.lastCheckpoint.consistency,
              formatDateTime(backup.lastCheckpoint.createdAt, locale),
              (backup.lastCheckpoint.datasets ?? []).map((d) => `${d.role} ${formatBytes(d.bytes)}`).join(', '),
            ].join(' · ')}
          />
        ) : null}
      </dl>

      {update.pendingPlan ? <PendingPlan plan={update.pendingPlan} /> : null}

      <h3>{t('update.candidates')}</h3>
      {(update.candidates ?? []).length === 0 ? <p className="empty">{t('update.noCandidates')}</p> : null}
      {(update.candidates ?? []).map((c) => (
        <Candidate key={c.id} c={c} />
      ))}

      <h3>{t('update.rollback')}</h3>
      {rb ? (
        <>
          <Chip tone={rb.available ? 'ok' : 'neutral'}>{rb.available ? t('update.rollback.available') : t('update.rollback.unavailable')}</Chip>
          {rb.reason ? <p className="panel__desc">{rb.reason}</p> : null}
          {rb.targetRelease ? <p className="mono">{t('update.rollback.target', { id: rb.targetRelease })}</p> : null}
          {rb.checkpointCreatedAt ? <p className="mono">{t('update.rollback.checkpoint', { when: formatDateTime(rb.checkpointCreatedAt, locale) })}</p> : null}
          {rb.dataLossNote ? <Notice tone="amber">{rb.dataLossNote}</Notice> : null}
          {rb.unaffected && rb.unaffected.length > 0 ? <p className="panel__desc">{t('update.rollback.unaffected', { list: rb.unaffected.join(', ') })}</p> : null}
          {rb.hostCommand ? <CopyCommand command={rb.hostCommand} label={t('update.rollback')} /> : null}
        </>
      ) : (
        <p className="empty">{t('update.rollback.none')}</p>
      )}

      {op ? (
        <>
          <h3>{t('update.lastOp')}</h3>
          <dl className="kv">
            <KV k={t('update.op.kind')} v={`${op.kind} ${op.id}`} />
            <KV k={t('update.op.status')} v={op.status} />
            <KV k={t('update.op.started')} v={formatDateTime(op.startedAt, locale)} />
            {op.finishedAt ? <KV k={t('update.op.finished')} v={formatDateTime(op.finishedAt, locale)} /> : null}
            {op.error ? <KV k={t('update.op.error')} v={op.error} /> : null}
          </dl>
        </>
      ) : null}
    </section>
  );
}

/** Host CLI storage totals. Unmeasured fields stay "unknown", never 0. */
function UpdateStorageRows({ storage }: { storage: UpdateStorage }): ReactNode {
  const { t } = useI18n();
  const rows: { key: keyof UpdateStorage; label: MessageKey }[] = [
    { key: 'profileBytes', label: 'update.storage.profile' },
    { key: 'keyringBytes', label: 'update.storage.keyring' },
    { key: 'downloadsBytes', label: 'update.storage.downloads' },
    { key: 'gatewayDbBytes', label: 'update.storage.db' },
    { key: 'artifactsBytes', label: 'update.storage.artifacts' },
    { key: 'privateBytes', label: 'update.storage.private' },
    { key: 'otherBytes', label: 'update.storage.other' },
    { key: 'totalBytes', label: 'update.storage.total' },
  ];
  return (
    <dl className="kv kv--tight">
      {rows.map((r) => {
        const v = storage[r.key];
        return <KV key={r.key} k={t(r.label)} v={v == null ? t('health.unknown') : formatBytes(v)} />;
      })}
      <p className="panel__desc">{t('update.storage.note')}</p>
    </dl>
  );
}

function PendingPlan({ plan }: { plan: NonNullable<UpdateStatus['pendingPlan']> }): ReactNode {
  const { t, locale } = useI18n();
  const blockers = plan.blockers ?? [];
  const warnings = plan.warnings ?? [];
  return (
    <Notice tone={blockers.length > 0 ? 'danger' : 'amber'} title={t('update.pending', { id: plan.release })}>
      {blockers.length > 0 ? (
        <ul>
          {blockers.map((b) => (
            <li key={b}>{b}</li>
          ))}
        </ul>
      ) : null}
      {warnings.length > 0 ? (
        <ul>
          {warnings.map((b) => (
            <li key={b}>{b}</li>
          ))}
        </ul>
      ) : null}
      <p className="mono">{t('update.planExpires', { when: formatDateTime(plan.expiresAt, locale) })}</p>
    </Notice>
  );
}

function Candidate({ c }: { c: UpdateCandidate }): ReactNode {
  const { t } = useI18n();
  const level = c.risk?.level;
  const tone: Tone = level === 'blocked' ? 'danger' : level === 'major' ? 'amber' : level ? 'teal' : 'neutral';
  const issues = c.issues ?? [];
  const reasons = c.risk?.reasons ?? [];
  return (
    <article className="candidate">
      <header>
        <strong className="mono">{c.id}</strong>
        {level ? <Chip tone={tone}>{t(`update.risk.${level}`)}</Chip> : null}
        {issues.length > 0 ? <Chip tone="danger" icon="warning">{t('update.notPlannable')}</Chip> : null}
      </header>
      {reasons.length > 0 ? (
        <ul>
          {reasons.map((r) => (
            <li key={r}>{r}</li>
          ))}
        </ul>
      ) : null}
      {c.diff && c.diff.length > 0 ? (
        <table className="table table--tight">
          <tbody>
            {c.diff.map((d) => (
              <tr key={d.component}>
                <td>{d.component}</td>
                <td className="mono">
                  {d.from} → {d.to}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
      {issues.length > 0 ? (
        <ul className="issues">
          {issues.map((r) => (
            <li key={r}>{r}</li>
          ))}
        </ul>
      ) : null}
      {c.hostCommands ? (
        <>
          <CopyCommand command={c.hostCommands.plan} label={t('update.cmd.plan')} />
          {c.hostCommands.apply ? <CopyCommand command={c.hostCommands.apply} label={t('update.cmd.apply')} /> : null}
        </>
      ) : null}
    </article>
  );
}
