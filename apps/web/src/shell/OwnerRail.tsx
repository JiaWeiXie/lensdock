import type { ReactNode } from 'react';
import { useI18n } from '@/i18n';
import { useRfb } from '@/vnc/DesktopProvider';
import { shortId } from '@/util/format';
import { OwnerGlyph, useOwner } from './owner';

/**
 * The control-owner rail shows glyphs, words and lease facts rather than
 * colour alone. The compact mobile workspace uses the top-bar owner readout.
 */
export function OwnerRail(): ReactNode {
  const { t } = useI18n();
  const owner = useOwner();
  const rfb = useRfb();

  const inputLive = rfb.phase === 'connected' && rfb.mode === 'control';
  const streamKey = rfb.phase === 'connected' ? (rfb.mode === 'control' ? 'rail.stream.control' : 'rail.stream.view') : 'rail.stream.none';

  return (
    <aside className="rail" aria-label={t('rail.label')} data-state={owner.state} data-mine={owner.mine || undefined}>
      <div className="rail__mark" aria-hidden="true">
        <svg width="30" height="30" viewBox="0 0 32 32">
          <circle cx="16" cy="16" r="10" fill="none" stroke="currentColor" strokeWidth="3" />
          <rect x="13.5" y="13.5" width="5" height="5" fill="var(--amber-bright)" />
        </svg>
      </div>

      <div className="rail__owner">
        <OwnerGlyph state={owner.state} />
        <p className="rail__state" aria-live="polite">
          {t(owner.labelKey)}
        </p>
        {owner.holderId ? (
          <p className="rail__holder mono" title={owner.holderId}>
            {shortId(owner.holderId, 10)}
          </p>
        ) : null}
      </div>

      <dl className="rail__meta">
        <div>
          <dt>{t('rail.epoch')}</dt>
          <dd className="mono">{owner.epoch ?? '—'}</dd>
        </div>
        <div>
          <dt>{t('rail.ttl')}</dt>
          <dd className="mono">{owner.secondsLeft !== null ? `${owner.secondsLeft}s` : '—'}</dd>
        </div>
        <div>
          <dt>{t('rail.generation')}</dt>
          <dd className="mono" title={owner.generation ?? undefined}>
            {shortId(owner.generation, 6)}
          </dd>
        </div>
      </dl>

      <div className={`rail__stream ${inputLive ? 'is-live' : ''}`}>
        <span className="rail__dot" aria-hidden="true" />
        <span>{t(streamKey)}</span>
      </div>
    </aside>
  );
}

/** Compact owner readout shown over the desktop in fullscreen, where the rail is out of view. */
export function OwnerBadge(): ReactNode {
  const { t } = useI18n();
  const owner = useOwner();
  return (
    <div className="owner-badge" data-state={owner.state} role="status">
      <OwnerGlyph state={owner.state} size={22} />
      <span>{t(owner.labelKey)}</span>
      <span className="mono">
        {t('rail.epoch')} {owner.epoch ?? '—'}
      </span>
      {owner.secondsLeft !== null ? <span className="mono">{owner.secondsLeft}s</span> : null}
    </div>
  );
}
