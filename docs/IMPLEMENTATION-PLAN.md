# 實作計畫

本文件記錄各階段的範圍、出口條件與目前的實作與驗證狀態。狀態只代表下列列出的證據，不是正式認證，也不是「所有驗收 ID 全數通過」。沒有列出證據的項目視為未驗證。不提供未經估算的工時或日期承諾。

## 狀態總覽（2026-10-25）

| 階段 | 實作 | 驗證範圍 |
|---|---|---|
| M0 | 完成 | OrbStack（macOS arm64）與 Ubuntu 22.04 amd64；amd64 證據只適用於 M0 |
| M1 | 已實作 | macOS arm64／OrbStack 的整合環境 |
| M2 | 已實作 | macOS arm64／OrbStack；真實 Claude Code、npx Codex 與 OMP CLI 交接 |
| M3 | 已實作（開發版 Release） | macOS arm64／OrbStack；Release 未簽署，非 production |
| 0.3.0 增量：使用模式、Agent 暫停、進階標註工具 | 已實作（開發版 Release `local-toolbar-modes-16`，generation 13；後續 release 17，generation 14，保留暫停策略） | macOS arm64／OrbStack；新控制項的 RWD（CA-14）尚未驗證；沒有 amd64 產品 E2E。見下方「使用模式與進階標註工具」 |
| M4 | 未實作 | 不在範圍內 |

目前整合版本沒有完整的 amd64 產品 E2E，不能由 M0 的 amd64 結果推論 M1–M3 在 amd64 上通過。

## M0 — 先驗證高風險條件

| 任務 | 產出 | 必須測試 | M0 狀態（2026-10-24） |
|---|---|---|---|
| SPIKE-01 Debian Chromium Sandbox | 最小 Container、主機需求、固定版本矩陣 | 非 root、有 Sandbox；禁止自動降級 | 完成：userns＋自訂 Seccomp，[報告](spikes/SPIKE-01-sandbox.md)、[ADR-0002](adr/0002-runtime-identity.md)、[ADR-0003](adr/0003-chromium-sandbox.md) |
| SPIKE-02 Display／雙 VNC | Xvfb、Window Manager、讀／寫入口 | 唯讀入口偽造輸入仍無效果 | 完成；amd64 CPU 規則觸發，決定維持雙實例（理由見報告），[報告](spikes/SPIKE-02-display-vnc.md) |
| SPIKE-03 CJK／Emoji／UTF-8 輸入 | Fontconfig、Fixture、Clipboard 路徑 | 文字／Emoji 顯示及貼入一致 | 完成（資料路徑）；實際 IME 組字留給 M1（macOS 注音結果見 M1），[報告](spikes/SPIKE-03-text-input.md) |
| SPIKE-04 CDP Bridge | HTTP Discovery 與 WebSocket Prototype | agent-browser／OMP，連線撤銷與 URL 重寫 | 完成；Bridge 預設根路徑（agent-browser、OMP 皆可用），`--path-secret` 為嚴格模式，[報告](spikes/SPIKE-04-cdp-bridge.md) |
| SPIKE-05 Capture／標註 | noVNC 公開擷取 API、圖片座標 | 縮放／Letterboxing／DPR 與字型匯出 | 未作為獨立 Spike 執行；對應功能已在 M2 實作（見下） |

出口：每項有實際版本、測試方式及結果。尚不允許用 Prototype 處理正式敏感帳號。

M0 在 OrbStack（macOS arm64）與 Ubuntu 22.04 amd64 都跑完 RT-01、RT-02、RT-03、CT-03（WebSocket 部分）、CT-04、CT-10、TX-01、TX-02、TX-03（資料路徑），全部通過。這是 M0 的結果，不延伸到 M1–M3。開發環境以 `mise` 管理，常用指令見 `mise tasks`。

## M1 — Runtime 與控制安全

範圍：Runtime Supervisor、Volume Lifecycle、單一 Profile Lock、認證 Gateway、Runtime 身分、租約狀態機、Epoch／Generation、Heartbeat、人工接管、唯讀入口與 Browser Health，加上精簡 UI。

實作現況：

