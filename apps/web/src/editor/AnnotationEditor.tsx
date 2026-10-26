import { useCallback, useEffect, useMemo, useRef, useState, type PointerEvent as ReactPointerEvent, type ReactNode } from 'react';
import { cropCapture } from '@/api/capture';
import { createDraft, deleteDraft } from '@/api/feedback';
import type { Annotation, NormRect, Point, PublishedFeedback } from '@/api/types';
import { Button, Chip, Modal, Notice, useToast } from '@/components/ui';
import { Popover } from '@/components/Popover';
import { describeError } from '@/errors';
import { TaskForm } from '@/feedback/TaskForm';
import { sanitizeTaskForSave, validateTask } from '@/feedback/task';
import { useI18n } from '@/i18n';
import { isForeignInput } from '@/shell/useShortcuts';
import { useControl } from '@/state/control';
import { useRoute, useUi } from '@/state/ui';
import { useWorkspace } from '@/state/workspace';
import { newKey } from '@/util/async';
import { formatDateTime, formatTimeOnly, shortId } from '@/util/format';
import { AnnotationLayer, handleMode } from './AnnotationLayer';
import {
  AnnotationInspector,
  AnnotationList,
  EditorToolbar,
  SelectionActions,
  SelectionStyle,
  StyleControls,
  toolForCode,
  type Tool,
} from './EditorPanels';
import {
  HANDLES,
  arrowControlPoint,
  defaultArrowControl,
  fontSizeOf,
  handlePosition,
  hitTest,
  newAnnotationId,
  nextPinNumber,
  pointFromClient,
  rectFromPoints,
  resizeRect,
  snapPoint,
  redactionsLast,
  simplifyPath,
  snapRect,
  strokeOf,
  toSourcePixel,
  toWire,
  validateAnnotations,
  cropToPixels,
  type Handle,
  type ImageSize,
} from './model';
import {
  INITIAL_DEFAULTS,
  applyPatchToDefaults,
  arrowAnnotation,
  capsOfKind,
  capsOfTool,
  hasAnyCaps,
  isCosmeticEffect,
  isOverlayKind,
  isRectGeometry,
  isRectTool,
  kindOfTool,
  newStyle,
  rectAnnotation,
  valuesOf,
  valuesOfDefaults,
  withStyle,
  type DrawDefaults,
  type StylePatch,
} from './drawStyle';
import { ExportError, FontNotReadyError, renderAnnotatedPng } from './raster';
import { alignSelection, duplicateSelection, marqueeSelection, moveSelection, reorderSelection, resizeSelection, selectionBounds, type Alignment, type ReorderDirection } from './selection';
import { measureTextWidth, textFontFamily, textFontWeight } from './shapes';
import { useDraftEditor, type DraftEditor } from './useDraftEditor';
import { useTextFonts } from './useTextFonts';
import { AgentControlButton, AgentStateBanner } from '@/agent/AgentControlButton';
import { useAgentControl } from '@/agent/AgentControlProvider';
import { useSendFlow } from './useSendFlow';

const MIN_ZOOM = 0.05;
const MAX_ZOOM = 8;
const PICK_PX = 8;
const HANDLE_PX = 10;

/** Arrow and line share two draggable endpoints. */
type LinearAnnotation = Extract<Annotation, { kind: 'arrow' | 'line' }>;
type ArrowAnnotation = Extract<Annotation, { kind: 'arrow' }>;

function samePoint(a: Point, b: Point): boolean {
  return Math.abs(a.x - b.x) < 1e-6 && Math.abs(a.y - b.y) < 1e-6;
}

/**
 * Moves the endpoints of an arrow or line. A bend point the user never dragged (still the default for the old endpoints)
 * follows the new endpoints; a dragged one is kept exactly where it is.
 */
function withEndpoints(o: LinearAnnotation, start: Point, end: Point): Annotation {
  if (o.kind === 'arrow' && o.geometry.control) {
    const wasDefault = samePoint(o.geometry.control, defaultArrowControl(o.geometry.start, o.geometry.end));
    return { ...o, geometry: { start, end, control: wasDefault ? defaultArrowControl(start, end) : o.geometry.control } };
  }
  return { ...o, geometry: { start, end } };
}

type Gesture =
  | { kind: 'pan'; sx: number; sy: number; px: number; py: number }
  | { kind: 'draw'; tool: Tool; start: Point; points: Point[] }
  | { kind: 'marquee'; start: Point; additive: boolean; base: readonly string[] }
  /** `collapseTo`: a plain click on a member of a multi-selection narrows to it unless the pointer dragged. */
  | { kind: 'move'; origin: Point; ids: ReadonlySet<string>; moved: boolean; collapseTo: string | null }
  | { kind: 'resize'; handle: Handle; before: NormRect; ids: ReadonlySet<string> }
  | { kind: 'endpoint'; which: 'start' | 'end'; orig: LinearAnnotation }
  /** Dragging the bend handle of a curved arrow. */
  | { kind: 'curve'; orig: ArrowAnnotation }
  | { kind: 'crop-resize'; handle: Handle };

function exportFilename(kind: 'drawing' | 'annotated'): string {
  const d = new Date();
  const p = (n: number): string => String(n).padStart(2, '0');
  return `lensdock-${kind}-${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}.png`;
}

function downloadBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Revocation is deferred: some browsers abort the download if the URL dies before they read the blob.
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
}

interface TextEdit {
  id: string | null;
  position: Point;
  text: string;
}

