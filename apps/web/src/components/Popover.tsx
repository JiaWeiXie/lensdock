import { useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { Icon, type IconName } from './icons';

/**
 * Disclosure popover for toolbar menus. The trigger is a real button with
 * aria-expanded; Escape and outside clicks close it and return focus.
 */
export function Popover({
  label,
  icon,
  children,
  align = 'start',
  disabledReason,
}: {
  label: string;
  icon?: IconName;
  children: ReactNode;
  align?: 'start' | 'end';
  disabledReason?: string | null | undefined;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const [position, setPosition] = useState<{ left: number; top: number; maxHeight: number } | null>(null);
  const panelId = useId();

  useLayoutEffect(() => {
    if (!open || !panel.current || !trigger.current) return;
    const place = (): void => {
      const menu = panel.current;
      const button = trigger.current;
      if (!menu || !button) return;
      const anchor = button.getBoundingClientRect();
      const width = menu.getBoundingClientRect().width;
      const viewportWidth = document.documentElement.clientWidth;
      const viewportHeight = window.innerHeight;
      const below = Math.max(0, viewportHeight - anchor.bottom - 12);
      const above = Math.max(0, anchor.top - 12);
      const downward = below >= Math.min(menu.scrollHeight, 320) || below >= above;
      const maxHeight = downward ? below : above;
      const left = Math.max(8, Math.min(align === 'end' ? anchor.right - width : anchor.left, viewportWidth - width - 8));
      const top = downward ? anchor.bottom + 4 : Math.max(8, anchor.top - 4 - Math.min(menu.scrollHeight + 2, maxHeight));
      setPosition((previous) => previous?.left === left && previous.top === top && previous.maxHeight === maxHeight ? previous : { left, top, maxHeight });
    };
    place();
    const observer = new ResizeObserver(place);
    observer.observe(panel.current);
    observer.observe(trigger.current);
    window.addEventListener('resize', place);
    window.addEventListener('scroll', place, true);
    return () => {
      observer.disconnect();
      window.removeEventListener('resize', place);
      window.removeEventListener('scroll', place, true);
    };
  }, [open, align]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent): void => {
      const target = e.target as Node;
      if (!root.current?.contains(target) && !panel.current?.contains(target)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') {
        setOpen(false);
        trigger.current?.focus();
      }
    };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', onDown);
      document.removeEventListener('keydown', onKey);
    };
  }, [open]);

  return (
    <div className="popover" ref={root}>
      <button
        ref={trigger}
        type="button"
        className="btn btn--secondary btn--sm"
        aria-expanded={open}
        aria-controls={panelId}
        disabled={Boolean(disabledReason)}
        title={disabledReason ?? undefined}
        onClick={() => {
          setPosition(null);
          setOpen((value) => !value);
        }}
      >
        {icon ? <Icon name={icon} size={15} /> : null}
        <span className="btn__label">{label}</span>
        <Icon name={open ? 'chevronUp' : 'chevronDown'} size={13} />
      </button>
      {open ? createPortal(
        <div
          ref={panel}
          id={panelId}
          className="popover__panel"
          style={position ? { left: position.left, top: position.top, maxHeight: position.maxHeight } : { visibility: 'hidden' }}
        >
          {children}
        </div>,
        document.body,
      ) : null}
    </div>
  );
}
