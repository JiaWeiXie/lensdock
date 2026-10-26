import { useState, useSyncExternalStore, type ReactNode } from 'react';
import type { Annotation } from '@/api/types';
import { Button, Field } from '@/components/ui';
import { Icon, type IconName } from '@/components/icons';
import { useI18n, type MessageKey } from '@/i18n';
import { graphemes, codePointLength } from '@/util/text';
import { ColorMenu, ColorSwatches } from './ColorPalette';
import {
  ARROW_STYLES,
  EFFECT_SIZE_MAX,
  EFFECT_SIZE_MIN,
  HIGHLIGHT_OPACITY_MAX,
  HIGHLIGHT_OPACITY_MIN,
  INITIAL_DEFAULTS,
  MAX_FONT_SIZE,
  MAX_STROKE_WIDTH,
  MIN_FONT_SIZE,
  MIN_STROKE_WIDTH,
  TEXT_STYLES,
  capsOfKind,
  hasAnyCaps,
  mergeCaps,
  valuesOf,
  valuesOfDefaults,
  valuesOfMany,
  withStyle,
  type ArrowStyle,
  type StyleCaps,
  type StylePatch,
  type StyleValues,
} from './drawStyle';
import type { Alignment, ReorderDirection } from './selection';
import { useDropdown } from './useDropdown';

export type Tool =
  | 'select'
  | 'pan'
  | 'rectangle'
  | 'filledRect'
  | 'ellipse'
  | 'diamond'
  | 'line'
  | 'arrow'
  | 'freehand'
  | 'text'
  | 'pin'
  | 'highlight'
  | 'spotlight'
  | 'blur'
  | 'pixelate'
  | 'redact'
  | 'crop';

interface ToolDef {
  id: Tool;
  icon: IconName;
  /** Single-letter shortcut, matched by physical key (`Key<letter>`) so it works with any layout and never fires during IME composition. */
  key: string;
  label: MessageKey;
}

export const TOOLS: readonly ToolDef[] = [
  { id: 'select', icon: 'select', key: 'V', label: 'tool.select' },
  { id: 'pan', icon: 'pan', key: 'H', label: 'tool.pan' },
  { id: 'rectangle', icon: 'rect', key: 'R', label: 'tool.rectangle' },
  { id: 'filledRect', icon: 'filledRect', key: 'F', label: 'tool.filledRect' },
  { id: 'ellipse', icon: 'ellipse', key: 'O', label: 'tool.ellipse' },
  { id: 'diamond', icon: 'diamond', key: 'D', label: 'tool.diamond' },
  { id: 'line', icon: 'line', key: 'L', label: 'tool.line' },
  { id: 'arrow', icon: 'arrow', key: 'A', label: 'tool.arrow' },
  { id: 'freehand', icon: 'pen', key: 'P', label: 'tool.freehand' },
  { id: 'text', icon: 'text', key: 'T', label: 'tool.text' },
  { id: 'pin', icon: 'step', key: 'N', label: 'tool.pin' },
  { id: 'highlight', icon: 'highlight', key: 'M', label: 'tool.highlight' },
  { id: 'spotlight', icon: 'spotlight', key: 'S', label: 'tool.spotlight' },
  { id: 'blur', icon: 'blur', key: 'B', label: 'tool.blur' },
  { id: 'pixelate', icon: 'pixelate', key: 'I', label: 'tool.pixelate' },
  { id: 'redact', icon: 'redact', key: 'X', label: 'tool.redact' },
  { id: 'crop', icon: 'crop', key: 'C', label: 'tool.crop' },
];

/** The tool bound to a physical key, or null. Used by the editor's keyboard handler. */
export function toolForCode(code: string): Tool | null {
  return TOOLS.find((d) => `Key${d.key}` === code)?.id ?? null;
}

interface ToolGroup {
  id: string;
  label: MessageKey;
  tools: readonly Tool[];
  /** Collapses into a menu when the toolbar is narrow. */
  collapses: boolean;
}

