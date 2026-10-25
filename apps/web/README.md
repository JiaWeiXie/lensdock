# LensDock web UI (`apps/web`)

React 19 + Vite 8 + strict TypeScript single-page app. It is the human side of
the workspace: live VNC desktop, single-writer control, capture, standalone
drawing, a self-built annotation editor, feedback delivery, file transfer and
diagnostics. The Gateway serves `apps/web/dist` and every call is same-origin
under `/api/v1`.

The interface defaults to English (`en-US`). Traditional Chinese (`zh-TW`) is a complete translation, selectable in Settings; the choice is saved in `localStorage` (`lensdock.lang`). Source identifiers and comments are English.

## Run

```sh
pnpm install --frozen-lockfile
pnpm dev          # http://127.0.0.1:5173, proxies /api (incl. WS) to LENSDOCK_GATEWAY
pnpm build        # tsc -p both configs --noEmit, then vite build -> dist/
pnpm test         # vitest (coordinate contract, selection and alignment, history, graphemes, effects, palette, mode and pause UI)
pnpm typecheck
```

`mise.toml` pins node 24.22.1 / pnpm 11.28.8; `.npmrc` stores exact versions.

## Dependency pins (exact)

| Package | Version | License |
|---|---|---|
| react, react-dom | 19.3.1 | MIT |
| @novnc/novnc | 1.7.0 | MPL-2.0 (core) |
| @fontsource/noto-sans-tc | 5.3.1 | OFL-1.1 |
| @fontsource/noto-sans-sc | 5.3.1 | OFL-1.1 |
| @fontsource/noto-sans-jp | 5.3.1 | OFL-1.1 |
| @fontsource/noto-sans-kr | 5.3.1 | OFL-1.1 |
| @fontsource/noto-color-emoji | 5.3.2 | OFL-1.1 |
| @fontsource/ibm-plex-sans | 5.3.0 | OFL-1.1 |
| @fontsource/ibm-plex-mono | 5.3.0 | OFL-1.1 |
| vite | 8.3.5 | MIT |
| @vitejs/plugin-react | 6.1.2 | MIT |
| typescript | 5.9.3 | Apache-2.0 |
| vitest | 5.0.5 | MIT |
| @types/react / @types/react-dom | 19.3.0 | MIT |
| @types/node | 26.6.5 | MIT |

Fonts are bundled from `@fontsource` WOFF2 chunks only (a Vite plugin drops the
WOFF1 fallback), so the UI and the PNG export render identically offline with
no CDN. Notices and license texts: `public/licenses/NOTICE.txt`,
`OFL-*.txt`, `noVNC-LICENSE.txt`, `MPL-2.0.txt`.

## API assumptions (Gateway public surface)

All requests are same-origin, cookie-authenticated; every mutation sends
`X-CSRF-Token` and an `Idempotency-Key`. The UI never receives, stores or sends
a lease capability token, and no credential appears in a URL.

