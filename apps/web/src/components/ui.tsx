import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type KeyboardEvent,
  type ReactNode,
} from 'react';
import { useI18n } from '@/i18n';
import { Icon, type IconName } from './icons';

// ------------------------------------------------------------------ Button

type ButtonVariant = 'primary' | 'secondary' | 'danger' | 'warn' | 'ghost';

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  icon?: IconName;
  busy?: boolean;
  small?: boolean;
  /** Explains why the control is unavailable; shown as title and read by assistive tech. */
  disabledReason?: string | null | undefined;
}

export function Button({
  variant = 'secondary',
  icon,
  busy = false,
  small = false,
  disabledReason,
  className,
  children,
  disabled,
  type = 'button',
  title,
  ...rest
}: ButtonProps): ReactNode {
  const off = disabled === true || busy || Boolean(disabledReason);
  const cls = ['btn', `btn--${variant}`, small ? 'btn--sm' : '', busy ? 'is-busy' : '', className ?? ''].filter(Boolean).join(' ');
  return (
    <button
      {...rest}
      type={type}
      className={cls}
      disabled={off}
      aria-busy={busy || undefined}
      title={disabledReason ?? title}
    >
      {icon ? <Icon name={icon} size={small ? 15 : 17} /> : null}
      {children !== undefined && children !== null ? <span className="btn__label">{children}</span> : null}
      {disabledReason ? <span className="sr-only"> ({disabledReason})</span> : null}
    </button>
  );
}

// ------------------------------------------------------------------- Modal

interface ModalProps {
  open: boolean;
  onClose: () => void;
  title: string;
  children: ReactNode;
  footer?: ReactNode;
  wide?: boolean;
  /** When false the dialog ignores Esc and backdrop clicks (an operation is in flight). */
  dismissible?: boolean;
}

/** Native <dialog>: focus trap, Esc, and inert background come from the browser. */
export function Modal({ open, onClose, title, children, footer, wide = false, dismissible = true }: ModalProps): ReactNode {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  const { t } = useI18n();

  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) d.showModal();
    if (!open && d.open) d.close();
  }, [open]);

  return (
    <dialog
      ref={ref}
      className={`modal ${wide ? 'modal--wide' : ''}`}
      aria-labelledby={titleId}
      onCancel={(e) => {
        e.preventDefault();
        if (dismissible) onClose();
      }}
      onMouseDown={(e) => {
        if (dismissible && e.target === e.currentTarget) onClose();
      }}
    >
      {open ? (
        <div className="modal__inner">
          <header className="modal__head">
            <h2 id={titleId}>{title}</h2>
            <button type="button" className="icon-btn" onClick={onClose} disabled={!dismissible} aria-label={t('common.close')}>
              <Icon name="x" />
            </button>
          </header>
          <div className="modal__body">{children}</div>
          {footer ? <footer className="modal__foot">{footer}</footer> : null}
        </div>
      ) : null}
    </dialog>
  );
}

// ------------------------------------------------------------------- Toasts

type ToastKind = 'info' | 'ok' | 'warn' | 'error';

interface Toast {
  id: number;
  kind: ToastKind;
  text: string;
}

interface ToastApi {
  push: (kind: ToastKind, text: string) => void;
}

const ToastCtx = createContext<ToastApi | null>(null);

export function ToastProvider({ children }: { children: ReactNode }): ReactNode {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const seq = useRef(0);
  const { t } = useI18n();

  const dismiss = useCallback((id: number) => setToasts((all) => all.filter((x) => x.id !== id)), []);
  const push = useCallback(
    (kind: ToastKind, text: string) => {
      seq.current += 1;
      const id = seq.current;
      setToasts((all) => [...all.slice(-4), { id, kind, text }]);
      window.setTimeout(() => dismiss(id), kind === 'error' ? 12000 : 6000);
    },
    [dismiss],
  );
  const api = useMemo<ToastApi>(() => ({ push }), [push]);

  return (
    <ToastCtx.Provider value={api}>
      {children}
      <div className="toasts" aria-live="polite">
        {toasts.map((x) => (
          <div key={x.id} className={`toast toast--${x.kind}`} role={x.kind === 'error' ? 'alert' : 'status'}>
            <Icon name={x.kind === 'ok' ? 'check' : x.kind === 'info' ? 'activity' : 'warning'} />
            <span>{x.text}</span>
            <button type="button" className="icon-btn" onClick={() => dismiss(x.id)} aria-label={t('common.dismiss')}>
              <Icon name="x" size={14} />
            </button>
          </div>
        ))}
      </div>
    </ToastCtx.Provider>
  );
}

export function useToast(): ToastApi {
  const v = useContext(ToastCtx);
  if (!v) throw new Error('useToast outside ToastProvider');
  return v;
}

// -------------------------------------------------------------------- Chip

export type Tone = 'neutral' | 'teal' | 'amber' | 'danger' | 'ok' | 'agent';

export function Chip({ tone = 'neutral', icon, children }: { tone?: Tone; icon?: IconName; children: ReactNode }): ReactNode {
  return (
    <span className={`chip chip--${tone}`}>
      {icon ? <Icon name={icon} size={13} /> : null}
      {children}
    </span>
  );
}

