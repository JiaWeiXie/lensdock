# LensDock

LensDock gives a person and, when you enable it, an Agent access to the same persistent, headful Chromium browser. The web UI provides noVNC viewing and human takeover. In the default **AGENT** usage mode it also provides screenshot annotation, standalone drawing and approved image delivery to an Agent. In **NOVNC** mode LensDock is only a remote browser: annotation, drawing, feedback and every Agent feature are hidden and disabled. The host CLI manages releases, encrypted checkpoints and paired rollback.

## Current status

The Runtime, authenticated Gateway, web UI, editor, feedback CLI/MCP server and host update workflow are implemented. The integrated development deployment has been exercised on macOS arm64 with OrbStack, including real Claude Code and Codex image handoffs, native macOS Zhuyin input, successful updates, failed-candidate recovery and paired rollback.

This is a development build. The demonstrated releases are unsigned `dev` releases; their integrity checks do not certify a signed production distribution. Current integrated amd64 coverage, Japanese/Korean native IMEs and the optional M4 features remain unverified or outside the implemented scope. Historical M0 amd64 results apply to M0 only. See [the validation report](docs/VALIDATION-REPORT.md) for the evidence and limits.

The annotation tools and usage modes described below were exercised on macOS arm64 with OrbStack on 2026-10-25. Local release `local-toolbar-modes-16` covered mode startup from the environment and from the flag, a paused state surviving a Docker restart, a real Agent lease with a CDP bridge, live role and CSRF checks, a schema `0.3.0` bundle pulled by the scoped Agent CLI, and pixel comparison of the clipboard, downloaded and shared images. A later managed update, release `17`, kept the production workspace's paused policy (revision 6, same timestamp) after its candidate passed an isolated `AGENT` policy check, and a probe showed that the browser user (uid 10001) cannot read the policy file. Timings from these single runs (for example, a pause closing a live CDP connection within tens of milliseconds) are observations, not a service-level promise. Evidence files stay under `.lensdock/evidence/final-runtime/`, which Git excludes.

## Start a local workspace