| Purpose | Call |
|---|---|
| Pair / session | `POST|GET|DELETE /api/v1/session` → `{actor, csrfToken}` |
| Workspace state | `GET /api/v1/workspaces`, `/workspaces/{ws}` (runtime state flattened: `controlState, holderId, ready, runtimeGeneration, epoch, holderMode, lease, leaseExpiresAt, browserVersion, handoff, locked, lockCode, maintenance, capture, resources`) |
| Health | `GET /workspaces/{ws}/health` → raw runtime readiness detail |
| Targets | `GET /workspaces/{ws}/targets` → `{targets:[{id,type,title,url,visible}]}` |
| Lease | `POST /workspaces/{ws}/leases {mode,ttlSeconds?}` (+`X-LensDock-Takeover-ID`), `POST .../leases/{id}/renew {epoch,runtimeGeneration}`, `DELETE .../leases/{id}`, `GET .../leases/current`; response modelled as `{lease}` only |
| Takeover | `POST /workspaces/{ws}/takeover {reason,force}` → `{status,requestId}`, `GET` → `{status:DRAINING|READY|DRAIN_TIMEOUT,drained,forced,priorLease,expiresAt}`, `DELETE` |
| Capture | `POST /workspaces/{ws}/captures {source:'desktop'}` → `{captureId,...}`; `PUT /api/v1/captures/{id}/image` (PNG, `X-LensDock-Captured-At`); `POST /workspaces/{ws}/captures {source:'page-viewport',targetId,leaseId,allowInterrupt?}`; `POST /workspaces/{ws}/captures {source:'drawing',imageWidthPx,imageHeightPx}` (integers 64..4096, no lease, no target, answers a ready capture); `GET /captures/{id}`, `GET /captures/{id}/image`, `POST /captures/{id}/crop {x,y,width,height}` |
| Clipboard | `PUT /workspaces/{ws}/clipboard` `text/plain; charset=utf-8` with `X-LensDock-Lease-Epoch` + `X-LensDock-Runtime-Generation` |
| Resolution | `GET /workspaces/{ws}/resolution` → `{current,supported[]}`; `PUT` `{width,height}` with the same lease-epoch headers, returns the size read back |
| Files | `GET /workspaces/{ws}/files?area=downloads|uploads` → `{area,files,usage}`; `GET|DELETE /files/{area}/{name}`; `PUT /files/uploads/{name}?overwrite=1` octet-stream (XHR for progress) |
| VNC | `WS /workspaces/{ws}/vnc/view`; `WS /workspaces/{ws}/vnc/control?epoch=&generation=` (cookie auth, no subprotocol, no token) |
| Feedback | `POST /api/v1/feedback`, `GET /feedback/drafts?workspaceId=`, `GET /feedback/{id}/draft`, `PATCH /feedback/{id}` (If-Match), `PUT /feedback/{id}/draft/preview` (If-Match), `GET /feedback/{id}/draft/preview/{screenshot.redacted.png|screenshot.annotated.png}`, `POST /feedback/{id}/publish` (If-Match), `GET /feedback?workspaceId=&state=&cursor=`, `GET /feedback/{id}/revisions`, `POST /feedback/{id}/revisions`, `GET /feedback/{id}?revision=`, `GET /feedback/{id}/delivery?revision=`, `GET /feedback/{id}/artifacts/{name}?revision=`, `GET /feedback/{id}/bundle?revision=`, `POST .../dispatch|verify|cancel` |
| Events / audit / system | `GET /api/v1/events?workspaceId=` (SSE, refresh hint), `GET /api/v1/audit?workspaceId=&after=` (admin), `GET /api/v1/system` (`gateway, storage, release, update, feedback`) |
| Usage mode and pause | `GET /workspaces/{ws}/agent` → `{mode, paused, revision, updatedAt}` with `mode` of `NOVNC` or `AGENT` (also flattened into the workspace read as `agentControl`); `PUT .../agent {mode}` (human or admin); `POST .../agent/pause` and `POST .../agent/resume` with body `{}` (human or admin); `GET .../agent/events` (SSE event `agent-control`, carries only this state, id is the revision); `GET .../agent/setup` → `{prompt}` (human or admin, `AGENT` mode only) |

Error envelope: both flat `{code,message,requestId}` and `{error:{...}}` are
accepted. Every optional field the Gateway may omit renders as “unknown”; the
UI never substitutes 0 or a guess (`resources`, `lease`, `browserVersion`,
`resolution.supported`, `/system` sections).

## Feature map

- **Login & session**: normal token pairing → HttpOnly cookie, CSRF on mutations,
  eight-hour session lifetime, no credential in URL/storage. Explicit local
  `LENSDOCK_DEMO_NO_AUTH=1` automatically creates a same-origin `demo-human`
  browser session with no expiry or idle logout. It never bypasses API CSRF,
  workspace scope or control leases. Logout offers explicit token-free re-entry.
- **Workspace**: health chip, runtime readiness, lock/maintenance/capture/
  handoff notices, controller and holder, tab list with foreground honesty
  (an Agent-held target is stated as untracked, not guessed).
- **Desktop**: noVNC RFB via the public API only (`toBlob`, `sendKey`,
  `qualityLevel`, `compressionLevel`, `scaleViewport`); bounded retries with
  backoff; freshness gate (last frame age, Gateway state age) before any
  capture; fit/actual size, fullscreen, quality/compression, remote resolution
  picker (only advertised sizes), combo keys, release-all-modifiers.
- **Control**: acquire, takeover handshake with drain polling, explicit forced
  takeover after `DRAIN_TIMEOUT`, cancel, release, 10 s heartbeat, epoch/
  generation fencing, stream closed by the server (4001/4002) triggers lease
  re-read; after a page capture the new lease id/epoch is adopted before any
  further lease-bound request.