// --------------------------------------------------------------------- Tabs

export interface TabItem<T extends string> {
  id: T;
  label: string;
  badge?: string | number | undefined;
}

export function tabPanelId(group: string, id: string): string {
  return `${group}-panel-${id}`;
}

export function Tabs<T extends string>({
  group,
  items,
  value,
  onChange,
  label,
}: {
  group: string;
  items: readonly TabItem<T>[];
  value: T;
  onChange: (id: T) => void;
  label: string;
}): ReactNode {
  const onKey = (e: KeyboardEvent<HTMLDivElement>): void => {
    const idx = items.findIndex((i) => i.id === value);
    let next = idx;
    if (e.key === 'ArrowRight') next = (idx + 1) % items.length;
    else if (e.key === 'ArrowLeft') next = (idx - 1 + items.length) % items.length;
    else if (e.key === 'Home') next = 0;
    else if (e.key === 'End') next = items.length - 1;
    else return;
    e.preventDefault();
    const target = items[next];
    if (target) {
      onChange(target.id);
      document.getElementById(`${group}-tab-${target.id}`)?.focus();
    }
  };
  return (
    <div role="tablist" aria-label={label} className="tabs" onKeyDown={onKey}>
      {items.map((i) => (
        <button
          key={i.id}
          type="button"
          role="tab"
          id={`${group}-tab-${i.id}`}
          aria-selected={i.id === value}
          aria-controls={tabPanelId(group, i.id)}
          tabIndex={i.id === value ? 0 : -1}
          className={`tab ${i.id === value ? 'is-active' : ''}`}
          onClick={() => onChange(i.id)}
        >
          {i.label}
          {i.badge !== undefined && i.badge !== 0 ? <span className="tab__badge">{i.badge}</span> : null}
        </button>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------- Form bits

export function Field({
  label,
  hint,
  error,
  children,
  htmlFor,
}: {
  label: string;
  hint?: string | undefined;
  error?: string | null | undefined;
  children: ReactNode;
  htmlFor?: string;
}): ReactNode {
  return (
    <div className={`field ${error ? 'field--error' : ''}`}>
      <label className="field__label" htmlFor={htmlFor}>
        {label}
      </label>
      {children}
      {hint ? <p className="field__hint">{hint}</p> : null}
      {error ? (
        <p className="field__error" role="alert">
          {error}
        </p>
      ) : null}
    </div>
  );
}

export function Meter({ value, max, label, tone = 'teal' }: { value: number; max: number; label: string; tone?: Tone }): ReactNode {
  const pct = max > 0 ? Math.min(100, Math.max(0, (value / max) * 100)) : 0;
  return (
    <div
      className={`meter meter--${pct > 90 ? 'danger' : pct > 75 ? 'amber' : tone}`}
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={max}
      aria-valuenow={value}
      aria-label={label}
    >
      <div className="meter__fill" style={{ width: `${pct}%` }} />
    </div>
  );
}

/** Notice banner: tone and glyph together, never color alone. */
export function Notice({
  tone,
  title,
  children,
  actions,
}: {
  tone: 'amber' | 'danger' | 'teal' | 'neutral' | 'ok';
  title?: string | undefined;
  children?: ReactNode;
  actions?: ReactNode;
}): ReactNode {
  const role = tone === 'danger' ? 'alert' : 'status';
  return (
    <div className={`notice notice--${tone}`} role={role}>
      <Icon name={tone === 'ok' || tone === 'teal' ? 'check' : tone === 'neutral' ? 'activity' : 'warning'} />
      <div className="notice__body">
        {title ? <strong>{title}</strong> : null}
        {children ? <div>{children}</div> : null}
      </div>
      {actions ? <div className="notice__actions">{actions}</div> : null}
    </div>
  );
}

/** Renders any JSON-ish object as labelled rows without guessing at its shape. */
export function KeyValueRows({ data }: { data: Record<string, unknown> }): ReactNode {
  const rows: { key: string; value: string }[] = [];
  const walk = (prefix: string, v: unknown, depth: number): void => {
    if (v === null || v === undefined) {
      rows.push({ key: prefix, value: '—' });
    } else if (typeof v === 'object' && !Array.isArray(v) && depth < 3) {
      for (const [k, inner] of Object.entries(v as Record<string, unknown>)) walk(prefix ? `${prefix}.${k}` : k, inner, depth + 1);
    } else if (Array.isArray(v)) {
      rows.push({ key: prefix, value: v.map((x) => (typeof x === 'object' ? JSON.stringify(x) : String(x))).join(', ') || '—' });
    } else if (typeof v === 'object') {
      rows.push({ key: prefix, value: JSON.stringify(v) });
    } else {
      rows.push({ key: prefix, value: String(v) });
    }
  };
  for (const [k, v] of Object.entries(data)) walk(k, v, 0);
  return (
    <dl className="kv">
      {rows.map((r) => (
        <div key={r.key} className="kv__row">
          <dt>{r.key}</dt>
          <dd className="mono">{r.value}</dd>
        </div>
      ))}
    </dl>
  );
}