const TOOL_GROUPS: readonly ToolGroup[] = [
  { id: 'select', label: 'editor.group.select', tools: ['select', 'pan'], collapses: false },
  { id: 'draw', label: 'editor.group.draw', tools: ['rectangle', 'filledRect', 'ellipse', 'diamond', 'line', 'arrow', 'freehand'], collapses: true },
  { id: 'mark', label: 'editor.group.mark', tools: ['text', 'pin'], collapses: true },
  { id: 'emphasis', label: 'editor.group.emphasis', tools: ['highlight', 'spotlight', 'blur', 'pixelate', 'redact'], collapses: true },
  { id: 'crop', label: 'editor.group.crop', tools: ['crop'], collapses: false },
];

const NARROW_QUERY = '(max-width: 900px)';

function subscribeNarrow(onChange: () => void): () => void {
  const mq = window.matchMedia(NARROW_QUERY);
  mq.addEventListener('change', onChange);
  return () => mq.removeEventListener('change', onChange);
}

function toolDef(id: Tool): ToolDef {
  const d = TOOLS.find((x) => x.id === id);
  if (!d) throw new Error(`unknown tool ${id}`);
  return d;
}

export function EditorToolbar({ tool, onTool }: { tool: Tool; onTool: (t: Tool) => void }): ReactNode {
  const { t } = useI18n();
  const narrow = useSyncExternalStore(subscribeNarrow, () => window.matchMedia(NARROW_QUERY).matches, () => false);

  const button = (d: ToolDef, after?: () => void): ReactNode => (
    <button
      key={d.id}
      type="button"
      className={`edtool ${tool === d.id ? 'is-on' : ''}`}
      aria-pressed={tool === d.id}
      aria-label={t(d.label)}
      title={`${t(d.label)} (${d.key})`}
      aria-keyshortcuts={d.key}
      onClick={() => {
        onTool(d.id);
        after?.();
      }}
    >
      <Icon name={d.icon} size={20} />
      <span className="edtool__label">{t(d.label)}</span>
      <kbd className="edtool__key" aria-hidden="true">{d.key}</kbd>
    </button>
  );

  return (
    <div className="edtools" role="toolbar" aria-label={t('editor.tools')}>
      {TOOL_GROUPS.map((g) =>
        narrow && g.collapses ? (
          <ToolGroupMenu key={g.id} group={g} tool={tool} renderTool={button} />
        ) : (
          <div key={g.id} className="edgroup" role="group" aria-label={t(g.label)}>
            {g.tools.map((id) => button(toolDef(id)))}
          </div>
        ),
      )}
    </div>
  );
}

/** One collapsed tool group: the trigger shows the active tool of the group (or the group name) and the menu closes on pick. */
function ToolGroupMenu({ group, tool, renderTool }: { group: ToolGroup; tool: Tool; renderTool: (d: ToolDef, after?: () => void) => ReactNode }): ReactNode {
  const { t } = useI18n();
  const menu = useDropdown();
  const active = group.tools.includes(tool) ? toolDef(tool) : null;
  const lead = active ?? toolDef(group.tools[0] ?? 'select');
  return (
    <div className="edgroup edmenu" ref={menu.rootRef}>
      <button type="button" className={`edtool edtool--menu ${active ? 'is-on' : ''}`} aria-expanded={menu.open} title={t(group.label)} onClick={menu.toggle}>
        <Icon name={lead.icon} size={20} />
        <span className="edtool__label edtool__label--shown">{active ? t(active.label) : t(group.label)}</span>
        <Icon name={menu.open ? 'chevronUp' : 'chevronDown'} size={12} />
      </button>
      {menu.open ? (
        <div className="edmenu__panel" role="group" aria-label={t(group.label)} ref={menu.panelRef} style={{ transform: `translateX(${menu.shift}px)` }}>
          {group.tools.map((id) => renderTool(toolDef(id), () => menu.setOpen(false)))}
        </div>
      ) : null}
    </div>
  );
}