- Gateway（`cmd/gateway`、`internal/gateway`）正常模式以 Token 配對換取 Cookie Session，角色為 viewer、human、agent、admin，並限定單一 Workspace。Session 刻意只存在記憶體，正常模式在 Gateway 重啟後需重新配對；本機展示模式自動重新進入。
  - 本機展示可選用 `LENSDOCK_DEMO_NO_AUTH=1`（預設關閉、僅限 loopback Origin、單一 Workspace），讓同源瀏覽器免貼 Token 以 `demo-human`（human）進入；Demo Session 無伺服器端到期，Cookie／CSRF 與控制租約仍保留，設定與限制見 [README](../README.md#local-demo-without-token-entry)。
- Human／Agent 租約帶 Epoch 與 Runtime Generation；人工接管先排空舊控制者，逾時才可明確強制。VNC 控制 WebSocket 與剪貼簿、解析度需要有效 HUMAN 租約，Agent 只走租約限定的 CDP。
- Runtime 為 Debian slim 上的 Headful Chromium（156.0.8078.47），Profile 持久化，Chromium Sandbox 維持啟用。CDP 與 noVNC 面對同一個瀏覽器。只有主機 Admin CLI 取得 Docker Socket。
- Web UI（`apps/web`）預設精簡：頂部工具與右側 Dock 預設收合，介面字級 12–20 px 可選，RWD 行為見 [apps/web/README.md](../apps/web/README.md)。
- UTF-8 剪貼簿以明確動作貼入遠端欄位；受控上傳／下載；SSE 事件、稽核與指標診斷。

出口條件的驗證：人工登入與 Agent 在同一瀏覽器交替操作、關閉視窗不關 Browser、Gateway 或 Agent 中斷後舊控制權失效，已在上述整合環境演示。

已知限制：

- 原生 IME（TX-03）：只有 macOS 注音通過。在明確選定注音輸入法、以桌面為目標送出 `s`、`u`、`3`、Return 後，本機 `#clip-text` 得到「你」，並記錄到可信的 `compositionstart`／`compositionupdate`；接著以真實 HUMAN 接管與「送出並貼上」貼入聚焦的遠端文字區，再由全新的 AGENT 租約經獨立 CDP 讀回，值恰為「你」（UTF-8 位元組 `[228,189,160]`）。限制：Relay 觀察到的 `compositionend` 是合成事件；早先以視窗為目標、未明確選定輸入法的探測只得到 ASCII `su3`，該結果已由上述探測取代，原因是來源選擇與路由，不是產品缺陷；日文與韓文原生 IME 未測，因此不宣稱所有平台或 TX-03 所有組合通過。UTF-8 資料路徑、CJK 與 Emoji 顯示、上傳與下載已獨立驗證。
- 沒有目前整合版本的 amd64 產品 E2E。

## M2 — 完成視覺交接閉環

範圍：桌面／Viewport Capture、來源與一致性 Metadata、標註編輯、遮蔽、分享預覽、Feedback Revision、Bundle、CLI 與 MCP 圖片交付。資料結構保留日後擴充多圖的空間，目前只支援單一圖片。

實作現況：

- 擷取：桌面擷取不需租約；Viewport 擷取針對明確 Target，記錄一致性並在必要時警告會中斷 Agent；獨立繪圖（`source: drawing`）由 Gateway 建立 64–4096 px 的白色畫布，不連 Runtime、不需租約。繪圖的 Provenance 為 `generated-canvas`，`runtimeGeneration` 為空，`targetId`／`navigationId` 為 null，沒有 `page`，`consistency` 為 `unknown`。
- 編輯器自建，不使用 tldraw 或 Excalidraw。工具含矩形、橢圓、菱形、直線、箭頭、自由筆、文字、編號 Pin、不透明遮蔽，並支援多選、框選、群組移動、縮放控制點、複製、六種對齊、四種圖層指令、單一與批次樣式、自動保存、Undo／Redo 與可復原的全部清除。裁切建立新草稿（不複製標註，介面明確警告）。可複製或下載攤平的 PNG（不含可編輯資料）。
  - 上一行的工具與快捷鍵是 0.2.0 的基準狀態（歷史記錄）。0.3.0 新增的螢光筆、聚光、模糊、馬賽克、箭頭／文字樣式、平滑自由筆、自訂色盤與獨立 PNG 按鈕見下方 0.3.0 增量一節，不要由本行推論。
- 發布：預覽使用伺服器重新編碼並覆寫遮蔽後的兩張圖；發布後 Revision 與 Artifact 不可變。標題、描述、預期／實際結果與標註註解都是選填，只有繪圖與標記也能發布；標題留空時發布為固定的 `Visual feedback`，其餘文字原樣保留（空的維持空，不由圖片推導）。`schemaVersion` 為 `0.2.0` 的條件是 Capture 來源為 `drawing`、標註含橢圓、菱形、直線、任務沒有描述、或 `bug`／`visual-regression` 任務缺預期或實際結果，且 `feedback.json`、`manifest.json` 等文件必須一致；其餘 Revision 維持 `0.1.0`，那個版本的 Schema 仍要求這些文字。
- 交付：`dispatch` 只排隊；被指派 Agent 以自己的身分明確 `ack`；取得 AGENT 租約（`lease acquire --feedback-id`）才進入 IN_PROGRESS；`resolve` 只到 RESOLVED，VERIFIED 只有人類能產生。CLI 與 MCP 的 `list`、`get`、`pull`、`feedback_image` 都不是 ACK。細節見 [AGENT-INTEGRATION.md](AGENT-INTEGRATION.md)。

驗證證據（macOS arm64／OrbStack 整合環境）：

- 完整修復流程使用真實 Claude Code 與 npx 固定版本的 Codex 0.164.0 實際讀圖與 ACK，Agent 租約經 CDP 修復後回傳結果截圖；人類退回後產生新 Revision，最後 VERIFIED（第一版 CANCELLED）。已安裝的 codex 有與本專案無關的 dyld 掛起，因此改用 npx 固定版本。OMP 的 CLI 與 CDP 路徑也執行過。
- 標註畫布以 Claude Code 2.1.310 驗證：UI 以指標建立 12 個標註並發布，明確派送給 Agent；Agent 呼叫 `feedback_get`、`feedback_image`（annotated）與 `feedback_ack`（`imageViewed: true`），對圖片的描述正確（紅框、線、琥珀色填色橢圓、箭頭、菱形、CJK 標題、折線、Pin 與實心遮蔽），伺服器端狀態為 ACKNOWLEDGED。該回合沒有 resolve，也沒有宣稱任何瀏覽器動作。
- 結果回報 Feedback 為 QUEUED，不等於已被讀取。

已知限制：Claude Code、Codex、OMP 之外的 Client 與其他版本未驗證；MCP 協定沒有圖片能力旗標，是否真的看到圖要靠整合測試。`page-full`、多圖、回歸比較屬 M4，未實作。

## M3 — 發行與日常維運

範圍：Release Manifest、依賴鎖、SBOM、來源證明、映像掃描、受控更新、Profile／DB Checkpoint、候選 Volume、成對回退、磁碟配額與指標。

實作現況：

- `lensdock release manifest|keygen|sign|promote|merge|verify` 與 `lensdock update init|check|plan|apply|approve|abort|rollback|status|checkpoints|branches|compose`。正式頻道要求 SBOM、Provenance、掃描報告與簽章；開發頻道（`--dev`）缺少時只出警告。髒工作樹需 `--dev --uncommitted`。
- Release 宣告 Gateway DB schema（`--gateway-db 2 --min-gateway-db 1`）。Gateway 遷移 1 保持不變，遷移 2 允許產生的畫布（含嚴格的無 Runtime 檢查）。更新先對 DB 複本做 migration，再啟動候選；回退還原 schema 1 的 DB 與舊 Binary，不讓舊 Binary 開啟 schema 2 的 DB。
- 更新維護前以同一原始 Repository 的 `:lensdock-keep-<完整 digest>` Tag 固定舊映像並驗證 digest 與平台；計畫為唯讀，保留失敗在維護前即中止，回退固定目前與更新前的 Checkpoint。
- Runtime 在 SIGTERM 前先以 `Browser.close` 乾淨關閉，處理 SQLite `PERSIST` 日誌，不直接刪除 Profile 日誌。Gateway 追蹤並取消 SSE 與升級連線，Shutdown 先排空再關 DB。候選環境的 Artifact 為唯讀，正式 Gateway 必須可寫 Artifact，憑證在正式容器唯讀。

驗證證據（macOS arm64／OrbStack）：多次真實更新、成對回退與 Migration；回退後更新前的 Cookie、localStorage、IndexedDB 還原，更新後新增的資料消失，已發布 Feedback 不變、更新後建立的 Feedback 回 404，保留的 Artifact Hash 一致。DB 1→2 升級與回退後，舊 Gateway 對新繪圖回 404 且既有 Feedback 相同。刻意壞掉的候選 Runtime（`/bin/false`）在 VNC 與 SSE 連線中失敗，並安全還原到舊的健康版本；後續兩次更新通過全部 8 項候選檢查。映像保留以實際 Docker 驗證。

已知限制：

- 全部演示使用未簽署的開發版 Release，不是 production 簽署或掃描過的發行；production 簽署、SBOM 與映像掃描流程未被驗證。
- 沒有目前整合版本的 amd64 產品 E2E。
- Web UI 只顯示更新與回退的主機指令，不會套用更新。

最低要求維持不變：M0–M3 每項出口條件都通過、且沒有未標示的限制，才可標示為正式可用。上列限制尚未解除，因此目前狀態是「已實作並在單一環境演示」，不是正式發行。

## 使用模式與進階標註工具（0.3.0 增量，`local-toolbar-modes-16`）

Status: implemented and exercised on one host (macOS arm64 with OrbStack, 2026-10-25). The feature set was exercised on development release `local-toolbar-modes-16` (generation 13, applied with the real `lensdock update` commands); a follow-up full managed update to release 17 (generation 14) is recorded at the end of this section. Releases are unsigned development releases. This section replaces no earlier evidence; older results in this file and in `docs/ACCEPTANCE.md` stay historical.

### Current feature map

| Feature | Where it lives | Commands and entry points |
|---|---|---|
| Initial usage mode | `internal/supervisor` (config, startup), `cmd/runtime` | `LENSDOCK_MODE` set to `NOVNC` or `AGENT` (case-insensitive, default `AGENT`); `lensdock-runtime supervise --mode NOVNC` or `--mode AGENT` overrides it; `lensdock admin product-init --mode NOVNC` or `--mode AGENT` writes `LENSDOCK_MODE` into `compose.env` and the flag overrides the variable. Used only until a policy is saved |
| Authoritative mode and pause | `internal/lease` manager, `internal/runtimeapi`, `internal/apiwire` (`AgentControl`) | Same lock as acquire, authorize and renew. Policy file `agent-control.json` (0600) in `/data/runtime`, a dedicated volume `lensdock-runtime-policy` (supervisor uid 10000, mode 0700; Chromium uid 10001 cannot write) |
| Gateway gating and routes | `internal/gateway`, `contracts/openapi.yaml` | `GET` and `PUT /api/v1/workspaces/{id}/agent`, `POST .../agent/pause`, `POST .../agent/resume`, `GET .../agent/events` (SSE), `GET .../agent/setup`; errors `AGENT_MODE_DISABLED`, `AGENT_PAUSED` |
| Agent notification | `cmd/lensdock/workspace_watch.go` | `lensdock workspace watch <workspace> [--api URL] [--token-file F]` prints mode and pause changes as JSON lines |
| Agent lease and Bridge | existing | `lensdock lease acquire <workspace> --mode agent ... --output LEASEFILE`, `lensdock cdp bridge --lease-file F` |
| Mode and pause UI | `apps/web/src/agent/` (`AgentControlProvider`, `AgentControlButton`, `AgentModePanel`, `EditorRouteGuard`, `messages.ts`), Settings, TopBar, WorkspaceView | Details in [apps/web/README.md](../apps/web/README.md) |
| Advanced annotation tools | `apps/web/src/editor/` (`effects.ts`, `effectsCanvas.ts`, `raster.ts`, `shapes.ts`, `drawStyle.ts`, `customPalette.ts`, `EditorPanels.tsx`), `internal/feedback`, `schemas/feedback.schema.json` | New kinds `highlight`, `spotlight`, `blur`, `pixelate`; arrow, text and freehand style fields; feedback schema 0.3.0 |
| Update handling of the new volume | `internal/update`, `docker/` Compose, `product-init` | The new retained dataset must be declared; the read-only plan admits it without rewriting the active generation. See `docs/UPDATE-RUNBOOK.md` and `docs/UPDATE-IMPLEMENTATION.md` |

Checks to run (not run by this document): `mise run check` (spec validator, `gofmt`, `go vet`, `go test -race`, `shellcheck`, pin check), `mise run web:build`, and in `apps/web` `pnpm typecheck` and `pnpm test`. The frontend suite now has 195 tests; the 75 quoted in older evidence entries is the count at those earlier releases and is historical, not a current figure.

### Exercised on `local-toolbar-modes-16` (single host, bounded evidence)

- Build and checks: both Debian images and the host and Linux arm64 CLIs built. `go test -race ./...` passed for all Go packages, with further Gateway regression tests; `go vet` passed for the host, Linux and Linux e2e builds. TypeScript passed and 195 frontend tests passed.
- Startup mode (`mode-startup.json`, `mode-flag.json`): an isolated container with `LENSDOCK_MODE=NOVNC` started in `NOVNC` at revision 1. After enabling `AGENT` and pausing, a Docker restart kept `AGENT` paused at revision 3 although the initial variable was `NOVNC`. An Agent acquire returned 409 `AGENT_PAUSED`, and after switching off 409 `AGENT_MODE_DISABLED`. A fresh container with `LENSDOCK_MODE=NOVNC` and `--mode AGENT` started in `AGENT`. The owned probe containers and anonymous volumes were removed.
- Onboarding prompt: the English text copied to the real clipboard matched the displayed text exactly (4585 characters). It has no credentials. It needs an installed `lensdock` CLI and a private approved Agent token file (placeholder path).
- Real Agent path: a 120 s Agent lease, `lensdock cdp bridge` and a live WebSocket `Browser.getVersion` returned Chrome 156.0.8078.47. One click on Pause closed the CDP socket with code 4001 within 24 ms (Bridge exit status 3), and `lensdock workspace watch` showed revision 2 paused. An Agent acquire got 409 while paused and a feedback list returned 200. A Human lease acquired during the pause created a screenshot draft, and Send auto-queued it without resuming. After the human released, an explicit resume (revision 3) invalidated the old capability (401) and a fresh lease worked. Switching the mode off with another live CDP connection closed it within 50 ms, the Bridge exited with status 3, the open feedback SSE ended, and the policy watch stayed open and saw `NOVNC` at revision 4. These are single-run timings, not a service-level objective.
- `NOVNC` denials (`mode16-disconnect.json`, `mode16-denials.json`): Agent requests for leases, targets, feedback, bundle and setup returned 409 `AGENT_MODE_DISABLED`. A deep editor URL redirected to the workspace, and the capture, drawing, feedback and pause controls were absent. Human acquire and release still worked. The mode was set back to `AGENT` (revision 5) for later checks.
- Watch output (`mode16-watch.txt`): `lensdock workspace watch` printed `AGENT` revision 1, pause 2, resume 3, `NOVNC` 4 and `AGENT` 5.
- Authorization (`mode16-auth.json`): a viewer pausing got 403; an Agent token for another workspace reading Agent state got 404; a same-origin cookie POST to pause without the CSRF token got 401 and revision 5 stayed unpaused.
- Tools and output (`toolbar16-bundle`, `toolbar16-pixels.json`, `toolbar-clipboard.png`): through the UI pointer, a 1920×1080 drawing got highlight, spotlight, blur, pixelate, solid redact overlapping blur, a filled rectangle, three arrow variants (curved, open, double), smoothed freehand and seven text styles. A curve control was dragged, Undo and Redo were used, and a custom color `#1286ab` was added; all 17 annotations saved and reloaded. Copy to the real clipboard and Download PNG matched pixel for pixel (0 differing pixels; compressed bytes differ because Chrome re-encodes). The published annotated image and the download had 0 differing pixels. The solid redaction region had 0 non-black pixels in the clipboard image, the download and both shared images. Blur and pixelate regions matched between the redacted base and the annotated PNG. Send auto-queued once and the scoped Agent CLI pulled a hash-verified bundle with schema 0.3.0; no ACK was performed. The annotated PNG was viewed visually (styles, effects, CJK looked right) and the SVG editor was inspected visually; no exact SVG-to-PNG pixel comparison is claimed. Clipboard copy needed the clipboard read, write and sanitized-write permissions in the isolated browser, and a denied permission showed an honest error.

### Not verified for this increment

- Policy persistence beyond the two local managed updates to releases 17 and 18, and rollback to a Runtime from before the mode feature (see `docs/UPDATE-RUNBOOK.md`).
- Any amd64 product run, a signed or scanned release, and production security review.
- A model actually receiving the notification or following the onboarding prompt, and any Client acknowledgment of it. LensDock enforces the stop with the server-side fence, not through the model.
- A failed policy write was not injected on the live system. By design the live session stays fenced but the request returns an error, and after a restart the saved state may be older.
- Already-dispatched website actions are not reversed by pause or disable.
- Blur and pixelate are cosmetic, not secure redaction; only solid redact is.

### Follow-up: policy persistence across a managed update (release 17, generation 14)

A full managed update to release 17 (generation 14) completed on the same host while the production workspace was in `AGENT`, paused, at revision 6. After the update the policy had the same mode, pause flag, revision and timestamp (`mode17-update-persistence.json`). The candidate passed its checks with an isolated scratch policy in `AGENT` mode; the production policy was retained. Through Docker exec on the host, browser uid 10001 could not read `/data/runtime/agent-control.json`, and uid 10000 saw the directory as 0700 and the file as 0600, owned by 10000:10000. See [policy-volume enrollment and rollback limits](UPDATE-IMPLEMENTATION.md#adding-the-runtime-policy-volume) before upgrading an existing installation.

### Final layout and mode checks (release 18, generation 15)

The final Gateway build passed TypeScript/Vite. `mise run check` passed 33 specification checks, Go formatting, vet and all-package race tests, shellcheck and version pins; all 195 frontend tests passed. The managed update preserved paused `AGENT` revision 6 with its original timestamp.

`mode18-rwd.json` records 140 deployed cases: seven widths (320, 390, 768, 900, 1024, 1280, 1600), zh-TW/en-US, 14/20 px fonts, paused `AGENT` settings/workspace/editor and `NOVNC` settings/workspace. No horizontal overflow or control overlap was measured. All 14 grouped tools were selected at 320 px/20 px in both languages; menus and style controls fit. The open palette fit all seven widths in both languages. Representative screenshots were visually inspected. `NOVNC` made no collaboration-feed requests during the run and the real 1920×1080 desktop connected view-only.

## M4 — 選配能力

全頁／多圖、Console／Network 診斷、跨版本圖片比較、錄影、多人觀察、選配遠端 IME、更多平台與架構。每項以需求與安全風險排序，不把它們塞進 MVP。目前都未實作。

## 程式目錄

```text
apps/web/                 React UI, noVNC adapter, self-built annotation editor
cmd/gateway/              Authenticated gateway (sessions, leases, feedback, SSE, migrations)
cmd/runtime/              Browser runtime supervisor and API
cmd/lensdock/             Host/admin/agent CLI (lease, cdp bridge, feedback, mcp, update, release)
internal/auth/            Operator/lease secrets, private file handling
internal/apiwire/         JSON error envelope and request decoding
internal/apiclient/       Runtime API client and lease file
internal/gateway/         Edge: identities, runtime proxy, lease registry, streams, system status
internal/lease/           Epoch, TTL, generation state machine
internal/cdp/             Discovery rewrite, WebSocket relay, method policy, bridge
internal/vnc/             x11vnc instances and RFB probe
internal/clipboard/       Lease-gated X clipboard via xclip
internal/supervisor/      Process tree, privilege drop, sandbox verification
internal/runtimeapi/      HTTP routes of the runtime
internal/capture/         Page-viewport capture and consistency
internal/feedback/        Captures, drafts, revisions, redaction, delivery state machine
internal/delivery/        Agent-side Gateway client and verified bundle pull
internal/mcp/             MCP stdio server for feedback tools
internal/storage/         SQLite and object storage
internal/transfer/        Controlled upload/download area
internal/update/          Release manifests, planning, checkpoints, rollback
internal/netx/            Shared network predicates
docker/browser/           Runtime image, compose, seccomp profile
docker/gateway/           Gateway and offline UI image
tools/                    Spec validator, seccomp generator, release scripts
tests/e2e/                Browser, sandbox, VNC, text and agent integration
```

API 型別在 `apps/web/src/api/types.ts` 與 `internal/apiwire`；沒有獨立的 `packages/contracts` 套件，也沒有 SDK。