- **Capture**: the always-visible Capture & annotate button grabs the live
  framebuffer, uploads the desktop PNG and opens a blank annotation draft in
  one action (no lease or control change). Draft retries reuse a successful
  upload. The source, frozen-image, target and consistency warnings remain
  visible as toasts. Page-viewport capture remains an advanced per-target
  flow with an explicit interruption warning. Limits remain 32 MiB / 24 MP.
- **Standalone drawing**: New drawing immediately creates a 1600×900 white
  canvas and opens its draft. The adjacent options button supports a custom
  title and 64 to 4096 px per side. It needs neither the browser nor a lease, so it works while
  the runtime is unavailable. The capture is a generated canvas with no target,
  navigation or page, and its consistency is unknown. Cropping a drawing gives
  a new drawing draft; the annotations are not copied and the dialog says so.
- **Annotation editor**: self-built, with no tldraw or Excalidraw dependency.
  Tools and shortcuts: select (V), pan (H), rectangle (R), filled rectangle (F),
  ellipse (O), diamond (D), line (L), arrow (A), freehand (P), text (T, local
  IME, never interrupted), step counter (N, wire kind `pin`, with comments),
  highlighter (M), spotlight (S), blur (B), pixelate (I), solid redact (X) and
  crop (C). The keys match physical keys and never fire while composing or
  typing. Editing: Shift-click
  and marquee selection, group move, resize handles and line or arrow
  endpoints, duplicate (Ctrl+D), six alignments, four layer commands
  (Ctrl+[ and Ctrl+]), per-object and bulk style including fill for closed
  shapes, undo and redo (200 steps), keyboard nudge, ETag autosave (800 ms) with
  conflict resolution, a confirmed and undoable Clear all, and per-grapheme
  limits. Crop creates a new capture and draft; annotations are not carried
  over, and the UI warns before cropping. Copy image and Download PNG export a
  flattened PNG with redactions burned in and no editable data. Geometry is
  normalized to the real image content rectangle; export waits for font
  readiness and rasterizes with the same primitives as the SVG layer, and
  solid redactions are painted last.
- **Advanced annotation tools** (feedback schema 0.3.0): highlighter, spotlight,
  blur and pixelate are rectangle kinds. The highlighter is a plain alpha fill
  (color `#f2a900` by default, opacity 0.1 to 0.8, default 0.35). The spotlight
  dims everything outside the union of all spotlights once at a fixed 0.6.
  Blur (radius) and pixelate (block edge) take 2 to 64 source pixels (defaults
  12 and 16). Arrows can be filled, open, double or curved (a draggable handle
  bends the curve). Text has seven styles (plain, outline, label, shadow, mono,
  bold, underline) using the bundled fonts only. Freehand strokes are smoothed by
  default (a rendering flag over the raw points). The editing layer paints real
  blur and pixelate pixels with the same integer algorithm as the PNG export and
  the Gateway, so they match on screen, in the file and in the shared base
  image. Solid redactions are blackened before any effect samples the base and
  repainted last, so an overlapping blur cannot reveal what they hide. Highlight,
  spotlight, blur and pixelate are not redaction, and the panel says so.
- **Color palette**: built-in swatches plus a custom palette. Add the current
  picked color, reuse it for stroke and fill, and remove it. The palette lives
  only in `localStorage` (`lensdock.editor.palette.v1`), holds at most 12
  validated `#RRGGBB` values and ignores anything else it reads back. There is
  no screen eyedropper.
- **Direct PNG outputs**: Copy PNG and Download PNG sit in the editor toolbar
  next to Send. They export the flattened image without publishing, without an
  Agent and without changing the draft, and the toolbar says so. Copy needs the
  browser's clipboard permissions; if they are denied the UI shows the error
  instead of success. The clipboard image and the downloaded file are pixel
  identical, but not necessarily byte identical, because Chrome re-encodes the
  clipboard image.
- **Send**: one click approves the visible canvas, freezes the draft and
  automatically saves, renders, uploads and validates the server preview.
  It then publishes the approved ETag with `dispatch: true`, atomically creating
  the immutable revision and its QUEUED delivery. Editing stays locked during
  the request. A double click is single-flight; unknown-response retries keep
  the same PNG, ETag and idempotency keys. A stale version requires the human
  to load and review the updated draft. There is no second confirmation dialog.
  A revision is published with
  feedback schema 0.2.0 when the capture is a drawing, an annotation is an
  ellipse, diamond or line, or optional prose would not meet the legacy 0.1.0
  minimums. Otherwise it stays at 0.1.0. Empty titles become `Visual feedback`
  only at publication; other prose stays empty. Published revisions never change.
  Feedback schema 0.3.0 is used when an annotation is a highlight, spotlight,
  blur or pixelate, or carries the new arrow control, arrow style, text style,
  effect size, opacity or smoothing fields; otherwise the 0.1.0 or 0.2.0 rule
  above still applies. New revisions do not carry blur and pixelate annotations
  over, because their effect is already in the shared base image.