export function annotationLabel(a: Annotation, t: (k: MessageKey, p?: Record<string, string | number>) => string): string {
  switch (a.kind) {
    case 'pin':
      return t('ann.pinN', { n: a.geometry.number });
    case 'text':
      return `${t('ann.kind.text')}: ${graphemes(a.geometry.text).slice(0, 24).join('')}`;
    default:
      return t(`ann.kind.${a.kind}`);
  }
}

// ----------------------------------------------------------- style controls

const ARROW_ICON: Record<ArrowStyle, IconName> = { filled: 'arrowFilled', open: 'arrowOpen', double: 'arrowDouble', curved: 'arrowCurved' };

/** Number box that lets the user type through out-of-range intermediate text (e.g. "1" on the way to "14") and settles on blur. */
function NumberField({ label, value, min, max, onCommit }: { label: string; value: number; min: number; max: number; onCommit: (n: number) => void }): ReactNode {
  const [text, setText] = useState<string | null>(null);
  return (
    <label className="stylebar__field editor__size">
      <span>{label}</span>
      <input
        type="number"
        inputMode="numeric"
        min={min}
        max={max}
        step={1}
        aria-label={label}
        value={text ?? String(Math.round(value))}
        onChange={(e) => {
          setText(e.target.value);
          const n = e.target.valueAsNumber;
          if (Number.isFinite(n) && n >= min && n <= max) onCommit(Math.round(n));
        }}
        onBlur={() => setText(null)}
      />
    </label>
  );
}

function RangeField({
  label,
  value,
  shown,
  min,
  max,
  step,
  onChange,
  layout,
}: {
  label: string;
  value: number;
  shown: string;
  min: number;
  max: number;
  step: number;
  onChange: (n: number) => void;
  layout: 'bar' | 'panel';
}): ReactNode {
  return (
    <label className={layout === 'bar' ? 'stylebar__field stylebar__range' : 'menu-form__row'}>
      <span>{label}</span>
      <input type="range" min={min} max={max} step={step} value={value} aria-valuetext={shown} onChange={(e) => onChange(Number(e.target.value))} />
      <output className="mono">{shown}</output>
    </label>
  );
}

/**
 * Every style control that applies to `caps`, for the toolbar (`bar`) or an inspector popover (`panel`).
 * Controls edit either the next object to draw or the selected ones; the caller decides which through `onPatch`.
 */
