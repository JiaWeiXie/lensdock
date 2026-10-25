import { useEffect, useLayoutEffect, useRef, useState, type RefObject } from 'react';

export interface Dropdown {
  open: boolean;
  setOpen: (open: boolean) => void;
  toggle: () => void;
  rootRef: RefObject<HTMLDivElement | null>;
  panelRef: RefObject<HTMLDivElement | null>;
  /** Horizontal correction in px that keeps the panel inside the viewport. */
  shift: number;
}

/**
 * Inline (non-portal) dropdown behaviour for toolbar menus: outside press and Escape close it, focus returns to the
 * trigger, and the panel is nudged sideways so it never leaves a 320px viewport. Unlike the shared Popover it can sit
 * inside another popover and can be closed by its own content (picking a tool).
 */
export function useDropdown(): Dropdown {
  const [open, setOpenState] = useState(false);
  const [shift, setShift] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);

  const setOpen = (next: boolean): void => {
    if (!next) setShift(0);
    setOpenState(next);
  };

  useLayoutEffect(() => {
    if (!open) return;
    const place = (): void => {
      const panel = panelRef.current;
      const root = rootRef.current;
      if (!panel || !root) return;
      const width = panel.offsetWidth;
      const vw = document.documentElement.clientWidth;
      const left = root.getBoundingClientRect().left;
      setShift(Math.max(8 - left, Math.min(0, vw - 8 - (left + width))));
    };
    place();
    window.addEventListener('resize', place);
    return () => window.removeEventListener('resize', place);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: PointerEvent): void => {
      if (e.target instanceof Node && !rootRef.current?.contains(e.target)) setOpenState(false);
    };
    const onKey = (e: KeyboardEvent): void => {
      if (e.key !== 'Escape') return;
      // The editor's own Escape (clear selection, leave tool) must not also fire.
      e.stopPropagation();
      setOpenState(false);
      rootRef.current?.querySelector('button')?.focus();
    };
    document.addEventListener('pointerdown', onDown);
    document.addEventListener('keydown', onKey, true);
    return () => {
      document.removeEventListener('pointerdown', onDown);
      document.removeEventListener('keydown', onKey, true);
    };
  }, [open]);

  return { open, setOpen, toggle: () => setOpen(!open), rootRef, panelRef, shift };
}