- **Feedback delivery**: distinct states READY → QUEUED → ACKNOWLEDGED →
  IN_PROGRESS → RESOLVED → VERIFIED (+CANCELLED) with per-step owner and
  timestamps, automatic routing to the configured workspace Agent,
  human verify/reject, cancel, revision creation, artifact/bundle download, and
  Agent results marked untrusted until verified.
- **Input/files**: manual UTF-8 clipboard bridge (explicit read/send, 64 KiB
  limit, no logging) plus “send and paste” with one Ctrl+V; controlled
  upload/download with quota meters, name validation, overwrite opt-in and
  progress/cancel.
- **Diagnostics/settings**: layered health, CPU (cumulative → delta), memory,
  disk free/total with unknown≠0, feedback queue/latency/artifact failures,
  live events, admin-only audit, release manifest verification state, update
  candidates/plans/rollback as copyable host commands (never applied by the
  UI), storage usage, language, shortcut toggle, license links.
- **Usage mode and pause** (`src/agent/`): Settings has a Mode panel to switch
  between `NOVNC` (browser only) and `AGENT` (collaboration), enabled for human
  and admin roles and read-only for viewers. The state comes only from the
  server (see below). In `NOVNC` the workspace shows the remote browser and its
  ordinary human controls; New drawing, Capture & annotate, the Dock feedback
  panel, the editor, Send, pause and all Agent status are hidden, and an
  `#/editor/...` link returns to the workspace. In `AGENT` the panel shows the
  English onboarding prompt (always English, read-only, one-click Copy)
  from `GET .../agent/setup`. The prompt has no token, cookie or lease; it
  needs an installed `lensdock` CLI and an approved Agent token file that the
  user provisions privately (it shows a placeholder path), and it covers
  `lensdock workspace watch`, a fresh AGENT lease and Bridge only after an
  explicit human resume, feedback and image reading, ACK, the shared noVNC
  browser, the stop errors, untrusted page content and image-only coordinates.
  Whether a model reads or follows it is not guaranteed.
- **Pause control**: a prominent Pause and Resume button is in the workspace
  top toolbar, in Settings and in the editor toolbar (`AgentControlButton`,
  variants toolbar, panel and compact), hidden in `NOVNC`. Pause is one click
  with no confirmation. The banner (`AgentStateBanner`) says the Agent is
  waiting, that resuming needs an explicit click (closing a screenshot or the
  editor does not resume), and that website actions already dispatched may
  still finish. Disabling Agent mode says in plain text that it disconnects the
  Agent. Paused still allows human drafts and Send; it blocks only browser
  control by the Agent.
- **Accessibility**: semantic landmarks, skip link, real buttons with
  `aria-pressed`/`aria-expanded`, tablist keyboard support, focus-visible
  rings, glyph+text for every state, shortcuts disabled during IME
  composition and in text fields, `prefers-reduced-motion` respected.

## Compact layout, font size and responsive behavior

The annotation editor is draw-first, with a compact horizontal toolbar. Tools sit in five groups: Select and pan;
Shapes and lines (rectangle, filled rectangle, ellipse, diamond, line, arrow,
freehand); Text and steps; Emphasis and hiding (highlighter, spotlight, blur,
pixelate, solid redact); and Crop. Arrow is selected initially. At 900 px and
below the Shapes, Text and Emphasis groups fold into grouped overflow menus
instead of opening a large side panel. Style controls follow the tool or the
selection: color, width, fill, text size, opacity, effect size, arrow style,
text style and smoothing, each shown only where it changes the saved result.
The browser rail is hidden while editing an image; a held human lease still
produces the explicit release warning. No inspector or task form opens by
default. Layers, style, arrange/alignment, zoom and source metadata remain
available through explicit disclosures. Annotation comments are optional and
collapsed.