export function StyleControls({
  caps,
  values,
  onPatch,
  layout,
}: {
  caps: StyleCaps;
  values: StyleValues;
  onPatch: (patch: StylePatch, key: string) => void;
  layout: 'bar' | 'panel';
}): ReactNode {
  const { t } = useI18n();
  const bar = layout === 'bar';
  const arrowLabel = (s: ArrowStyle): string => t(`editor.arrowStyle.${s}`);

  return (
    <div className={bar ? 'stylebar' : 'stylebar stylebar--panel'} role="group" aria-label={t('editor.styleControls')}>
      {caps.color ? (
        bar ? (
          <ColorMenu label={t('editor.color')} value={values.color} onPick={(c) => c !== null && onPatch({ stroke: c }, 'color')} />
        ) : (
          <ColorSwatches legend={t('editor.color')} value={values.color} onPick={(c) => c !== null && onPatch({ stroke: c }, 'color')} />
        )
      ) : null}
      {caps.fill ? (
        bar ? (
          <ColorMenu label={t('editor.fill')} value={values.fill} allowNone onPick={(c) => onPatch({ fill: c }, 'fill')} />
        ) : (
          <ColorSwatches legend={t('editor.fill')} value={values.fill} allowNone onPick={(c) => onPatch({ fill: c }, 'fill')} />
        )
      ) : null}
      {caps.width ? (
        bar ? (
          <NumberField label={t('editor.strokeWidth')} value={values.width} min={MIN_STROKE_WIDTH} max={MAX_STROKE_WIDTH} onCommit={(n) => onPatch({ strokeWidthPx: n }, 'width')} />
        ) : (
          <RangeField layout={layout} label={t('editor.strokeWidth')} value={Math.round(values.width)} shown={String(Math.round(values.width))} min={MIN_STROKE_WIDTH} max={MAX_STROKE_WIDTH} step={1} onChange={(n) => onPatch({ strokeWidthPx: n }, 'width')} />
        )
      ) : null}
      {caps.font ? (
        bar ? (
          <NumberField label={t('editor.fontSize')} value={values.font} min={MIN_FONT_SIZE} max={MAX_FONT_SIZE} onCommit={(n) => onPatch({ fontSizePx: n }, 'font')} />
        ) : (
          <RangeField layout={layout} label={t('editor.fontSize')} value={Math.round(values.font)} shown={String(Math.round(values.font))} min={MIN_FONT_SIZE} max={MAX_FONT_SIZE} step={1} onChange={(n) => onPatch({ fontSizePx: n }, 'font')} />
        )
      ) : null}
      {caps.opacity ? (
        <RangeField
          layout={layout}
          label={t('editor.opacity')}
          value={Math.round(values.opacity * 100)}
          shown={`${Math.round(values.opacity * 100)}%`}
          min={Math.round(HIGHLIGHT_OPACITY_MIN * 100)}
          max={Math.round(HIGHLIGHT_OPACITY_MAX * 100)}
          step={5}
          onChange={(n) => onPatch({ opacity: n / 100 }, 'opacity')}
        />
      ) : null}
      {caps.effect ? (
        <RangeField layout={layout} label={t('editor.effectSize')} value={Math.round(values.effect)} shown={`${Math.round(values.effect)} px`} min={EFFECT_SIZE_MIN} max={EFFECT_SIZE_MAX} step={1} onChange={(n) => onPatch({ effectSizePx: n }, 'effect')} />
      ) : null}
      {caps.arrow ? (
        <div className="stylebar__field">
          {bar ? null : <span>{t('editor.arrowStyle.group')}</span>}
          <div className="stylebar__seg" role="radiogroup" aria-label={t('editor.arrowStyle.group')}>
            {ARROW_STYLES.map((s) => (
              <button key={s} type="button" role="radio" aria-checked={values.arrow === s} className={`stylebar__segbtn ${values.arrow === s ? 'is-on' : ''}`} title={arrowLabel(s)} aria-label={arrowLabel(s)} onClick={() => onPatch({ arrowStyle: s }, 'arrow')}>
                <Icon name={ARROW_ICON[s]} size={18} />
              </button>
            ))}
          </div>
        </div>
      ) : null}
      {caps.text ? (
        <label className="stylebar__field">
          <span>{t('editor.textStyle.title')}</span>
          <select className="input input--sm" value={values.text} onChange={(e) => {
            const next = TEXT_STYLES.find((s) => s === e.target.value);
            if (next) onPatch({ textStyle: next }, 'textstyle');
          }}>
            {TEXT_STYLES.map((s) => (
              <option key={s} value={s}>
                {t(`editor.textStyle.${s}`)}
              </option>
            ))}
          </select>
        </label>
      ) : null}
      {caps.smoothing ? (
        <button type="button" className={`stylebar__toggle ${values.smoothing ? 'is-on' : ''}`} aria-pressed={values.smoothing} title={t('editor.smoothing.hint')} onClick={() => onPatch({ smoothing: !values.smoothing }, 'smoothing')}>
          <Icon name="smooth" size={16} />
          <span>{t('editor.smoothing')}</span>
        </button>
      ) : null}
    </div>
  );
}

// -------------------------------------------------------------------- list

