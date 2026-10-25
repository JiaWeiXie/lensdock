/**
 * X11 keysyms used by the combo menu and the "release all keys" action.
 * Values come from the X11 keysymdef (XK_*). Keys are sent through the public
 * RFB.sendKey(); no text is ever replayed key by key (see the clipboard bridge).
 */

export const XK = {
  Shift_L: 0xffe1,
  Shift_R: 0xffe2,
  Control_L: 0xffe3,
  Control_R: 0xffe4,
  Alt_L: 0xffe9,
  Alt_R: 0xffea,
  Meta_L: 0xffe7,
  Meta_R: 0xffe8,
  Super_L: 0xffeb,
  Super_R: 0xffec,
  ISO_Level3_Shift: 0xfe03,
  Tab: 0xff09,
  Return: 0xff0d,
  Escape: 0xff1b,
  Delete: 0xffff,
  F4: 0xffc1,
  F5: 0xffc2,
  F11: 0xffc8,
  a: 0x61,
  c: 0x63,
  l: 0x6c,
  r: 0x72,
  t: 0x74,
  v: 0x76,
  w: 0x77,
  x: 0x78,
  z: 0x7a,
} as const;

export const MODIFIER_KEYSYMS: readonly number[] = [
  XK.Shift_L,
  XK.Shift_R,
  XK.Control_L,
  XK.Control_R,
  XK.Alt_L,
  XK.Alt_R,
  XK.Meta_L,
  XK.Meta_R,
  XK.Super_L,
  XK.Super_R,
  XK.ISO_Level3_Shift,
];

export interface KeyCombo {
  id: string;
  /** Message key for the label. */
  labelKey: 'combo.copy' | 'combo.cut' | 'combo.paste' | 'combo.selectAll' | 'combo.undo' | 'combo.focusAddress' | 'combo.newTab' | 'combo.closeTab' | 'combo.reload' | 'combo.reopenTab' | 'combo.altTab' | 'combo.escape' | 'combo.altF4' | 'combo.ctrlAltDel';
  label: string;
  keys: readonly number[];
}

export const KEY_COMBOS: readonly KeyCombo[] = [
  { id: 'copy', labelKey: 'combo.copy', label: 'Ctrl+C', keys: [XK.Control_L, XK.c] },
  { id: 'cut', labelKey: 'combo.cut', label: 'Ctrl+X', keys: [XK.Control_L, XK.x] },
  { id: 'paste', labelKey: 'combo.paste', label: 'Ctrl+V', keys: [XK.Control_L, XK.v] },
  { id: 'selectAll', labelKey: 'combo.selectAll', label: 'Ctrl+A', keys: [XK.Control_L, XK.a] },
  { id: 'undo', labelKey: 'combo.undo', label: 'Ctrl+Z', keys: [XK.Control_L, XK.z] },
  { id: 'focusAddress', labelKey: 'combo.focusAddress', label: 'Ctrl+L', keys: [XK.Control_L, XK.l] },
  { id: 'newTab', labelKey: 'combo.newTab', label: 'Ctrl+T', keys: [XK.Control_L, XK.t] },
  { id: 'closeTab', labelKey: 'combo.closeTab', label: 'Ctrl+W', keys: [XK.Control_L, XK.w] },
  { id: 'reopenTab', labelKey: 'combo.reopenTab', label: 'Ctrl+Shift+T', keys: [XK.Control_L, XK.Shift_L, XK.t] },
  { id: 'reload', labelKey: 'combo.reload', label: 'F5', keys: [XK.F5] },
  { id: 'altTab', labelKey: 'combo.altTab', label: 'Alt+Tab', keys: [XK.Alt_L, XK.Tab] },
  { id: 'escape', labelKey: 'combo.escape', label: 'Esc', keys: [XK.Escape] },
  { id: 'altF4', labelKey: 'combo.altF4', label: 'Alt+F4', keys: [XK.Alt_L, XK.F4] },
];