Notes opens optional title and description; classification, intent,
expected/actual and steps are collapsed under Advanced task details. No prose
is required for publication. The default intent remains analyze-only. The
basic arrow flow is three operations: create/capture, drag, Send.
Choosing another tool adds one operation; extra edits and advanced options
are outside that basic count. Sending publishes and queues atomically; Agent
acknowledgment and execution remain separate states. READY revisions created
through the API also have a Send button without an Agent ID input.

The top bar starts compact. It shows connection and controller state,
New drawing and Capture & annotate; Show tools expands the remaining actions.
Both creation buttons remain visible on mobile. The dock starts collapsed as
an icon strip and opens with the chevron or
Alt+Shift+D. The other global shortcuts are Alt+Shift+K (control), C (desktop
capture), F (fullscreen) and M (fit). They match physical keys, never fire
while typing, composing with an IME or when the remote canvas has focus, and
can be switched off in Settings.

Settings offers an interface font size of 12, 14, 16, 18 or 20 px (default 14,
kept in `localStorage` as `lensdock.fontSize`). It scales LensDock interface
text only, not the remote desktop or exported images.

Shared form fields align to the top and use `min-width: 0`, so a long value
cannot push its neighbours. Paired task fields use a font-relative auto-fit
column (minimum 10rem) and drop to one column on narrow screens. Audit tables
turn into label and value cards at 720 px and below. Popovers render in a
portal on `body`, stay inside the viewport and scroll internally. A native
single-line text input keeps a long value inside the bounded input and scrolls
it there by design, so not every editable value is shown in full.

These layouts were checked in the running UI at widths 320, 390, 768, 900, 1024,
1280 and 1600 px in both languages. Settings and the feedback form were checked at
all five font sizes with no overflow or misaligned controls. The dock panels,
diagnostics tabs, editor, share preview, drawing dialog and custom palette were
checked at the largest font size. Paused `AGENT` and `NOVNC` settings and
workspace views had no horizontal overflow or overlapping controls, and all 14
grouped tools fit at 320 px and 20 px. These are the tested combinations, not a
claim for every browser or device. At 1280×800 and 14 px the remote canvas fills
about 75% of the viewport, and a 16:9 source is letterboxed rather than cropped
or distorted.

## Backend assumptions for mode and toolbar

- The Runtime is the only authority for mode and pause. The UI derives policy
  from the workspace read (polled and refreshed by events) plus the
  `agent/events` stream, so other tabs follow a change. Nothing is cached in
  `localStorage`. A missing, malformed or unreachable policy is `unknown`:
  collaboration surfaces stay hidden and mutations are disabled, never assumed
  to be `AGENT`. An editor that is already open keeps unsent edits through a
  transient unknown state, but cannot Send.
- `NOVNC` closes the capture, drawing, feedback and artifact routes for every
  role, and denies Agent lease, target, feedback, bundle and setup calls with
  409 `AGENT_MODE_DISABLED`. A paused workspace answers Agent lease calls with
  409 `AGENT_PAUSED`. The UI maps these to localized messages.
- Startup `LENSDOCK_MODE` or `--mode` is only the initial mode; the saved policy
  wins afterwards, including pause. The UI never edits the startup value.
- Publishing sends the annotations as they are; the Gateway applies blur and
  pixelate to `screenshot.redacted.png` and re-encodes the uploaded annotated
  PNG without filtering it again. The UI relies on the Gateway for the schema
  version choice and for server-side limits (including 100 million affected
  pixels for blur and pixelate).

## Deliberate non-features (surface must not pretend)

- No remote VNC password prompt: credentials are never collected by the UI.
- No audio, recording or high-frame-rate claims.
- The UI never applies updates, never restarts the browser, and never claims a
  queued item was read by an Agent.
- Agent accounts use the CLI/MCP; the web UI shows them an explanatory notice
  instead of a desktop.
- No whiteboard SDK: the editor does not depend on tldraw or Excalidraw.
- Native IME composition has been verified only for macOS Zhuyin: composed 你
  reached the local clipboard field, went through Send and paste, and an
  independent Agent CDP readback returned exactly that value. The
  `compositionend` seen by the relay is synthetic in that run, and Japanese and
  Korean native IMEs are untested.
- Pause, disable and the onboarding prompt do not guarantee that a model hears
  about them; the stop is the Runtime's server-side fence. Already-dispatched
  website actions cannot be undone.
- No image import or upload, multiple images, backgrounds or rotation in the
  editor, and no screen eyedropper.