export function AnnotationList({
  annotations,
  selectedIds,
  onSelect,
  onDelete,
  onClear,
}: {
  annotations: readonly Annotation[];
  selectedIds: ReadonlySet<string>;
  /** `additive` toggles the row in the current selection instead of replacing it. */
  onSelect: (id: string, additive: boolean) => void;
  onDelete: (id: string) => void;
  onClear: () => void;
}): ReactNode {
  const { t } = useI18n();
  return (
    <div>
      <div className="listhead">
        <p className="panel__desc" aria-live="polite">
          {t('editor.count', { n: annotations.length })}
        </p>
        <Button small variant="ghost" icon="trash" disabled={annotations.length === 0} onClick={onClear}>
          {t('editor.clear.button')}
        </Button>
      </div>
      <ul className="rows">
        {annotations.map((a) => {
          const on = selectedIds.has(a.id);
          return (
            <li key={a.id} className={`row ${on ? 'row--selected' : ''}`}>
              <button type="button" className="row__open" aria-current={on} onClick={(e) => onSelect(a.id, e.shiftKey || e.ctrlKey || e.metaKey)}>
                <span className="row__main">
                  <strong>{annotationLabel(a, t)}</strong>
                  {a.comment ? <span className="row__meta">{graphemes(a.comment).slice(0, 40).join('')}</span> : null}
                </span>
              </button>
              <Button small variant="ghost" icon="trash" aria-label={t('editor.delete')} onClick={() => onDelete(a.id)} />
            </li>
          );
        })}
      </ul>
    </div>
  );
}

// --------------------------------------------------------------- selection

const LAYER_ACTIONS: readonly { dir: ReorderDirection; icon: IconName; label: MessageKey }[] = [
  { dir: 'front', icon: 'toFront', label: 'editor.layer.front' },
  { dir: 'forward', icon: 'forward', label: 'editor.layer.forward' },
  { dir: 'backward', icon: 'backward', label: 'editor.layer.backward' },
  { dir: 'back', icon: 'toBack', label: 'editor.layer.back' },
];

const ALIGN_ACTIONS: readonly { align: Alignment; icon: IconName; label: MessageKey }[] = [
  { align: 'left', icon: 'alignLeft', label: 'editor.align.left' },
  { align: 'center', icon: 'alignCenter', label: 'editor.align.center' },
  { align: 'right', icon: 'alignRight', label: 'editor.align.right' },
  { align: 'top', icon: 'alignTop', label: 'editor.align.top' },
  { align: 'middle', icon: 'alignMiddle', label: 'editor.align.middle' },
  { align: 'bottom', icon: 'alignBottom', label: 'editor.align.bottom' },
];

/** Duplicate, layer order, alignment and delete for the current selection (one or more objects). */
export function SelectionActions({
  count,
  onDuplicate,
  onLayer,
  onAlign,
  onDelete,
}: {
  count: number;
  onDuplicate: () => void;
  onLayer: (dir: ReorderDirection) => void;
  onAlign: (align: Alignment) => void;
  onDelete: () => void;
}): ReactNode {
  const { t } = useI18n();
  const canAlign = count >= 2;
  return (
    <div className="selactions">
      <p className="panel__desc" aria-live="polite">
        {t('editor.selection.count', { n: count })}
      </p>
      <div className="selactions__row">
        <Button small icon="copy" onClick={onDuplicate} aria-keyshortcuts="Control+D" title={`${t('editor.duplicate')} (Ctrl+D)`}>
          {t('editor.duplicate')}
        </Button>
        <Button small variant="danger" icon="trash" onClick={onDelete}>
          {t('editor.deleteSelected')}
        </Button>
      </div>
      <div className="iconrow" role="group" aria-label={t('editor.layer.group')}>
        {LAYER_ACTIONS.map((l) => (
          <Button key={l.dir} small icon={l.icon} aria-label={t(l.label)} title={t(l.label)} onClick={() => onLayer(l.dir)} />
        ))}
      </div>
      <p className="panel__desc">{t('editor.layer.redactionNote')}</p>
      <div className="iconrow" role="group" aria-label={t('editor.align.group')}>
        {ALIGN_ACTIONS.map((l) => (
          <Button
            key={l.align}
            small
            icon={l.icon}
            aria-label={t(l.label)}
            title={canAlign ? t(l.label) : undefined}
            disabledReason={canAlign ? null : t('editor.align.needTwo')}
            onClick={() => onAlign(l.align)}
          />
        ))}
      </div>
    </div>
  );
}

