import { useEffect } from 'react';

export interface ShortcutActions {
  toggleControl: () => void;
  captureDesktop: () => void;
  toggleFullscreen: () => void;
  toggleFit: () => void;
  toggleDock: () => void;
}

/** Shortcuts are Alt+Shift+<key>, matched by physical key so they do not depend on the keyboard layout. */
const MAP: Record<string, keyof ShortcutActions> = {
  KeyK: 'toggleControl',
  KeyC: 'captureDesktop',
  KeyF: 'toggleFullscreen',
  KeyM: 'toggleFit',
  KeyD: 'toggleDock',
};

/** True when the keystroke belongs to text entry or to the remote desktop and must be left alone. */
export function isForeignInput(e: KeyboardEvent): boolean {
  // IME composition: never intercept (keyCode 229 is the legacy composition marker).
  if (e.isComposing || e.keyCode === 229) return true;
  const el = e.target;
  if (!(el instanceof HTMLElement)) return false;
  if (el.isContentEditable) return true;
  const tag = el.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true;
  // The remote desktop canvas owns the keyboard while it is focused.
  return tag === 'CANVAS';
}

export function useShortcuts(enabled: boolean, actions: ShortcutActions): void {
  useEffect(() => {
    if (!enabled) return;
    const onKey = (e: KeyboardEvent): void => {
      if (e.defaultPrevented || !e.altKey || !e.shiftKey || e.ctrlKey || e.metaKey) return;
      if (isForeignInput(e)) return;
      const action = MAP[e.code];
      if (!action) return;
      e.preventDefault();
      actions[action]();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [enabled, actions]);
}