Run these commands from the repository root. You need a running Docker engine with Compose v2 and [mise](https://mise.jdx.dev/). Docker must support the documented Chromium user-namespace sandbox; LensDock does not fall back to `--no-sandbox` or privileged containers.

```bash
mise install
mise run build
mise run build:linux
mise run web:build
mise run image:build
mise run gateway:image

# First installation only. Existing product configuration is never overwritten.
mise run product:init
mise run product:up

curl --fail --silent --show-error http://127.0.0.1:18081/readyz
```

Open **http://127.0.0.1:18081/**. Pair through the UI's token field using the contents of `.lensdock/product/admin-token`. Read that private file locally; do not put its contents in a URL, command argument, screenshot, issue or chat. The bootstrap also creates `human-token`, `viewer-token` and `agent-token`. Use the Agent credential for Agent delivery and control; an admin credential is not an Agent identity.

The default workspace is `default`. State and credentials live under `.lensdock/`; browser profile, database, downloads and image data use persistent Docker volumes. The public port binds to loopback. Runtime traffic uses mutual TLS inside the Compose network. Remote exposure requires an explicit TLS and origin configuration.

The default build architecture follows `LENSDOCK_ARCH` in `mise.toml`. `build:linux` produces the Linux CLI used inside update verification; the host binary alone is insufficient on macOS.

### Choose a usage mode

A workspace runs in one of two usage modes.

| Mode | What you get |
|---|---|
| `AGENT` (default) | A shared browser with Agent collaboration: annotation, drawing, screenshots, feedback delivery, Agent leases and the CDP bridge. Existing deployments keep this behavior. |
| `NOVNC` | A plain remote browser. Annotation, drawing, capture, feedback and every Agent control are hidden, and the Gateway refuses those routes for every role. Human control, clipboard, files and the live view still work. |

**Set the initial mode.** Use one of these before the first start:

- `LENSDOCK_MODE=NOVNC` (or `AGENT`, case-insensitive) in the environment of `product-init`, for example `LENSDOCK_MODE=NOVNC mise run product:init`. The value is written to `.lensdock/product/compose.env` and passed to the Runtime container.
- `bin/lensdock admin product-init --mode NOVNC`. The flag overrides the environment variable. Any other value is a usage error.
- For the Runtime image itself, `lensdock-runtime supervise --mode NOVNC|AGENT` overrides `LENSDOCK_MODE`. The image `ENTRYPOINT` already ends in `supervise`, so pass `--mode NOVNC` as the container arguments, either as the Compose `command` or after the image name in Docker. See [the Browser Runtime image notes](docker/browser/README.md).

**The initial mode applies once.** It is used only while the workspace has no saved policy. After that, the saved mode and pause state win, including across container restarts. If you switch to `NOVNC` or pause the Agent, a restart does not undo it, even when the environment still says `AGENT`. Changing the environment variable or flag later does not change a workspace that already has a policy; use Settings instead.

The policy lives on its own volume, `lensdock-runtime-policy`, mounted at `/data/runtime` in the Runtime container. The supervisor (uid 10000) owns the directory with mode `0700` and writes `agent-control.json` with mode `0600`. Chromium runs as uid 10001 and cannot read or write the policy. Product Compose and the deployment record declare the volume as retained data; existing installations must [enroll the Runtime policy volume](docs/UPDATE-IMPLEMENTATION.md#adding-the-runtime-policy-volume) before upgrading. Do not remove the volume. A corrupt policy file stops Runtime startup. If saving a pause or disable fails, the Runtime still fences the Agent for the current session, but the request returns an error and the saved state may be older after a restart. The retained policy carries across compatible updates and rollbacks. A Runtime build from before this feature neither reads nor enforces it, so its rollback drops the mode and pause restrictions.

**Switch modes in Settings.** The **使用模式** panel (Usage mode) has an **Agent 協作** switch for human and admin identities; a viewer sees the state only.

- Turning it off disconnects the Agent immediately: its lease ends, its tickets and CDP connections close, and its Gateway grant is removed. A HUMAN lease is kept. Existing feedback is retained but unreachable until you turn collaboration back on. In `NOVNC`, Agent requests for leases, targets, feedback, bundles and the setup prompt fail with 409 `AGENT_MODE_DISABLED`. The Agent identity can still read its scoped workspace state and the state notification stream, so an Agent running `workspace watch` is told. A deep link to the editor redirects to the workspace.
- Turning it on starts unpaused. It never revives an old lease; the Agent must acquire a new one.
- If the Gateway cannot read the policy, the UI hides collaboration and blocks changes instead of assuming `AGENT`.

**Give the Agent its instructions.** In `AGENT` mode, Settings shows an English setup prompt that the Gateway generates for the workspace, and **複製提示詞** copies it to the clipboard. The prompt contains no bearer token, session cookie or lease. The Agent needs the `lensdock` CLI installed and an approved Agent token file delivered through a private channel; the prompt uses a placeholder path, and you must provide the real file yourself. It covers `workspace watch`, a fresh lease and bridge, feedback, image and ACK commands, the stop rules, treating page and feedback text as untrusted, and using image-only coordinates. LensDock cannot tell whether a model read the prompt or the notification output.

**Pause and resume.** **暫停 Agent** sits in the top toolbar, in Settings and in the annotation editor. One click pauses with no confirmation. The Runtime fences the Agent lease and closes its CDP connection, and further control returns 409 `AGENT_PAUSED`. You can still capture, annotate and Send, and the Agent can still read, ACK and resolve its assigned feedback. **恢復 Agent** only lets the Agent request a fresh lease. It does not take control away from you, and closing the editor does not resume. A page action the Agent sent just before the pause may still finish, and LensDock cannot undo it.

**Watch from the Agent side.** The CLI streams mode and pause changes as one JSON object per line. The first line is the current state:

```bash
bin/lensdock workspace watch default \
  --api http://127.0.0.1:18081 \
  --token-file .lensdock/product/agent-token
```

Each line has the shape `{"mode":"AGENT","paused":false,"revision":3,"updatedAt":"<RFC 3339 time>"}`. If the stream drops, the command exits with an error; treat that as an unknown state and stop browser work. The stream is a state feed for a process the Agent runs. It does not insert a message into a model conversation.

### Operate an existing installation

After initialization, use the deployment-aware CLI. It includes the current generation override, custom origin and volume selection:

```bash
bin/lensdock update compose --deployment .lensdock/deployment.json -- ps
bin/lensdock update compose --deployment .lensdock/deployment.json -- logs --tail 100 gateway runtime
bin/lensdock update compose --deployment .lensdock/deployment.json -- down
bin/lensdock update compose --deployment .lensdock/deployment.json -- up -d --wait
```

`down` retains data. Do not use `down -v`, prune the managed volumes, rerun `product-init`, or start an updated installation with a bare base Compose file. Gateway web sessions are held in memory: normal mode requires pairing again after a Gateway restart, while local demo mode re-enters automatically. Neither erases the Chromium profile.

### Local demo without token entry

Set `LENSDOCK_DEMO_NO_AUTH=1` in `.lensdock/product/compose.env`, then restart
the Gateway with the deployment-aware Compose command above. Set it to `0`
and restart to restore normal token pairing. The default is `0`; the standalone
Gateway also accepts `serve --config FILE --demo-no-auth`.

Demo mode requires a loopback public origin and exactly one workspace.
Keep the Compose host port bound to `127.0.0.1`; do not expose this mode
through a public proxy or tunnel. A same-origin browser enters automatically
as `demo-human`, with a non-expiring server session and no idle logout.
The cookie remains HttpOnly and SameSite=Strict; all mutations still require
CSRF. Anonymous raw API requests are not enabled. Agent tokens, request
deadlines, Runtime authentication and control lease TTLs remain unchanged.
Normal token sessions still expire after eight hours.

Gateway restarts clear in-memory sessions; the demo UI enters again
automatically. Explicit logout stays signed out across reloads and offers
**進入展示** to return without a token. Up to 16 demo sessions are retained;
new ones replace the oldest. Use only on a trusted single-user machine.

### Try the synthetic text fixture

In another terminal, serve only the checked-in fixture directory:

```bash
mise exec -- python -m http.server 18082 --bind 0.0.0.0 --directory tests/fixtures
```

On the tested macOS/OrbStack setup, navigate the remote Chromium browser to `http://host.docker.internal:18082/cjk-emoji.html`. On Linux, use an explicitly reachable host address; `localhost` inside Chromium refers to its container. The fixture server binds to all host interfaces for container access, so use it only on a trusted development network and stop it when finished. Never serve the repository root or `.lensdock/`, which can contain credentials and private images.

## Use the workspace

The default UI language is Traditional Chinese. Settings offers English and font sizes of 12, 14, 16, 18 or 20 px. Font size changes the application controls, not remote desktop pixels or exported images.

Steps 3 to 5 and the annotation features below need **AGENT** mode. In **NOVNC** mode they are absent; see [Choose a usage mode](#choose-a-usage-mode).

1. Watch the live desktop without taking control. Choose **取得控制** to interact, or **接管** and enter a reason when an Agent owns the lease. Old Agent connections are revoked before human control is granted.
2. Use **輸入與檔案** for UTF-8 text and file transfer. Compose text with your local IME, focus the intended remote input in noVNC, then choose **送出並貼上**. **送出** only writes the remote clipboard. The remote clipboard expires; contents are not logged. Uploads and downloads stay in the workspace's managed directories.
3. Click **截圖標註** to capture the desktop and open its annotation draft directly. Expand the tools or **擷取與草稿** for advanced selected-page capture. Desktop capture does not steal an Agent lease. A page-viewport capture temporarily fences control and restores an eligible owner with a new epoch.
4. Mark the private image with arrows, shapes or text, redact sensitive information, then click **送出**. This approves the visible canvas and automatically prepares the share images, publishes an immutable revision and queues it for the workspace's configured Agent. Notes and advanced details are optional; there is no Agent ID field.
5. Inspect the feedback status. **已排隊** means queued; **Agent 已確認** means acknowledged. Neither means the Agent has fixed the task. Review returned evidence before marking a resolved task verified.
6. To stop an Agent that is driving the browser, click **暫停 Agent** in the top toolbar, Settings or the editor. Resume only when you want the Agent to request a fresh lease; see [Choose a usage mode](#choose-a-usage-mode).

The tools and side panel collapse to preserve desktop space. At the verified 1280×800 viewport with the default 14 px font and collapsed panel, the connected noVNC canvas occupied 75.32% of the viewport. Narrow layouts wrap controls, stack fields and use internal scrolling where needed. The remote image keeps its aspect ratio rather than being cropped to fill a phone screen.

### Annotate a screenshot or draw from scratch

The default annotation path takes three operations:

1. Click **新增繪圖** or **截圖標註**. The editor opens directly.
2. Drag on the image to draw an arrow. Selecting another tool adds one operation.
3. Click **送出**. This confirms that the visible canvas is safe to share. Saving, rendering, server redaction, publication and queuing happen automatically.

There is no mandatory title form, creation modal, separate preview or confirmation
step, Agent ID field, checkbox checklist or success dialog to dismiss. More
complex edits and advanced settings are outside that basic three-operation count.
The server chooses the workspace's configured default Agent, or its sole Agent.
Missing or ambiguous configuration leaves the draft unpublished and asks for
administrator setup. **已排隊** does not confirm that an Agent has read or acted
on the feedback.

**新增繪圖** creates a 1600×900 blank canvas; its adjacent options button lets you choose a title and 64 to 4096 pixels per side. Drawing needs no browser lease or runtime capture. It remains identifiable as `drawing` / `generated-canvas`, with no browser target or navigation metadata. **截圖標註** captures the live desktop and creates its draft in the same action, without taking control. A failed draft creation can retry the already uploaded image without taking another screenshot.

The self-built editor has select and pan, rectangle with a **填滿矩形** (filled rectangle) preset, ellipse, diamond, line, arrow, freehand, text, **步驟編號** (step counter), **螢光筆** (highlighter), **聚光燈** (spotlight), **模糊** (blur), **馬賽克** (pixelate), opaque **遮蔽** (redact) and crop. It supports Shift multi-select, marquee selection, group movement, resize handles, line endpoints, duplicate, alignment, layer ordering, styles, autosave, undo and redo. It does not embed tldraw or Excalidraw.

- **Arrow styles:** filled (default), open, double and curved. A curved arrow has a round handle you drag to bend it.
- **Text styles:** plain, outline (default), label, shadow, monospace, bold and underline. Only bundled fonts are used.
- **Freehand:** new strokes are smoothed by default. Strokes saved before this option exist stay unsmoothed.
- **Highlighter:** opacity 0.1 to 0.8, default 0.35; its colour is the stroke colour, default `#f2a900`.
- **Spotlight:** dims everything outside the union of all spotlight areas at a fixed strength of 0.6. There is no per-spotlight setting.
- **Blur and pixelate:** effect size 2 to 64 source-image pixels. The defaults are a blur radius of 12 and a pixelate block of 16.
- **Custom palette:** save up to 12 colours; they are kept in this browser's local storage, validated as hex values, and reused for stroke and fill. A screen-pick button appears only where the browser provides one.

The toolbar is organized in groups: select and pan, shapes and lines, text and steps, emphasis and hiding, and crop. At widths up to 900 px the shape, text and emphasis groups collapse into menus. **圖層** opens the object list; selecting an object exposes **樣式**, with arrangement and alignment collapsed inside. **更多** holds zoom and source metadata. The **暫停 Agent** button and a separate image-output group with **複製 PNG** and **下載 PNG** sit beside the Send action. Arrows are selected initially and no sidebar opens automatically. The compact horizontal layout follows the draw-first pattern of [CleanShot's annotation UI](https://cleanshot.com/screenshots).

**備註（選填）** opens optional title and notes. Classification, intent, expected/actual results and reproduction steps live under **進階任務細節**. Leave all prose empty to publish the image alone; an empty title becomes `Visual feedback` when published. The intent stays `analyze-only` unless explicitly changed. Privacy review remains mandatory.

A published revision uses the lowest schema version that can describe it. Schema `0.3.0` is used only when a revision contains a highlight, spotlight, blur or pixelate annotation, a curved-arrow control point, or one of the new style fields (`opacity`, `effectSizePx`, `arrowStyle`, `textStyle`, `smoothing`). Otherwise a revision keeps its earlier version: image-only tasks, drawings, and ellipse, diamond and line annotations use `0.2.0`, and older `0.1.0` publications retain their stricter prose contract. Versions `0.1.0` and `0.2.0` reject `0.3.0`-only data, and published records are never rewritten. The CLI and MCP server read all three versions; an older `lensdock` that knows only `0.1.0` or `0.2.0` refuses a `0.3.0` bundle, so update the CLI.

- **複製 PNG** and **下載 PNG** export a flattened annotated image. They do not publish anything and need no Agent. The exported PNG has no editable layer metadata. Copying requires browser clipboard permission and a supported secure context; loopback is supported by the tested Chrome. If the browser denies the permission, the UI shows an error and you can download instead. Only **送出** publishes and queues the image for the Agent.
- Highlight, spotlight, blur and pixelate are cosmetic emphasis, not redaction. Blur and pixelate change how the image looks and may be partly recoverable. Use the opaque redaction tool for secrets. Share previews and exports render redactions above other annotations.
- The editor, the exported PNG and the shared images apply the same cosmetic effects to real image pixels. Solid redaction regions are blacked out before blur or pixelate samples the image and are painted again last, so an overlapping blur cannot leak or lighten them. The shared redacted base already contains blur and pixelate, while spotlight and highlight appear only in the annotated image. As a server resource bound, the combined area that blur and pixelate affect is capped at 100 million pixels.
- Crop creates a new private capture and draft from the source image. It does not copy the old annotations; the UI warns before proceeding. The original remains unless you explicitly choose to delete it.
- Editing published content requires a new revision and a new approval. Existing approved bundles never change in place.

## Connect an Agent

The Gateway endpoint is `http://127.0.0.1:18081`. Token files must remain private, mode `0600`. The following command lists only feedback available to the Agent identity:

```bash
bin/lensdock feedback list --workspace default \
  --api http://127.0.0.1:18081 \
  --token-file .lensdock/product/agent-token
```

For an MCP client, configure a stdio server with this executable and these arguments. Use absolute paths when the client starts outside the repository:

```bash
bin/lensdock mcp serve --workspace default \
  --api http://127.0.0.1:18081 \
  --token-file "$PWD/.lensdock/product/agent-token"
```

The server exposes `feedback_list`, `feedback_get`, `feedback_image`, `feedback_ack` and `feedback_resolve`. Standard output carries MCP protocol messages only. An Agent must actually view an authorized image before acknowledging it with `imageViewed: true`; fetching metadata or pulling a bundle does not acknowledge it.

For browser work, the Agent acquires a lease using `.lensdock/product/agent-token` as `--operator-token-file`, then starts `lensdock cdp bridge`. Bind the lease to the acknowledged feedback ID and revision when working on a handoff. The bridge is loopback-only and uses the existing Chromium process. Never let an Agent launch a replacement browser or connect to an unrestricted Runtime port. Full commands and state transitions are in [Agent integration](docs/AGENT-INTEGRATION.md).

Run browser actions only while the workspace is in `AGENT` mode and not paused. Before acquiring a lease, start `lensdock workspace watch` (see [Choose a usage mode](#choose-a-usage-mode)) and stop all browser work, including the bridge process, when it reports `paused: true` or `mode: "NOVNC"` or when a request fails with 409 `AGENT_PAUSED` or `AGENT_MODE_DISABLED`. After a human resumes, acquire a fresh lease; an old lease is never revived. The bridge renews its lease on its own.

The actual handoff runs used Claude Code 2.1.310 and Codex 0.164.0 through `npx`; OMP's CLI delivery and CDP path were also exercised. Those results do not establish support for every client version or image renderer.

## Build and apply a local update

Rebuild the changed images using the build steps above, and rebuild both host and Linux CLIs if Go code changed. Initialize the backup key against the existing product deployment, then create a uniquely named local release:

```bash
bin/lensdock update init --deployment .lensdock/deployment.json
mkdir -p .lensdock/product/releases .lensdock/product/plans

RELEASE_ID="local-$(date -u +%Y%m%dT%H%M%SZ)"
ARCH="$(mise exec -- sh -c 'printf "%s" "$LENSDOCK_ARCH"')"

bin/lensdock release manifest --dev --uncommitted \
  --id "$RELEASE_ID" \
  --output ".lensdock/product/releases/$RELEASE_ID" \
  --runtime-image lensdock/runtime:dev \
  --gateway-image lensdock/gateway:dev \
  --linux-cli "bin/linux-$ARCH/lensdock" \
  --novnc-version 1.7.0 --agent-browser-version 0.39.2 \
  --gateway-version development --cli-version development \
  --gateway-db 2 --min-gateway-db 1

bin/lensdock update plan --deployment .lensdock/deployment.json \
  --release ".lensdock/product/releases/$RELEASE_ID" --allow-dev \
  --output ".lensdock/product/plans/$RELEASE_ID.json"

bin/lensdock update apply --deployment .lensdock/deployment.json \
  --plan ".lensdock/product/plans/$RELEASE_ID.json" \
  --yes --approve-switch

bin/lensdock update status --deployment .lensdock/deployment.json
```

`--uncommitted` explicitly permits a development build from a dirty working tree. `--allow-dev` is for local development, not production release approval. Return control before updating; do not add forced lease revocation or crash-snapshot flags to routine commands.

The updater retains exact images in their original repositories before maintenance, drains live VNC/SSE connections, shuts Chromium down cleanly, checkpoints the profile and database, migrates cloned data and checks an isolated candidate before switching. The drawing migration advances the Gateway database from schema 1 to 2. Rollback restores the matching old database and profile with the old images; changing only an image tag is unsafe.

```bash
bin/lensdock update checkpoints list --deployment .lensdock/deployment.json

# Set this to an actual checkpoint ID from the list.
CHECKPOINT_ID='cp-...'
bin/lensdock update rollback --deployment .lensdock/deployment.json \
  --checkpoint "$CHECKPOINT_ID" --yes
```

The backup key must be preserved separately from its encrypted checkpoints. Published artifact volumes are retained across rollback; a database rollback can remove later feedback records without deleting their retained files. See [the runbook](docs/UPDATE-RUNBOOK.md) for recovery and [the implementation guide](docs/UPDATE-IMPLEMENTATION.md) for signed release evidence, candidate isolation, retention and cleanup rules.

## Development and verification

```bash
mise run check
mise exec -- pnpm --dir apps/web test
mise run web:build
```

`mise run check` validates the specification, formatting, Go vet and race tests, shell scripts and version pins. It does not replace a running-browser check. `mise run validate`, or `mise exec -- uv run --script tools/validate_spec.py`, checks schema examples, API references and coordinate invariants only. The `uv` invocation installs the script's declared validation dependencies in an isolated environment.

The completed UI checks covered settings and feedback fields at widths 320, 390, 768, 1024, 1280 and 1600 px, both languages and all five font sizes. The editor, dock, diagnostics, share previews and mobile image viewer were also exercised. Editable long input values may scroll inside their controls; this is distinct from clipping labels or putting controls outside the viewport.

The final mode and toolbar build (`local-toolbar-modes-18`) passed 140 layout cases at 320, 390, 768, 900, 1024, 1280 and 1600 px, in zh-TW and en-US at 14 and 20 px. These cover paused `AGENT` settings, workspace and editor, plus `NOVNC` settings and workspace, with no horizontal overflow or overlapping controls. All 14 tools in the grouped menus were selected at 320 px in both languages at 20 px; their menus and style controls stayed within the viewport. The open custom palette also fit all seven widths in both languages. Representative screenshots were visually inspected.

Native macOS Zhuyin produced `你` in the local clipboard field, and the remote Chromium input read back the same UTF-8 bytes after the UI paste. The observed composition start/update events were trusted; the relay-observed composition-end event was synthetic. Japanese and Korean native composition remain untested.

Local demo screenshots and evidence are under `.lensdock/evidence/final-runtime/` and are intentionally excluded from Git. Final captures include `32-final-settings-aligned.png`, `33-final-feedback-fields.png`, `34-final-workspace-cjk.png`, `35-final-drawing-acknowledged.png`, `36-final-published-drawing.png` and `37-final-drawing-mobile.png`. Earlier screenshots can show intermediate states; use the final captures when comparing the layout.

## Documentation

| Document | Purpose |
|---|---|
| [SPEC.md](SPEC.md) | Product requirements and security boundaries |
| [Implementation plan](docs/IMPLEMENTATION-PLAN.md) | Implemented milestones and remaining scope |
| [Acceptance matrix](docs/ACCEPTANCE.md) | Requirement IDs and verification coverage |
| [Validation report](docs/VALIDATION-REPORT.md) | Actual evidence, versions and limits |
| [Agent integration](docs/AGENT-INTEGRATION.md) | Feedback CLI, MCP, leases and CDP |
| [Update runbook](docs/UPDATE-RUNBOOK.md) | Host operations and recovery |
| [Web UI](apps/web/README.md) | Frontend development and interactions |
| [OpenAPI](contracts/openapi.yaml) | Core HTTP contract; coverage limits are stated in the file |
| [Feedback schema](schemas/feedback.schema.json) | Published bundle formats 0.1.0, 0.2.0 and 0.3.0 |
| [References](docs/REFERENCES.md) | Upstream sources and licensing |

LensDock's own public license has not been selected. Upstream components retain their licenses; the implemented development workflow does not grant permission to redistribute all dependencies under a single project license.