/** Style shared by several selected objects; each patch only touches objects it applies to. */
export function SelectionStyle({ items, onApply }: { items: readonly Annotation[]; onApply: (patch: StylePatch, coalesceKey: string) => void }): ReactNode {
  const { t } = useI18n();
  const caps = mergeCaps(items.map((a) => capsOfKind(a.kind)));
  if (!hasAnyCaps(caps)) return <p className="panel__desc">{t('editor.redactNote')}</p>;
  const values = valuesOfMany(items, valuesOfDefaults(INITIAL_DEFAULTS, null));
  return (
    <div className="inspector">
      <h4>{t('editor.selection.styleAll')}</h4>
      <StyleControls layout="panel" caps={caps} values={values} onPatch={(patch, key) => onApply(patch, `sel-${key}`)} />
    </div>
  );
}

// --------------------------------------------------------------- inspector

export function AnnotationInspector({
  a,
  onChange,
  onEditText,
  usedPins,
}: {
  a: Annotation;
  onChange: (next: Annotation, coalesceKey?: string) => void;
  onEditText: () => void;
  usedPins: readonly number[];
}): ReactNode {
  const { t } = useI18n();
  const free: number[] = [];
  if (a.kind === 'pin') {
    for (let n = 1; n <= 100; n += 1) if (n === a.geometry.number || !usedPins.includes(n)) free.push(n);
  }
  const caps = capsOfKind(a.kind);

  return (
    <div className="inspector">
      <h4>{annotationLabel(a, t)}</h4>

      {a.kind === 'redact' ? <p className="panel__desc">{t('editor.redactNote')}</p> : null}
      {a.kind === 'spotlight' ? <p className="panel__desc">{t('editor.spotlightNote')}</p> : null}
      {a.kind === 'blur' || a.kind === 'pixelate' ? <p className="panel__desc">{t('editor.effectWarning')}</p> : null}
      {a.kind === 'arrow' && a.style?.arrowStyle === 'curved' ? <p className="panel__desc">{t('editor.curveHint')}</p> : null}

      {hasAnyCaps(caps) ? <StyleControls layout="panel" caps={caps} values={valuesOf(a)} onPatch={(patch, key) => onChange(withStyle(a, patch), `${key}:${a.id}`)} /> : null}

      {a.kind === 'pin' ? (
        <label className="menu-form__row">
          <span>{t('editor.pinNumber')}</span>
          <select className="input input--sm" value={a.geometry.number} onChange={(e) => onChange({ ...a, geometry: { ...a.geometry, number: Number(e.target.value) } })}>
            {free.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
      ) : null}

      {a.kind === 'text' ? (
        <Button small icon="text" onClick={onEditText}>
          {t('editor.editText')}
        </Button>
      ) : null}

      <details className="editor__disclosure">
        <summary>{t('editor.optionalNote')}</summary>
        <Field
          label={a.kind === 'pin' ? t('editor.pinComment') : t('editor.comment')}
          htmlFor="ann-comment"
          hint={t('common.count', { n: codePointLength(a.comment ?? '') })}
        >
          <textarea
            id="ann-comment"
            className="input"
            rows={3}
            lang="zh-TW"
            value={a.comment ?? ''}
            onChange={(e) => onChange({ ...a, comment: e.target.value }, `comment:${a.id}`)}
          />
        </Field>
        {a.kind === 'pin' ? <p className="panel__desc">{t('editor.pinHint')}</p> : null}
      </details>
    </div>
  );
}
