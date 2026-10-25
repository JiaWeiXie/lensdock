import { useId, type ReactNode } from 'react';
import { Icon } from '@/components/icons';
import { useI18n } from '@/i18n';
import { MAX_CUSTOM_COLORS, isBuiltInColor, normalizeHex, useCustomPalette } from './customPalette';
import { PALETTE } from './model';
import { useDropdown } from './useDropdown';

/** Browsers that ship the EyeDropper API (Chromium) can sample any pixel on screen; others simply do not get the button. */
interface EyeDropperLike {
  open: () => Promise<{ sRGBHex: string }>;
}
type EyeDropperCtor = new () => EyeDropperLike;

function eyeDropper(): EyeDropperCtor | null {
  if (!('EyeDropper' in window)) return null;
  // Feature-detected above; lib.dom does not describe the constructor yet.
  const ctor: unknown = window.EyeDropper;
  return ctor ? (ctor as EyeDropperCtor) : null;
}

interface SwatchesProps {
  legend: string;
  /** Selected colour; `null` means "none" (only meaningful with allowNone). */
  value: string | null | undefined;
  onPick: (color: string | null) => void;
  allowNone?: boolean;
}

/**
 * Built-in swatches, the user's saved colours (removable) and a free colour input. The picked colour
 * applies immediately; "Save colour" additionally keeps it in the persistent palette for later use as
 * stroke or fill.
 */
export function ColorSwatches({ legend, value, onPick, allowNone = false }: SwatchesProps): ReactNode {
  const { t } = useI18n();
  const name = useId();
  const palette = useCustomPalette();
  const current = normalizeHex(value);
  const pickerValue = current ?? '#e5362b';
  const canSave = current !== null && palette.canAdd(current);
  const dropper = eyeDropper();

  const swatch = (color: string, removable: boolean): ReactNode => (
    <span key={color} className={`swatch ${removable ? 'swatch--custom' : ''}`}>
      <label title={color}>
        <input type="radio" name={name} checked={current === color} onChange={() => onPick(color)} />
        <span style={{ background: color }} />
        <span className="sr-only">{color}</span>
      </label>
      {removable ? (
        <button type="button" className="swatch__remove" aria-label={t('editor.palette.remove', { color })} title={t('editor.palette.remove', { color })} onClick={() => palette.remove(color)}>
          <Icon name="x" size={9} />
        </button>
      ) : null}
    </span>
  );

  return (
    <fieldset className="swatches">
      <legend>{legend}</legend>
      <div className="swatches__grid">
        {allowNone ? (
          <span className="swatch swatch--none">
            <label title={t('editor.fill.none')}>
              <input type="radio" name={name} checked={current === null} onChange={() => onPick(null)} />
              <span />
              <span className="sr-only">{t('editor.fill.none')}</span>
            </label>
          </span>
        ) : null}
        {PALETTE.map((c) => swatch(c, false))}
        {palette.colors.map((c) => swatch(c, true))}
      </div>
      <div className="swatches__custom">
        <label className="swatches__picker" title={t('editor.palette.custom')}>
          <input type="color" aria-label={t('editor.palette.custom')} value={pickerValue} onChange={(e) => onPick(e.target.value.toLowerCase())} />
        </label>
        {dropper ? (
          <button
            type="button"
            className="btn btn--secondary btn--sm"
            title={t('editor.palette.eyedropper')}
            onClick={() => {
              // Cancelling the browser's picker rejects; nothing to report.
              void new dropper().open().then((r) => {
                const hex = normalizeHex(r.sRGBHex);
                if (hex) onPick(hex);
              }, () => undefined);
            }}
          >
            <Icon name="eyedropper" size={15} />
            <span className="btn__label">{t('editor.palette.eyedropper')}</span>
          </button>
        ) : null}
        <button
          type="button"
          className="btn btn--secondary btn--sm"
          disabled={!canSave}
          title={current !== null && isBuiltInColor(current) ? t('editor.palette.builtIn') : palette.colors.length >= MAX_CUSTOM_COLORS ? t('editor.palette.full', { max: MAX_CUSTOM_COLORS }) : t('editor.palette.addHint')}
          onClick={() => current !== null && palette.add(current)}
        >
          <Icon name="plus" size={15} />
          <span className="btn__label">{t('editor.palette.add')}</span>
        </button>
      </div>
      <p className="swatches__count" aria-live="polite">
        {t('editor.palette.count', { n: palette.colors.length, max: MAX_CUSTOM_COLORS })}
      </p>
    </fieldset>
  );
}

/**
 * A toolbar colour button: shows the current colour and opens the swatch panel inline (no portal, so it can
 * live inside another popover without breaking outside-click handling). Escape or a click elsewhere closes it.
 */
export function ColorMenu({ label, ...props }: Omit<SwatchesProps, 'legend'> & { label: string }): ReactNode {
  const { t } = useI18n();
  const menu = useDropdown();
  const panelId = useId();
  const current = normalizeHex(props.value);

  return (
    <div className="colormenu" ref={menu.rootRef}>
      <button type="button" className="colormenu__btn" aria-expanded={menu.open} aria-controls={panelId} title={`${label}: ${current ?? t('editor.fill.none')}`} onClick={menu.toggle}>
        <span className={`colormenu__dot ${current === null ? 'colormenu__dot--none' : ''}`} style={current ? { background: current } : undefined} aria-hidden="true" />
        <span className="colormenu__label">{label}</span>
        <Icon name={menu.open ? 'chevronUp' : 'chevronDown'} size={12} />
      </button>
      {menu.open ? (
        <div className="colormenu__panel" id={panelId} ref={menu.panelRef} style={{ transform: `translateX(${menu.shift}px)` }}>
          <ColorSwatches {...props} legend={label} />
        </div>
      ) : null}
    </div>
  );
}