/** Static annotation editor over an immutable capture. It never talks to the remote desktop. */
export function AnnotationEditorView({ feedbackId }: { feedbackId: string }): ReactNode {
  const { t, locale } = useI18n();
  const agent = useAgentControl();
  const toast = useToast();
  const editor = useDraftEditor(feedbackId);
  const [, navigate] = useRoute();
  const { select, setDockTab } = useUi();
  const { workspaceId } = useWorkspace();
  const { lease, release, busy: releaseBusy } = useControl();

  const [tool, setTool] = useState<Tool>('arrow');
  const [defaults, setDefaults] = useState<DrawDefaults>(INITIAL_DEFAULTS);
  const [selectedIds, setSelectedIds] = useState<readonly string[]>([]);
  const [zoom, setZoom] = useState(1);
  const [pan, setPan] = useState({ x: 0, y: 0 });
  const [fit, setFit] = useState(true);
  const [viewport, setViewport] = useState({ w: 0, h: 0 });
  const [working, setWorkingState] = useState<Annotation[] | null>(null);
  const [draftShape, setDraftShapeState] = useState<Annotation | null>(null);
  const [crop, setCropState] = useState<NormRect | null>(null);
  const [marquee, setMarquee] = useState<NormRect | null>(null);
  // Gesture handlers run between renders, so the live values are mirrored in refs that are written
  // synchronously together with the state used for painting.
  const workingRef = useRef<Annotation[] | null>(null);
  const draftRef = useRef<Annotation | null>(null);
  const cropRef = useRef<NormRect | null>(null);
  const setWorking = (v: Annotation[] | null): void => {
    workingRef.current = v;
    setWorkingState(v);
  };
  const setDraftShape = (v: Annotation | null): void => {
    draftRef.current = v;
    setDraftShapeState(v);
  };
  const setCrop = (v: NormRect | null): void => {
    cropRef.current = v;
    setCropState(v);
  };
  const [editing, setEditing] = useState<TextEdit | null>(null);
  const [notesOpen, setNotesOpen] = useState(false);
  const [leaveOpen, setLeaveOpen] = useState(false);
  const [cropOpen, setCropOpen] = useState(false);
  const [cropBusy, setCropBusy] = useState(false);
  const [cropError, setCropError] = useState<string | null>(null);
  const [deleteOld, setDeleteOld] = useState(false);
  const [spaceDown, setSpaceDown] = useState(false);
  const [cursorPx, setCursorPx] = useState<{ x: number; y: number } | null>(null);
  const [conflictBusy, setConflictBusy] = useState(false);
  const [exportBusy, setExportBusy] = useState<'copy' | 'download' | null>(null);
  const [clearOpen, setClearOpen] = useState(false);

  const finishSend = (published: PublishedFeedback): void => {
    select({ feedbackId: published.feedbackId, revision: published.revision });
    setDockTab('feedback');
    toast.push('ok', t('send.done'));
    navigate({ name: 'workspace' });
  };
  const flow = useSendFlow(editor, finishSend, () => {
    if (editor.task && validateTask(editor.task).length > 0) setNotesOpen(true);
  });

  const viewportRef = useRef<HTMLDivElement>(null);
  const stageRef = useRef<HTMLDivElement>(null);
  const gesture = useRef<Gesture | null>(null);

  const size: ImageSize | null = editor.size;
  const annotations = working ?? editor.annotations;
  const selectedSet = useMemo((): ReadonlySet<string> => {
    const live = new Set(annotations.map((a) => a.id));
    return new Set(selectedIds.filter((id) => live.has(id)));
  }, [annotations, selectedIds]);
  const selectedList = useMemo(() => annotations.filter((a) => selectedSet.has(a.id)), [annotations, selectedSet]);
  const selected = selectedList.length === 1 ? selectedList[0] : undefined;
  // Text widths depend on the bundled faces; the epoch changes when they finish loading so handle boxes settle on exact bounds.
  const fontEpoch = useTextFonts(annotations);
  const selBounds = useMemo(
    () => (size && selectedList.length > 0 ? selectionBounds(selectedList, size, measureTextWidth) : null),
    // eslint-disable-next-line react-hooks/exhaustive-deps -- fontEpoch only invalidates the memo
    [size, selectedList, fontEpoch],
  );
  const ready = editor.load.status === 'ready' && size !== null && editor.imageUrl !== null;

  // ---------------------------------------------------------------- viewport
  useEffect(() => {
    const el = viewportRef.current;
    if (!el) return;
    const ro = new ResizeObserver(() => setViewport({ w: el.clientWidth, h: el.clientHeight }));
    ro.observe(el);
    setViewport({ w: el.clientWidth, h: el.clientHeight });
    return () => ro.disconnect();
  }, [ready]);

  const fitZoom = useMemo((): number => {
    if (!size || viewport.w === 0 || viewport.h === 0) return 1;
    return Math.min(1, Math.max(MIN_ZOOM, Math.min((viewport.w - 24) / size.width, (viewport.h - 24) / size.height)));
  }, [size, viewport]);

  const centerPan = useCallback(
    (z: number): { x: number; y: number } => ({
      x: size ? (viewport.w - size.width * z) / 2 : 0,
      y: size ? (viewport.h - size.height * z) / 2 : 0,
    }),
    [size, viewport],
  );

  useEffect(() => {
    if (!fit || !size) return;
    setZoom(fitZoom);
    setPan(centerPan(fitZoom));
  }, [fit, fitZoom, centerPan, size]);

  const zoomTo = useCallback(
    (next: number, anchor?: { x: number; y: number }): void => {
      const z = Math.min(MAX_ZOOM, Math.max(MIN_ZOOM, next));
      const a = anchor ?? { x: viewport.w / 2, y: viewport.h / 2 };
      setFit(false);
      setPan((p) => ({ x: a.x - ((a.x - p.x) * z) / zoom, y: a.y - ((a.y - p.y) * z) / zoom }));
      setZoom(z);
    },
    [viewport, zoom],
  );

  useEffect(() => {
    const el = viewportRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent): void => {
      e.preventDefault();
      const rect = el.getBoundingClientRect();
      if (e.ctrlKey || e.metaKey) {
        zoomTo(zoom * Math.exp(-e.deltaY * 0.0015), { x: e.clientX - rect.left, y: e.clientY - rect.top });
      } else {
        setFit(false);
        setPan((p) => ({ x: p.x - (e.shiftKey ? e.deltaY : e.deltaX), y: p.y - (e.shiftKey ? 0 : e.deltaY) }));
      }
    };
    el.addEventListener('wheel', onWheel, { passive: false });
    return () => el.removeEventListener('wheel', onWheel);
  }, [zoomTo, zoom, ready]);

  // ----------------------------------------------------------------- editing
  const selectIds = useCallback((ids: readonly string[]): void => {
    setSelectedIds((prev) => (prev.length === ids.length && prev.every((id, i) => id === ids[i]) ? prev : [...ids]));
  }, []);

  /** One history entry per call; a result identical to the current list records nothing. */
  const commitList = useCallback(
    (next: Annotation[], coalesceKey?: string): void => {
      const cur = editor.annotations;
      if (next.length === cur.length && next.every((a, i) => Object.is(a, cur[i]))) return;
      editor.setAnnotations(next, coalesceKey);
    },
    [editor],
  );

  const addAnnotation = useCallback(
    (a: Annotation): void => {
      editor.setAnnotations([...editor.annotations, a]);
      setSelectedIds([a.id]);
    },
    [editor],
  );

  const updateAnnotation = useCallback(
    (next: Annotation, coalesceKey?: string): void => {
      editor.setAnnotations(
        editor.annotations.map((a) => (a.id === next.id ? next : a)),
        coalesceKey,
      );
    },
    [editor],
  );

  const deleteAnnotations = useCallback(
    (ids: ReadonlySet<string>): void => {
      editor.setAnnotations(editor.annotations.filter((a) => !ids.has(a.id)));
      setSelectedIds((prev) => prev.filter((id) => !ids.has(id)));
    },
    [editor],
  );

  const duplicateSelected = useCallback((): void => {
    if (!size || selectedSet.size === 0) return;
    const result = duplicateSelection(editor.annotations, selectedSet, size);
    editor.setAnnotations(result.annotations);
    setSelectedIds(result.selectedIds);
  }, [editor, size, selectedSet]);

  const layerSelected = useCallback(
    (dir: ReorderDirection): void => {
      if (selectedSet.size > 0) commitList(reorderSelection(editor.annotations, selectedSet, dir));
    },
    [editor, selectedSet, commitList],
  );

  const alignSelected = useCallback(
    (alignment: Alignment): void => {
      if (size && selectedSet.size >= 2) commitList(alignSelection(editor.annotations, selectedSet, alignment, size, measureTextWidth));
    },
    [editor, size, selectedSet, commitList],
  );

  const styleSelected = useCallback(
    (patch: StylePatch, coalesceKey: string): void => {
      commitList(
        editor.annotations.map((a) => (selectedSet.has(a.id) ? withStyle(a, patch) : a)),
        coalesceKey,
      );
    },
    [editor, selectedSet, commitList],
  );

  const startTextEdit = useCallback((id: string | null, position: Point, text: string): void => {
    setEditing({ id, position, text });
    if (id) setSelectedIds([id]);
  }, []);

  const commitText = useCallback((): void => {
    if (!editing) return;
    const text = editing.text;
    setEditing(null);
    if (text.trim() === '') {
      if (editing.id) deleteAnnotations(new Set([editing.id]));
      return;
    }
    if (editing.id) {
      const cur = editor.annotations.find((a) => a.id === editing.id);
      if (cur && cur.kind === 'text') updateAnnotation({ ...cur, geometry: { ...cur.geometry, text } });
    } else if (size) {
      addAnnotation({
        id: newAnnotationId(),
        kind: 'text',
        geometry: { position: snapPoint(editing.position), text },
        comment: '',
        style: newStyle('text', defaults),
      });
    }
  }, [editing, editor.annotations, size, addAnnotation, updateAnnotation, deleteAnnotations, defaults]);

  const stageRect = (): DOMRect | null => stageRef.current?.getBoundingClientRect() ?? null;

  const hitHandle = (r: NormRect, clientX: number, clientY: number, rect: DOMRect): Handle | null => {
    for (const h of HANDLES) {
      const pos = handlePosition(r, h);
      if (Math.hypot(rect.left + pos.x * rect.width - clientX, rect.top + pos.y * rect.height - clientY) <= HANDLE_PX) return h;
    }
    return null;
  };

  /** Topmost annotation under `p`, using the painted order (redactions always on top). */
  const pick = (p: Point, tolPx: number): string | null => (size ? hitTest(redactionsLast(annotations), p, size, tolPx, measureTextWidth) : null);

  const onPointerDown = (e: ReactPointerEvent<HTMLDivElement>): void => {
    if (!ready || !size || editing || flow.locked) return;
    if (e.button === 1 || tool === 'pan' || spaceDown) {
      gesture.current = { kind: 'pan', sx: e.clientX, sy: e.clientY, px: pan.x, py: pan.y };
      e.currentTarget.setPointerCapture(e.pointerId);
      e.preventDefault();
      return;
    }
    if (e.button !== 0) return;
    const rect = stageRect();
    if (!rect) return;
    const p = pointFromClient(e.clientX, e.clientY, rect);
    e.currentTarget.setPointerCapture(e.pointerId);
    const tol = PICK_PX / zoom;

    switch (tool) {
      case 'select': {
        const additive = e.shiftKey;
        const mode = handleMode(selectedList);
        if (mode === 'box' && selBounds) {
          const h = hitHandle(selBounds, e.clientX, e.clientY, rect);
          if (h) {
            gesture.current = { kind: 'resize', handle: h, before: selBounds, ids: selectedSet };
            return;
          }
        }
        if (mode === 'endpoints' && selected && (selected.kind === 'arrow' || selected.kind === 'line')) {
          // Start, end and (for a curved arrow) the bend handle: the nearest one within reach wins.
          const candidates: { gesture: Gesture; at: Point }[] = [
            { gesture: { kind: 'endpoint', which: 'start', orig: selected }, at: selected.geometry.start },
            { gesture: { kind: 'endpoint', which: 'end', orig: selected }, at: selected.geometry.end },
          ];
          const bend = selected.kind === 'arrow' ? arrowControlPoint(selected) : null;
          if (selected.kind === 'arrow' && bend) candidates.push({ gesture: { kind: 'curve', orig: selected }, at: bend });
          let best: { gesture: Gesture; dist: number } | null = null;
          for (const c of candidates) {
            const dist = Math.hypot(rect.left + c.at.x * rect.width - e.clientX, rect.top + c.at.y * rect.height - e.clientY);
            if (dist <= HANDLE_PX && (!best || dist < best.dist)) best = { gesture: c.gesture, dist };
          }
          if (best) {
            gesture.current = best.gesture;
            return;
          }
        }
        const id = pick(p, tol);
        if (!id) {
          // Empty space: start a marquee. Without Shift it replaces the selection, with Shift it adds to it.
          gesture.current = { kind: 'marquee', start: p, additive, base: additive ? [...selectedSet] : [] };
          if (!additive) selectIds([]);
          return;
        }
        if (additive) {
          const next = selectedSet.has(id) ? [...selectedSet].filter((x) => x !== id) : [...selectedSet, id];
          selectIds(next);
          if (!next.includes(id)) return;
          gesture.current = { kind: 'move', origin: p, ids: new Set(next), moved: false, collapseTo: null };
        } else if (selectedSet.has(id)) {
          gesture.current = { kind: 'move', origin: p, ids: selectedSet, moved: false, collapseTo: selectedSet.size > 1 ? id : null };
        } else {
          selectIds([id]);
          gesture.current = { kind: 'move', origin: p, ids: new Set([id]), moved: false, collapseTo: null };
        }
        return;
      }
      case 'rectangle':
      case 'filledRect':
      case 'ellipse':
      case 'diamond':
      case 'highlight':
      case 'spotlight':
      case 'blur':
      case 'pixelate':
      case 'line':
      case 'redact':
      case 'arrow':
      case 'freehand':
        gesture.current = { kind: 'draw', tool, start: p, points: [p] };
        return;
      case 'crop': {
        if (crop) {
          const h = hitHandle(crop, e.clientX, e.clientY, rect);
          if (h) {
            gesture.current = { kind: 'crop-resize', handle: h };
            return;
          }
        }
        gesture.current = { kind: 'draw', tool, start: p, points: [p] };
        return;
      }
      case 'text': {
        const id = pick(p, tol);
        const hit = id ? annotations.find((a) => a.id === id) : undefined;
        if (hit && hit.kind === 'text') startTextEdit(hit.id, hit.geometry.position, hit.geometry.text);
        else startTextEdit(null, p, '');
        gesture.current = null;
        e.preventDefault();
        return;
      }
      case 'pin': {
        const n = nextPinNumber(editor.annotations);
        addAnnotation({
          id: newAnnotationId(),
          kind: 'pin',
          geometry: { position: snapPoint(p), number: n },
          comment: '',
          style: newStyle('pin', defaults),
        });
        gesture.current = null;
        return;
      }
    }
  };

  const onPointerMove = (e: ReactPointerEvent<HTMLDivElement>): void => {
    const rect = stageRect();
    if (rect && size) {
      const p0 = pointFromClient(e.clientX, e.clientY, rect);
      const inside = e.clientX >= rect.left && e.clientX <= rect.right && e.clientY >= rect.top && e.clientY <= rect.bottom;
      setCursorPx(inside ? toSourcePixel(p0, size) : null);
    }
    const g = gesture.current;
    if (!g || !size) return;
    if (g.kind === 'pan') {
      setFit(false);
      setPan({ x: g.px + (e.clientX - g.sx), y: g.py + (e.clientY - g.sy) });
      return;
    }
    applyPointer(g, e.clientX, e.clientY);
  };

  /** Applies a pointer position to the active gesture; shared by every move and by the final pointerup. */
  const applyPointer = (g: Gesture, clientX: number, clientY: number): void => {
    const rect = stageRect();
    if (!size || !rect || g.kind === 'pan') return;
    const p = pointFromClient(clientX, clientY, rect);
    switch (g.kind) {
      case 'draw': {
        const tl = g.tool;
        if (isRectTool(tl) || tl === 'redact' || tl === 'crop') {
          const r = snapRect(rectFromPoints(g.start, p));
          if (tl === 'crop') setCrop(r);
          else if (tl === 'redact') setDraftShape({ id: 'draft', kind: 'redact', mode: 'solid', geometry: r, comment: '' });
          else setDraftShape(rectAnnotation(tl, 'draft', r, defaults));
        } else if (tl === 'arrow') {
          setDraftShape(arrowAnnotation('draft', g.start, p, defaults));
        } else if (tl === 'line') {
          setDraftShape({ id: 'draft', kind: 'line', geometry: { start: g.start, end: p }, comment: '', style: newStyle('line', defaults) });
        } else if (tl === 'freehand') {
          const last = g.points[g.points.length - 1];
          if (!last || Math.hypot((p.x - last.x) * size.width, (p.y - last.y) * size.height) >= Math.max(1.5, 1.5 / zoom)) {
            g.points.push(p);
            if (g.points.length >= 2) setDraftShape({ id: 'draft', kind: 'freehand', geometry: { points: [...g.points] }, comment: '', style: newStyle('freehand', defaults) });
          }
        }
        return;
      }
      case 'marquee': {
        const r = rectFromPoints(g.start, p);
        setMarquee(r);
        // A click that barely moved must not select whatever its zero-area rectangle touches.
        if (r.width * size.width * zoom < 3 && r.height * size.height * zoom < 3) return;
        const hit = marqueeSelection(editor.annotations, r, size, measureTextWidth);
        selectIds(g.additive ? [...new Set([...g.base, ...hit])] : hit);
        return;
      }
      case 'move': {
        const dx = p.x - g.origin.x;
        const dy = p.y - g.origin.y;
        if (!g.moved && Math.hypot(dx * size.width, dy * size.height) < 3 / zoom) return;
        g.moved = true;
        setWorking(moveSelection(editor.annotations, g.ids, dx, dy));
        return;
      }
      case 'resize': {
        const after = resizeRect(g.before, g.handle, p, size, 2);
        setWorking(resizeSelection(editor.annotations, g.ids, g.before, after, size));
        return;
      }
      case 'endpoint': {
        const o = g.orig;
        const start = g.which === 'start' ? snapPoint(p) : o.geometry.start;
        const end = g.which === 'end' ? snapPoint(p) : o.geometry.end;
        setWorking(editor.annotations.map((a) => (a.id === o.id ? withEndpoints(o, start, end) : a)));
        return;
      }
      case 'curve': {
        const o = g.orig;
        setWorking(editor.annotations.map((a) => (a.id === o.id ? { ...o, geometry: { ...o.geometry, control: snapPoint(p) } } : a)));
        return;
      }
      case 'crop-resize':
        if (cropRef.current) setCrop(resizeRect(cropRef.current, g.handle, p, size, 8));
        return;
    }
  };

  const finishGesture = (e: ReactPointerEvent<HTMLDivElement>, cancelled: boolean): void => {
    const g = gesture.current;
    gesture.current = null;
    if (e.currentTarget.hasPointerCapture(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId);
    setMarquee(null);
    if (!g || !size) {
      setDraftShape(null);
      return;
    }
    // The last pointer sample is applied here: React may not have rendered the previous move yet, and
    // pointerup carries the final position, so reading render state would drop the last segment.
    if (!cancelled) applyPointer(g, e.clientX, e.clientY);
    const shape = draftRef.current;
    setDraftShape(null);
    if (cancelled) {
      setWorking(null);
      if (g.kind === 'draw' && g.tool === 'crop') setCrop(null);
      return;
    }
    switch (g.kind) {
      case 'draw': {
        if (g.tool === 'crop') {
          if (cropRef.current && (cropRef.current.width * size.width < 8 || cropRef.current.height * size.height < 8)) setCrop(null);
          return;
        }
        if (!shape) return;
        if (isRectGeometry(shape)) {
          if (shape.geometry.width * size.width < 4 || shape.geometry.height * size.height < 4) return;
          addAnnotation({ ...shape, id: newAnnotationId() });
        } else if (shape.kind === 'arrow' || shape.kind === 'line') {
          const len = Math.hypot((shape.geometry.end.x - shape.geometry.start.x) * size.width, (shape.geometry.end.y - shape.geometry.start.y) * size.height);
          if (len < 6) return;
          const start = snapPoint(shape.geometry.start);
          const end = snapPoint(shape.geometry.end);
          if (shape.kind === 'arrow') {
            const control = shape.geometry.control;
            addAnnotation({ ...shape, id: newAnnotationId(), geometry: control ? { start, end, control: snapPoint(control) } : { start, end } });
          } else {
            addAnnotation({ ...shape, id: newAnnotationId(), geometry: { start, end } });
          }
        } else if (shape.kind === 'freehand') {
          let points = simplifyPath(shape.geometry.points, size, 0.75);
          for (let tol = 1.5; points.length > 10000 && tol < 64; tol *= 2) points = simplifyPath(points, size, tol);
          if (points.length < 2) return;
          addAnnotation({ ...shape, id: newAnnotationId(), geometry: { points: points.map(snapPoint) } });
        }
        return;
      }
      case 'move':
      case 'resize':
      case 'endpoint':
      case 'curve': {
        // One history entry per gesture: the live working copy is committed exactly once here.
        const next = workingRef.current;
        setWorking(null);
        if (next) editor.setAnnotations(next);
        else if (g.kind === 'move' && g.collapseTo) selectIds([g.collapseTo]);
        return;
      }
      default:
        return;
    }
  };

  // ---------------------------------------------------------------- keyboard
  const dialogOpen = cropOpen || leaveOpen || clearOpen || flow.locked || editor.saveState === 'conflict';
  useEffect(() => {
    const onKey = (e: KeyboardEvent): void => {
      // Never act while a dialog is open, during text/IME entry, or when another widget owns the key.
      if (editing || dialogOpen || isForeignInput(e)) return;
      const mod = e.ctrlKey || e.metaKey;
      const target = e.target instanceof HTMLElement ? e.target : null;
      if (mod && !e.altKey) {
        if (e.code === 'BracketRight' || e.code === 'BracketLeft') {
          e.preventDefault();
          const up = e.code === 'BracketRight';
          layerSelected(e.shiftKey ? (up ? 'front' : 'back') : up ? 'forward' : 'backward');
          return;
        }
        const k = e.key.toLowerCase();
        if (k === 'z') {
          e.preventDefault();
          if (e.shiftKey) editor.redo();
          else editor.undo();
        } else if (k === 'y') {
          e.preventDefault();
          editor.redo();
        } else if (k === 'a') {
          e.preventDefault();
          setTool('select');
          selectIds(editor.annotations.map((a) => a.id));
        } else if (k === 'd') {
          e.preventDefault();
          if (!e.repeat) duplicateSelected();
        }
        return;
      }
      if (e.altKey) return;
      if (e.code === 'Space' && target?.tagName !== 'BUTTON' && !e.repeat) {
        e.preventDefault();
        setSpaceDown(true);
        return;
      }
      switch (e.key) {
        case 'Delete':
        case 'Backspace':
          if (selectedSet.size > 0) {
            e.preventDefault();
            deleteAnnotations(selectedSet);
          }
          return;
        case 'Escape':
          gesture.current = null;
          setWorking(null);
          setDraftShape(null);
          setMarquee(null);
          if (crop) setCrop(null);
          else if (selectedSet.size > 0) selectIds([]);
          else setTool('select');
          return;
        case 'ArrowLeft':
        case 'ArrowRight':
        case 'ArrowUp':
        case 'ArrowDown': {
          if (selectedSet.size === 0 || !size) return;
          e.preventDefault();
          const step = e.shiftKey ? 10 : 1;
          const dx = e.key === 'ArrowLeft' ? -step / size.width : e.key === 'ArrowRight' ? step / size.width : 0;
          const dy = e.key === 'ArrowUp' ? -step / size.height : e.key === 'ArrowDown' ? step / size.height : 0;
          commitList(moveSelection(editor.annotations, selectedSet, dx, dy), `nudge:${[...selectedSet].join(',')}`);
          return;
        }
        case '+':
        case '=':
          zoomTo(zoom * 1.25);
          return;
        case '-':
          zoomTo(zoom / 1.25);
          return;
        case '0':
          setFit(true);
          return;
        case '1':
          zoomTo(1);
          return;
        default:
      }
      const next = toolForCode(e.code);
      if (next && !e.shiftKey) setTool(next);
    };
    const onKeyUp = (e: KeyboardEvent): void => {
      if (e.code === 'Space') setSpaceDown(false);
    };
    window.addEventListener('keydown', onKey);
    window.addEventListener('keyup', onKeyUp);
    return () => {
      window.removeEventListener('keydown', onKey);
      window.removeEventListener('keyup', onKeyUp);
    };
  }, [editing, dialogOpen, editor, selectedSet, size, crop, zoom, zoomTo, selectIds, deleteAnnotations, duplicateSelected, layerSelected, commitList]);

  // ------------------------------------------------------------------ export
  const describeExportError = (err: unknown, fallback: 'editor.export.failed' | 'editor.export.copyFailed'): string => {
    if (err instanceof FontNotReadyError) return t('publish.err.fonts');
    if (err instanceof ExportError) return t('publish.err.encode');
    return t(fallback);
  };

  /**
   * Flat PNG of the base image plus annotations at native size. Annotations are wire-normalized and
   * redactions are painted last; fonts are awaited by the rasterizer. Nothing editable is embedded.
   */
  const flattenedPng = async (): Promise<Blob> => {
    if (!editor.bitmap || !size) throw new Error(t('publish.err.notLoaded'));
    const raster = await renderAnnotatedPng(editor.bitmap, size, redactionsLast(toWire(editor.annotations)));
    return raster.blob;
  };

  const copyImage = async (): Promise<void> => {
    if (typeof ClipboardItem === 'undefined' || typeof navigator.clipboard?.write !== 'function') {
      toast.push('error', t('editor.export.copyUnsupported'));
      return;
    }
    setExportBusy('copy');
    try {
      // The blob is passed as a promise so the clipboard write stays inside the click's user activation.
      await navigator.clipboard.write([new ClipboardItem({ 'image/png': flattenedPng() })]);
      toast.push('ok', t('editor.export.copied'));
    } catch (err) {
      toast.push('error', describeExportError(err, 'editor.export.copyFailed'));
    } finally {
      setExportBusy(null);
    }
  };

  const downloadImage = async (): Promise<void> => {
    setExportBusy('download');
    try {
      downloadBlob(await flattenedPng(), exportFilename(editor.capture?.source === 'drawing' ? 'drawing' : 'annotated'));
      toast.push('ok', t('editor.export.downloaded'));
    } catch (err) {
      toast.push('error', describeExportError(err, 'editor.export.failed'));
    } finally {
      setExportBusy(null);
    }
  };

  const clearAll = (): void => {
    setClearOpen(false);
    setEditing(null);
    editor.setAnnotations([]);
    setSelectedIds([]);
    toast.push('ok', t('editor.clear.done'));
  };

  // ------------------------------------------------------------------ actions
  const goBack = async (): Promise<void> => {
    if (editor.saveState === 'saved') {
      navigate({ name: 'workspace' });
      return;
    }
    const ok = await editor.flush();
    if (ok) navigate({ name: 'workspace' });
    else setLeaveOpen(true);
  };

  const applyCrop = async (): Promise<void> => {
    if (!crop || !size || !editor.capture || !editor.task || !editor.draft) return;
    setCropBusy(true);
    setCropError(null);
    try {
      const saved = await editor.flush();
      if (!saved) throw new Error(t('publish.err.unsaved'));
      const px = cropToPixels(crop, size);
      const cropped = await cropCapture(editor.capture.captureId, px, newKey());
      const draft = await createDraft(workspaceId, cropped.captureId, sanitizeTaskForSave(editor.task), newKey());
      if (deleteOld) await deleteDraft(feedbackId);
      toast.push('ok', t('crop.done'));
      setCropOpen(false);
      setCrop(null);
      navigate({ name: 'editor', feedbackId: draft.feedbackId });
    } catch (err) {
      setCropError(describeError(err, t));
    } finally {
      setCropBusy(false);
    }
  };

  // ------------------------------------------------------------------- render
  if (editor.load.status === 'loading') {
    return (
      <main className="editor editor--state" id="main">
        <p role="status">{t('editor.loading')}</p>
      </main>
    );
  }
  if (editor.load.status === 'error') {
    return (
      <main className="editor editor--state" id="main">
        <Notice tone="danger" title={t('editor.loadFailed')} actions={<Button onClick={() => navigate({ name: 'workspace' })}>{t('top.backToWorkspace')}</Button>}>
          {editor.load.message}
        </Notice>
      </main>
    );
  }

  const capture = editor.capture;
  const isDrawing = capture?.source === 'drawing';
  const taskIssues = editor.task ? validateTask(editor.task) : [];
  const annotationIssues = validateAnnotations(editor.annotations);
  const expiresSoon = editor.draft?.expiresAt ? Date.parse(editor.draft.expiresAt) - Date.now() < 6 * 3600 * 1000 : false;
  const editingAnn = editing?.id ? editor.annotations.find((a) => a.id === editing.id) : undefined;
  const editFont = editingAnn ? fontSizeOf(editingAnn) : defaults.font;
  const editColor = editingAnn ? strokeOf(editingAnn) : defaults.color;
  const editTextStyle = editingAnn?.kind === 'text' ? editingAnn.style?.textStyle : defaults.text === 'outline' ? undefined : defaults.text;
  const usedPins = editor.annotations.flatMap((a) => (a.kind === 'pin' ? [a.geometry.number] : []));
  const pins = editor.annotations.flatMap((a) => (a.kind === 'pin' ? [{ number: a.geometry.number, comment: a.comment ?? '' }] : [])).sort((a, b) => a.number - b.number);
  // Controls follow the single selected object, otherwise the active drawing tool; several objects use the Style menu.
  const barKind = selected ? selected.kind : kindOfTool(tool);
  const barCaps = selectedList.length > 1 ? null : selected ? capsOfKind(selected.kind) : capsOfTool(tool);
  const barValues = selected ? valuesOf(selected) : valuesOfDefaults(defaults, barKind);
  const patchBar = (patch: StylePatch, key: string): void => {
    setDefaults((d) => applyPatchToDefaults(d, patch, barKind));
    if (selected) styleSelected(patch, `quick-${key}:${selected.id}`);
  };
  const effectActive = tool === 'blur' || tool === 'pixelate' || editor.annotations.some((a) => isCosmeticEffect(a.kind));
  const overlayOnly = !editor.annotations.some((a) => a.kind === 'redact') && editor.annotations.some((a) => isOverlayKind(a.kind));

  return (
    <main className="editor" id="main">
      <header className="editor__head">
        <Button small variant="ghost" icon="back" disabled={flow.locked} onClick={() => void goBack()}>
          {t('editor.back')}
        </Button>
        <h1 className="editor__title" title={editor.task?.title}>
          {editor.task?.title || t('drafts.untitled')}
        </h1>
        <SaveIndicator editor={editor} />
        <div className="editor__tools">
          <Button small icon="undo" disabled={!editor.canUndo || flow.locked} onClick={editor.undo} aria-keyshortcuts="Control+Z" title={`${t('editor.undo')} (Ctrl+Z)`}>
            {t('editor.undo')}
          </Button>
          <Button small icon="redo" disabled={!editor.canRedo || flow.locked} onClick={editor.redo} aria-keyshortcuts="Control+Shift+Z" title={`${t('editor.redo')} (Ctrl+Shift+Z)`}>
            {t('editor.redo')}
          </Button>
          <Popover label={t('editor.moreActions')} align="end">
            <div className="menu-form">
              <div className="editor__zoom" role="group" aria-label={t('editor.zoom')}>
                <Button small variant="ghost" icon="zoomOut" aria-label={t('editor.zoomOut')} onClick={() => zoomTo(zoom / 1.25)} />
                <output>{Math.round(zoom * 100)}%</output>
                <Button small variant="ghost" icon="zoomIn" aria-label={t('editor.zoomIn')} onClick={() => zoomTo(zoom * 1.25)} />
                <Button small variant="ghost" icon="fit" aria-label={t('editor.zoomFit')} onClick={() => setFit(true)} />
                <Button small variant="ghost" icon="native" aria-label={t('editor.zoom100')} onClick={() => zoomTo(1)} />
              </div>
              <details className="editor__disclosure">
                <summary>{t('editor.sourceDetails')}</summary>
                <p className="panel__desc">{capture?.source} · {size?.width}×{size?.height}px · {capture ? formatDateTime(capture.capturedAt, locale) : ''}</p>
                <p className="panel__desc">{capture?.consistency}</p>
                {cursorPx ? <p className="mono">x {cursorPx.x} · y {cursorPx.y}</p> : null}
                {capture?.parentTransform ? <p>{t('editor.derived', { id: shortId(String(capture.parentTransform['parentCaptureId'] ?? ''), 8) })}</p> : null}
              </details>
            </div>
          </Popover>
        </div>
        <Button small icon="text" aria-expanded={notesOpen} aria-controls="editor-notes" disabled={flow.locked} onClick={() => setNotesOpen(!notesOpen)}>
          {t('editor.optionalNote')}
        </Button>
        <AgentControlButton variant="compact" />
        <div className="editor__outputs" role="group" aria-label={t('editor.export.group')} aria-describedby="editor-output-note">
          <Button small icon="clipboard" busy={exportBusy === 'copy'} disabled={exportBusy !== null || !editor.bitmap} onClick={() => void copyImage()}>
            {t('editor.export.copy')}
          </Button>
          <Button small icon="download" busy={exportBusy === 'download'} disabled={exportBusy !== null || !editor.bitmap} onClick={() => void downloadImage()}>
            {t('editor.export.download')}
          </Button>
          <span className="editor__outputnote" id="editor-output-note">{t('editor.export.independent')}</span>
        </div>
        <div className="editor__send">
          <p className="editor__consent" id="editor-send-caption">{t('send.caption')}</p>
          <Button
            variant="primary"
            icon="send"
            busy={flow.locked}
            disabled={!ready || editor.saveState === 'conflict' || agent.collaboration !== 'enabled'}
            aria-describedby="editor-send-caption"
            onClick={() => void flow.send()}
          >
            {t(flow.problem?.kind === 'failed' ? 'send.retry' : 'send.button')}
          </Button>
        </div>
      </header>

      <AgentStateBanner context="editor" />
      {flow.problem ? (
        <Notice
          tone="danger"
          actions={
            <>
              {flow.problem.kind === 'stale' ? (
                <Button small icon="refresh" onClick={() => void flow.reloadLatest()}>{t('send.reload')}</Button>
              ) : (
                <Button small icon="refresh" onClick={() => void flow.send()}>{t('send.retry')}</Button>
              )}
              <Button small variant="ghost" onClick={flow.dismissProblem}>{t('common.dismiss')}</Button>
            </>
          }
        >
          {flow.problem.message}
        </Notice>
      ) : null}
      {flow.issues.length > 0 ? (
        <Notice tone="danger" title={t('send.issues')}>
          <ul>{flow.issues.map((key) => <li key={key}>{t(key)}</li>)}</ul>
        </Notice>
      ) : null}
      {effectActive ? <Notice tone="amber">{t('editor.effectWarning')}</Notice> : overlayOnly ? <Notice tone="amber">{t('publish.overlayNotRedaction')}</Notice> : null}

      <p className="editor__hint">{t('editor.quickHint')}</p>

      {lease ? (
        <Notice tone="amber" actions={<Button small variant="warn" icon="release" busy={releaseBusy} onClick={() => void release()}>{t('top.release')}</Button>}>
          {t('editor.holdingControl')}
        </Notice>
      ) : null}
      {expiresSoon && editor.draft?.expiresAt ? <Notice tone="amber">{t('editor.expiresSoon', { when: formatDateTime(editor.draft.expiresAt, locale) })}</Notice> : null}
      {annotationIssues.length > 0 ? <Notice tone="danger">{t('editor.invalid')}</Notice> : null}

      <div className="editor__toolbar" inert={flow.locked}>
        <EditorToolbar
          tool={tool}
          onTool={(next) => {
            setTool(next);
            if (next !== 'crop') setCrop(null);
            selectIds([]);
          }}
        />
        {barCaps && hasAnyCaps(barCaps) ? (
          <div className="editor__quickstyle">
            <StyleControls layout="bar" caps={barCaps} values={barValues} onPatch={patchBar} />
          </div>
        ) : null}
        {selectedList.length > 0 ? (
          <>
            <Button small variant="ghost" icon="trash" aria-label={t('editor.deleteSelected')} onClick={() => deleteAnnotations(selectedSet)} />
            <Popover label={t('editor.selectionOptions')} align="end">
              <div className="menu-form">
                {selected ? <AnnotationInspector a={selected} onChange={updateAnnotation} onEditText={() => selected.kind === 'text' && startTextEdit(selected.id, selected.geometry.position, selected.geometry.text)} usedPins={usedPins} /> : <SelectionStyle items={selectedList} onApply={styleSelected} />}
                <details className="editor__disclosure">
                  <summary>{t('editor.arrange')}</summary>
                  <SelectionActions count={selectedList.length} onDuplicate={duplicateSelected} onLayer={layerSelected} onAlign={alignSelected} onDelete={() => deleteAnnotations(selectedSet)} />
                </details>
              </div>
            </Popover>
          </>
        ) : null}
        <Popover label={t('editor.layers')} align="end">
          <div className="menu-form">
            <AnnotationList annotations={editor.annotations} selectedIds={selectedSet} onSelect={(id, additive) => {
              setTool('select');
              selectIds(additive ? (selectedSet.has(id) ? [...selectedSet].filter((x) => x !== id) : [...selectedSet, id]) : [id]);
            }} onDelete={(id) => deleteAnnotations(new Set([id]))} onClear={() => setClearOpen(true)} />
          </div>
        </Popover>
      </div>
      <div className={`editor__body ${notesOpen ? 'editor__body--notes' : ''}`}>

        <div
          className={`editor__viewport tool-${tool} ${spaceDown ? 'is-panning' : ''}`}
          ref={viewportRef}
          inert={flow.locked}
          onPointerDown={onPointerDown}
          onPointerMove={onPointerMove}
          onPointerUp={(e) => finishGesture(e, false)}
          onPointerCancel={(e) => finishGesture(e, true)}
          onPointerLeave={() => setCursorPx(null)}
          onDoubleClick={(e) => {
            if (tool !== 'select' || !size || flow.locked) return;
            const rect = stageRect();
            if (!rect) return;
            const id = pick(pointFromClient(e.clientX, e.clientY, rect), PICK_PX / zoom);
            const hit = id ? annotations.find((a) => a.id === id) : undefined;
            if (hit && hit.kind === 'text') startTextEdit(hit.id, hit.geometry.position, hit.geometry.text);
          }}
          role="application"
          aria-label={t('editor.canvasLabel')}
          aria-describedby="editor-canvas-help"
        >
          {size && editor.imageUrl ? (
            <div className="editor__stage" ref={stageRef} style={{ left: pan.x, top: pan.y, width: size.width * zoom, height: size.height * zoom }}>
              <img src={editor.imageUrl} alt={t(isDrawing ? 'editor.drawingBaseAlt' : 'editor.baseImageAlt')} width={size.width} height={size.height} draggable={false} />
              <AnnotationLayer annotations={annotations} size={size} zoom={zoom} selectedIds={selectedSet} hiddenId={editing?.id ?? null} draft={draftShape} crop={crop} marquee={marquee} image={editor.bitmap} />
              {editing ? (
                <textarea
                  className="textedit"
                  autoFocus
                  lang="zh-TW"
                  spellCheck={false}
                  aria-label={t('editor.textInput')}
                  value={editing.text}
                  style={{
                    left: `${editing.position.x * 100}%`,
                    top: `${editing.position.y * 100}%`,
                    fontSize: editFont * zoom,
                    color: editColor,
                    fontFamily: textFontFamily(editTextStyle),
                    fontWeight: textFontWeight(editTextStyle),
                    textDecoration: editTextStyle === 'underline' ? 'underline' : 'none',
                    minWidth: `${Math.max(120, editFont * zoom * 6)}px`,
                  }}
                  rows={Math.max(1, editing.text.split('\n').length)}
                  onChange={(e) => setEditing({ ...editing, text: e.target.value })}
                  onBlur={commitText}
                  onPointerDown={(e) => e.stopPropagation()}
                  onKeyDown={(e) => {
                    if (e.nativeEvent.isComposing || e.keyCode === 229) return;
                    if (e.key === 'Escape') {
                      e.preventDefault();
                      setEditing(null);
                    } else if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
                      e.preventDefault();
                      commitText();
                    }
                  }}
                />
              ) : null}
            </div>
          ) : null}
          <p id="editor-canvas-help" className="sr-only">
            {t('editor.canvasHelp')}
          </p>
          {crop && size ? (
            <div className="cropbar" role="group" aria-label={t('crop.title')} onPointerDown={(e) => e.stopPropagation()}>
              <span className="mono">
                {(() => {
                  const px = cropToPixels(crop, size);
                  return `${px.width}×${px.height}px @ ${px.x},${px.y}`;
                })()}
              </span>
              <Button small variant="primary" icon="crop" onClick={() => setCropOpen(true)}>
                {t('crop.apply')}
              </Button>
              <Button small onClick={() => setCrop(null)}>
                {t('common.cancel')}
              </Button>
            </div>
          ) : null}
        </div>

        {notesOpen && editor.task ? (
          <aside className="editor__side" id="editor-notes" aria-label={t('editor.optionalNote')} inert={flow.locked}>
            <div className="editor__sidebody">
              <TaskForm value={editor.task} onChange={editor.setTask} issues={taskIssues} pins={pins} />
            </div>
          </aside>
        ) : null}

        {flow.phase !== 'idle' ? (
          <div className="sendprogress" role="status" aria-live="polite">
            <div className="sendprogress__card">
              <span className="sendprogress__bar" aria-hidden="true" />
              <strong>{t(`send.phase.${flow.phase}`)}</strong>
              <span>{t('send.progress.hint')}</span>
            </div>
          </div>
        ) : null}
      </div>

      <Modal
        open={editor.saveState === 'conflict'}
        onClose={() => undefined}
        dismissible={false}
        title={t('conflict.title')}
        footer={
          <>
            <Button
              busy={conflictBusy}
              onClick={() => {
                setConflictBusy(true);
                void editor.reloadFromServer().finally(() => setConflictBusy(false));
              }}
            >
              {t('conflict.useServer')}
            </Button>
            <Button
              variant="danger"
              busy={conflictBusy}
              onClick={() => {
                setConflictBusy(true);
                void editor.overwriteWithLocal().finally(() => setConflictBusy(false));
              }}
            >
              {t('conflict.useLocal')}
            </Button>
          </>
        }
      >
        <p>{t('conflict.body')}</p>
        {editor.saveError ? <Notice tone="danger">{editor.saveError}</Notice> : null}
      </Modal>

      <Modal
        open={leaveOpen}
        onClose={() => setLeaveOpen(false)}
        title={t('leave.title')}
        footer={
          <>
            <Button onClick={() => setLeaveOpen(false)}>{t('leave.stay')}</Button>
            <Button
              variant="danger"
              onClick={() => {
                setLeaveOpen(false);
                navigate({ name: 'workspace' });
              }}
            >
              {t('leave.discard')}
            </Button>
          </>
        }
      >
        <p>{t('leave.body')}</p>
        {editor.saveError ? <Notice tone="danger">{editor.saveError}</Notice> : null}
      </Modal>

      <Modal
        open={cropOpen}
        onClose={() => setCropOpen(false)}
        dismissible={!cropBusy}
        title={t('crop.title')}
        footer={
          <>
            <Button disabled={cropBusy} onClick={() => setCropOpen(false)}>
              {t('common.cancel')}
            </Button>
            <Button variant="primary" icon="crop" busy={cropBusy} onClick={() => void applyCrop()}>
              {t('crop.apply')}
            </Button>
          </>
        }
      >
        {crop && size ? (
          <p className="mono">
            {(() => {
              const px = cropToPixels(crop, size);
              return `${size.width}×${size.height} → ${px.width}×${px.height} @ (${px.x}, ${px.y})`;
            })()}
          </p>
        ) : null}
        <Notice tone="amber">{t('crop.body', { n: editor.annotations.length })}</Notice>
        <label className="check">
          <input type="checkbox" checked={deleteOld} onChange={(e) => setDeleteOld(e.target.checked)} />
          <span>{t('crop.deleteOld')}</span>
        </label>
        {cropError ? <Notice tone="danger">{cropError}</Notice> : null}
      </Modal>

      <Modal
        open={clearOpen}
        onClose={() => setClearOpen(false)}
        title={t('editor.clear.title')}
        footer={
          <>
            <Button onClick={() => setClearOpen(false)}>{t('common.cancel')}</Button>
            <Button variant="danger" icon="trash" onClick={clearAll}>
              {t('editor.clear.confirm')}
            </Button>
          </>
        }
      >
        <Notice tone="amber">{t('editor.clear.body', { n: editor.annotations.length })}</Notice>
      </Modal>
    </main>
  );
}

function SaveIndicator({ editor }: { editor: DraftEditor }): ReactNode {
  const { t, locale } = useI18n();
  switch (editor.saveState) {
    case 'saved':
      return (
        <Chip tone="ok" icon="check">
          {editor.lastSavedAt ? t('save.savedAt', { time: formatTimeOnly(new Date(editor.lastSavedAt).toISOString(), locale) }) : t('save.saved')}
        </Chip>
      );
    case 'dirty':
      return <Chip tone="amber" icon="warning">{t('save.dirty')}</Chip>;
    case 'saving':
      return <Chip tone="teal" icon="refresh">{t('save.saving')}</Chip>;
    case 'conflict':
      return <Chip tone="danger" icon="warning">{t('save.conflict')}</Chip>;
    case 'error':
      return (
        <span className="save-error" role="alert">
          <Chip tone="danger" icon="warning">{t('save.error')}</Chip>
          <span className="save-error__msg">{editor.saveError}</span>
          <Button small icon="refresh" onClick={editor.retrySave}>
            {t('common.retry')}
          </Button>
        </span>
      );
  }
}


