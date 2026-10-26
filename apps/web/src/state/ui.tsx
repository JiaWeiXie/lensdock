import { createContext, useCallback, useContext, useEffect, useMemo, useState, useSyncExternalStore, type ReactNode } from 'react';

/**
 * UI-only state. Nothing credential-like lives here: the route carries IDs,
 * localStorage carries preferences (language, shortcut toggle, view defaults).
 */

export type Route =
  | { name: 'workspace' }
  | { name: 'editor'; feedbackId: string }
  | { name: 'settings' };

export type DockTab = 'capture' | 'feedback' | 'transfer';
export type DrawerTab = 'health' | 'events' | 'audit';

function parseHash(hash: string): Route {
  const parts = hash.replace(/^#\/?/, '').split('/').filter(Boolean);
  if (parts[0] === 'editor' && parts[1]) return { name: 'editor', feedbackId: decodeURIComponent(parts[1]) };
  if (parts[0] === 'settings') return { name: 'settings' };
  return { name: 'workspace' };
}

export function routeHref(route: Route): string {
  if (route.name === 'editor') return `#/editor/${encodeURIComponent(route.feedbackId)}`;
  if (route.name === 'settings') return '#/settings';
  return '#/';
}

function subscribeHash(cb: () => void): () => void {
  window.addEventListener('hashchange', cb);
  return () => window.removeEventListener('hashchange', cb);
}

const readHash = (): string => window.location.hash;

export function useRoute(): [Route, (route: Route) => void] {
  const hash = useSyncExternalStore(subscribeHash, readHash);
  const route = useMemo(() => parseHash(hash), [hash]);
  const navigate = useCallback((r: Route) => {
    window.location.hash = routeHref(r);
  }, []);
  return [route, navigate];
}

export interface SelectedFeedback {
  feedbackId: string;
  revision: number;
}

interface UiCtx {
  dockTab: DockTab;
  setDockTab: (tab: DockTab) => void;
  selected: SelectedFeedback | null;
  select: (s: SelectedFeedback | null) => void;
  drawerOpen: boolean;
  drawerTab: DrawerTab;
  setDrawer: (open: boolean, tab?: DrawerTab) => void;
  shortcuts: boolean;
  setShortcuts: (on: boolean) => void;
  fontSize: number;
  setFontSize: (size: number) => void;
  dockCollapsed: boolean;
  setDockCollapsed: (v: boolean) => void;
}

const Ctx = createContext<UiCtx | null>(null);

const SHORTCUTS_KEY = 'lensdock.shortcuts';
const FONT_SIZE_KEY = 'lensdock.fontSize';
export const FONT_SIZES = [12, 14, 16, 18, 20] as const;

function loadFontSize(): number {
  try {
    const value = Number(localStorage.getItem(FONT_SIZE_KEY));
    return FONT_SIZES.some((size) => size === value) ? value : 14;
  } catch {
    return 14;
  }
}

export function UiProvider({ children }: { children: ReactNode }): ReactNode {
  const [dockTab, setDockTabState] = useState<DockTab>('capture');
  const [selected, select] = useState<SelectedFeedback | null>(null);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [drawerTab, setDrawerTab] = useState<DrawerTab>('health');
  const [shortcuts, setShortcutsState] = useState<boolean>(() => localStorage.getItem(SHORTCUTS_KEY) !== 'off');
  const [dockCollapsed, setDockCollapsed] = useState(true);
  const [fontSize, setFontSizeState] = useState(loadFontSize);

  const setDockTab = useCallback((tab: DockTab) => {
    setDockTabState(tab);
    setDockCollapsed(false);
  }, []);

  const setFontSize = useCallback((size: number) => {
    if (FONT_SIZES.some((allowed) => allowed === size)) setFontSizeState(size);
  }, []);

  useEffect(() => {
    document.documentElement.style.fontSize = `${fontSize}px`;
    try {
      localStorage.setItem(FONT_SIZE_KEY, String(fontSize));
    } catch {
      // The preference remains usable for this session when storage is unavailable.
    }
  }, [fontSize]);

  useEffect(() => {
    localStorage.setItem(SHORTCUTS_KEY, shortcuts ? 'on' : 'off');
  }, [shortcuts]);

  const setDrawer = useCallback((open: boolean, tab?: DrawerTab) => {
    setDrawerOpen(open);
    if (tab) setDrawerTab(tab);
  }, []);

  const value = useMemo<UiCtx>(
    () => ({
      dockTab,
      setDockTab,
      selected,
      select,
      drawerOpen,
      drawerTab,
      setDrawer,
      shortcuts,
      setShortcuts: setShortcutsState,
      fontSize,
      setFontSize,
      dockCollapsed,
      setDockCollapsed,
    }),
    [dockTab, setDockTab, selected, drawerOpen, drawerTab, setDrawer, shortcuts, fontSize, setFontSize, dockCollapsed],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useUi(): UiCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error('useUi outside UiProvider');
  return v;
}
