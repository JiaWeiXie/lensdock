# 驗收矩陣

狀態：下列矩陣是測試要求，本身不代表通過。實際執行過什麼、在哪個環境、有哪些限制，記錄在本檔最後的「Evidence status」一節；沒有出現在那裡的 ID 沒有執行紀錄。每次執行必須附 OS／Kernel、Docker、CPU 架構、Image Digest、Browser、noVNC、Agent Client 與測試 Commit。

## A. Runtime 與持久化

| ID | Given / When | Then |
|---|---|---|
| RT-01 | 用核准 Debian slim 基底建置並啟動 | OS 與套件清單可辨識；無 Alpine 基底混入正式 Runtime |
| RT-02 | 非 root Chromium 啟動 | Sandbox 驗證通過；失敗不自動使用 --no-sandbox |
| RT-03 | 關閉 noVNC、CLI、Agent | Chromium 與登入頁維持運作 |
| RT-04 | 測試頁設定持久 Cookie、Local Storage、IndexedDB，正常重建 Container | 三者按測試預期保留；另說明 Session Cookie 不保證 |
| RT-05 | 第二個 Runtime 嘗試可寫開啟同一 Profile | 明確拒絕，不自動刪 Lock |
| RT-06 | Browser 崩潰並重啟 | 新 Generation、舊租約失效，不自動重送最後操作 |
| RT-07 | Agent 在 Container 內測本機 Web App | 使用明確設定的可達地址，不能誤認 localhost 是開發主機 |
| RT-08 | A Runtime starts with `LENSDOCK_MODE` set to `NOVNC` or `AGENT` (case-insensitive, default `AGENT`) or with `supervise --mode`, with and without a saved policy, then restarts and is updated | The variable and the flag are the initial mode only, the flag wins over the variable, and they apply only while no policy has been saved; after that, the saved mode and pause win across restart and update, including a saved pause. The policy sits in a dedicated volume (`/data/runtime`, directory 0700, file 0600, supervisor uid) that Chromium cannot read or write; a corrupt policy file fails startup instead of relaxing it. `lensdock admin product-init --mode` validates the value before creating any file |

## B. 控制權與安全

| ID | Given / When | Then |
|---|---|---|
| CT-01 | 人類與兩個 Agent 同時申請控制 | 最多一個成功，其他得到可解釋衝突 |
| CT-02 | Agent 操作中使用者接管 | 舊連線撤銷後才發 HUMAN；不出現雙寫入窗口 |
| CT-03 | 使用舊 Epoch、過期 Token 或重啟前 Generation | HTTP 與既有 WebSocket 都不能繼續輸入 |
| CT-04 | Viewer 修改前端 viewOnly 或自行傳 RFB Key／Pointer／Clipboard | 後端唯讀路徑不接受寫入 |
| CT-05 | Gateway 斷線、Agent Heartbeat 停止 | Runtime 在 TTL 到期後撤銷，恢復不復活舊租約 |
| CT-06 | 人類輸入 MFA／密碼 | Agent 無有效 CDP；按鍵與剪貼簿不進 Log |
| CT-07 | 無認證、錯誤 Origin、跨 Workspace Token | HTTP／WS／圖片存取都拒絕 |
| CT-08 | Browser 或 Agent 嘗試取得 Docker Socket、管理功能、私有原圖 | 存取失敗並有不含秘密的稽核 |
| CT-09 | 掛起的網頁 Request 在接管後完成 | UI 不宣稱已撤銷網站副作用，保留已知不確定狀態 |
| CT-10 | 偽造只讀 CDP 使用 Runtime.evaluate | 沒有 AGENT 租約完全拒絕泛用 CDP |
| CT-11 | The workspace is in `NOVNC` and every role calls Agent or feedback routes, and opens a stale editor link | Agent lease, target, feedback, bundle and setup requests get 409 `AGENT_MODE_DISABLED`; capture, drawing, feedback and artifact routes are closed to every role while stored data is kept. The UI shows no annotation, drawing, Send, Dock feedback, pause or Agent status, and the editor link goes back to the workspace. Human control, VNC, clipboard and files, the policy state and the `agent/events` stream keep working, and a human can enable `AGENT` in Settings. An unreadable policy is treated as unknown (collaboration hidden, mutations disabled), never as `AGENT` |
| CT-12 | A human pauses while an Agent holds a lease and while a screenshot or feedback is being made, then resumes | One click pauses with no confirmation. The held AGENT lease, its tickets and its CDP sockets are fenced at once, and Agent acquire and renew get 409 `AGENT_PAUSED` with no race against the toggle. A HUMAN lease is kept; the human can capture, draft and Send, and the assigned Agent can still read, ACK and resolve feedback. Resume needs an explicit action (closing a screenshot or the editor does not resume), never revives the old lease or token, never takes a HUMAN lease, and the Agent must acquire a fresh lease with a new epoch. Viewers cannot pause (403), a cookie request without the CSRF token is refused (401), and another workspace's Agent token gets 404. The UI warns that already-dispatched website actions may still finish |
| CT-13 | A human switches `AGENT` to `NOVNC` while an Agent has a CDP connection, a Bridge, an open feedback stream and a policy watch | The CDP connection closes, the Bridge exits, tickets and sockets end, and the feedback stream closes. The policy event stream stays open, and `lensdock workspace watch` emits the state change as JSON lines. The UI says the Agent is disconnected. Notification delivery into a model conversation is not claimed; the server-side fence is what stops the Agent. Already-dispatched website actions are not claimed to be reversed |

## C. CJK／Emoji／輸入

| ID | Given / When | Then |
|---|---|---|
| TX-01 | 開啟 CJK Fixture，截直接 Browser 圖與 noVNC 圖 | 固定字串無缺字方框；TC／SC／JP／KR 符合語言設定 |
| TX-02 | 顯示 Emoji 膚色、旗幟、ZWJ 與 Variation Selector | 固定版本的核准 Corpus 符合預期形狀／色彩 |
| TX-03 | 用本機注音／日文／韓文 IME 輸入並貼入遠端 | UTF-8 逐位元內容一致；組字鍵未觸發快捷鍵 |
| TX-04 | 使用沒有預裝 Noto 的 Client 開啟標註編輯器 | 字型資產或明確後備可用，匯出不因未載入字型而缺字 |
| TX-05 | 複雜 Emoji 旁做游標、刪除與字數限制 | 不截斷 ZWJ／Surrogate，Editor 按 Grapheme 操作 |

## D. Capture／Annotation

| ID | Given / When | Then |
|---|---|---|
| CA-01 | 擷取桌面與指定 Target Viewport | 來源標示正確；桌面含 Browser UI，Viewport 不混入別的分頁 |
| CA-02 | noVNC 畫面失連／過期 | 不可當成新的即時截圖；顯示 Capture 鮮度 |
| CA-03 | 擷取過程重新整理／切 Target | 一致性檢查偵測，重試或明確標記，不連結錯誤 DOM |
| CA-04 | 以 50%、100%、150%、不同 DPR／Letterboxing 編輯 | 同一標註對準相同來源像素，誤差目標 ≤ 1 來源像素 |
| CA-05 | 人類在畫面上畫框／箭頭、Undo、Redo、移動、存草稿 | 標註狀態可重建，沒有操作透傳至遠端 Browser |
| CA-06 | 裁切後再標註 | 新 Capture 或 Parent Transform 完整，舊資料不漂移 |
| CA-07 | 桌面圖交給 Agent | 不把桌面像素直接當 CDP CSS 座標點擊 |
| CA-08 | Agent 在背景頁、使用者看其他頁 | 顯示 Target 差異；未授權時不搶前景 |
| CA-09 | 超大圖、解碼炸彈、非 PNG、過多標註 | 在資源上限內拒絕，Gateway 不耗盡記憶體 |
| CA-10 | AGENT 租約進行中，人類擷取 `desktop` | 租約、Epoch、Agent CDP 連線不變；Agent 不被中斷 |
| CA-11 | `page-viewport` 擷取前持有人租約仍有效 | 進入 CAPTURE 時舊連線關閉；結束後以新 Epoch 還給原持有人，舊 Epoch 重連被拒 |
| CA-12 | `page-viewport` 擷取時原租約已過期或已釋放 | 擷取後回到 `IDLE`，不自動發新租約 |
| CA-13 | Create every advanced tool (highlighter, spotlight, blur, pixelate, filled rectangle, arrow styles filled, open, double and curved, smoothed freehand, seven text styles, step counter), then select, move, resize, undo, save, reload, copy, download and Send | Each object persists and renders. Highlighter, spotlight, blur and pixelate use real image pixels in the editor, the exported PNG and `screenshot.redacted.png` (spotlight only on the annotated image), with the shared integer algorithm. Pixels under a solid redaction stay opaque black after an overlapping blur or pixelate in every output, and blur and pixelate are labeled as not redaction. Documents with new kinds or fields declare schema 0.3.0; documents without them keep 0.1.0 or 0.2.0, and the 0.1.0 and 0.2.0 schemas reject 0.3.0-only data. Blur and pixelate are bounded to 100 million affected pixels server-side. Create or capture, drag an arrow, Send still takes three operations with no Agent ID field |
| CA-14 | Use the custom palette, the toolbar groups and overflow menus, the direct Copy PNG and Download PNG buttons, the Mode panel and the pause banner at widths 1600, 1280, 1024, 768, 390 and 320 px in zh-TW and en-US (at least font sizes 14 and 20 px) | The palette keeps at most 12 validated `#RRGGBB` colors in `localStorage` (add the current color, reuse it for stroke and fill, remove it) and offers no screen eyedropper. Copy PNG and Download PNG are visible next to Send as independent outputs needing no publication or Agent. Every new control is reachable, with no horizontal overflow, clipping or overlap, measured and not only seen. Copy PNG reports the real clipboard permission error honestly |

## E. 隱私與交付

| ID | Given / When | Then |
|---|---|---|
| FB-01 | 在圖中放測試秘密並實心遮蔽 | 分享圖、合成圖、縮圖和附件都不能還原該秘密 |
| FB-02 | 只使用透明框／模糊效果 | 不被視為已完成機密遮蔽；使用者得到正確提醒 |
| FB-03 | 原圖 URL／猜測物件 ID／舊 Revision 存取 | 未授權 Agent 無法取得；發布不暴露私有路徑 |
| FB-04 | Agent 在另一台主機領取 Feedback | 能下載圖片、驗證 Hash、實際讀圖，不依賴 Server 絕對路徑 |
| FB-05 | MCP Client 不支援圖片內容 | 明確失敗／回退，不能回報已看見圖片 |
| FB-06 | 使用者按交付，但 Agent 尚未 Poll | 維持 QUEUED，不冒稱 ACK／完成 |
| FB-07 | 同一 publish／dispatch 請求重送 | 回同一結果，不產生重複工作或半套 Artifact |
| FB-08 | Bundle 含 ../、絕對路徑、Symlink 或錯誤 Hash | CLI 拒絕解包與寫入核准範圍外 |
| FB-09 | 網頁／診斷內容含惡意指令 | 不取得額外工具權限，不執行未核准 Shell 指令 |
| FB-10 | Agent 回報修好但人類未驗證 | RESOLVED，不自動 VERIFIED |
| FB-11 | 發布後修改圖或文字 | 新 Revision、重新遮蔽與核准，舊 Bundle 不原地變更 |
| FB-12 | 只有繪圖、標記或文字標註，標題、描述、預期／實際結果與註解全部留空，發布、派送、下載 | 可發布；標題為 `Visual feedback`，其餘欄位維持空字串，不由圖片推導文字；`schemaVersion` 為 `0.2.0`；預覽綁定、遮蔽與長度／控制字元檢查不變；舊的 0.1.0 Revision 不被改寫 |
| FB-13 | 預設繪圖／截圖、拖曳箭頭、送出 | 三個操作即完成發布與 QUEUED；沒有獨立預覽確認或 Agent ID 欄位；伺服器依 Workspace 預設或唯一 Agent 派送；沒有有效接收者時保留草稿、不部分發布 |
| FB-14 | 送出中雙擊、預覽失敗、發布回應遺失或 ETag 過期 | 雙擊 single-flight；預覽失敗不發布；回應遺失以同鍵重試且僅一筆交付；過期版本拒絕發布、不自動核准新內容 |

## F. 更新與回復

| ID | Given / When | Then |
|---|---|---|
| UP-01 | 檢查有更新 | 只顯示候選／計畫，工作中的 Browser 不突然重啟 |
| UP-02 | 正常更新 | 停止租約、乾淨備份、候選驗證、切換、新 Generation |
| UP-03 | 新 Chromium 已寫入 Profile 後回退 | 使用舊映像 + 更新前 Profile／Keyring／DB，不讀新 Profile |
| UP-04 | 磁碟滿／備份或 Migration 失敗 | 不覆蓋可用資料；保持可恢復狀態 |
| UP-05 | 候選環境擁有登入 Profile | 預設不能對外發請求，不與正式帳號重複操作 |
| UP-06 | 升級 Debian userspace | 不誤稱已更新 Host Kernel／Docker |
| UP-07 | 只有新 Image Tag 而無精確 Manifest | 拒絕正式發行／顯示資訊不完整 |

## G. 完整使用情境

**E2E-01**：登入測試帳號 → 交由 Agent 打開 Fixture → 人工發現 UI 問題 → 接管 → 擷取 → 標註並遮蔽 → 填寫預期／實際／步驟 → 交付 → Agent 明確讀圖並 ACK → 取得租約、重新觀察 → 回傳修正及驗證圖 → 人工 VERIFIED → 執行更新與回退 → 檢查 Profile 與證據仍符合預期。

首版必須在 Codex 與 Claude Code 的至少一個固定版本走完交接路徑，OMP 至少走完 CLI 交接及原生 CDP 控制。任何未測組合明確列為未驗證，不能由其中一個 Client 成功推論所有 Client 成功。

## Evidence status

This section records what has actually been run. The matrices above are requirements. Nothing here says that all acceptance IDs passed, and an ID that is not named below has no recorded run.

### Evidence sources

| Source | Covers | Does not cover |
|---|---|---|
| Spec validator (`tools/validate_spec.py`, [VALIDATION-REPORT.md](VALIDATION-REPORT.md)) | Static checks of the specification package: JSON Schema, example documents, cross-field rules, OpenAPI references and structure, coordinate round trips, font configuration XML, fixture JavaScript syntax | It starts no container and no browser. It is not a browser E2E run and shows no behavior of the product |
| M0 spikes (2026-10-24, [spikes](spikes/)) | Runtime Sandbox, VNC, CJK and emoji, clipboard and CDP bridge on OrbStack arm64 and Ubuntu 22.04 amd64 | The M0 Runtime only. Historical results, not rerun on the integrated product |
| Integrated demo (2026-10-25) | Gateway, UI, editor, agent hand-off, update and rollback on macOS arm64 with OrbStack, `http://127.0.0.1:18081`, development release `local-final-demo-11`, generation 8, Gateway schema 2 | One host, one architecture, an unsigned development release. Evidence files are in `.lensdock/evidence/final-runtime/`, a private directory excluded from version control; do not publish them |
| Mode and toolbar run (2026-10-25) | Release `local-toolbar-modes-16` (generation 13), then managed updates to 17 and 18 (generations 14 and 15): startup, pause, disable and notification checks with a real Agent lease and Bridge; advanced tools through the UI pointer; clipboard/download pixel checks; scoped Agent bundle pull; final responsive checks. Private evidence includes `mode-startup.json`, `mode-flag.json`, `mode16-*`, `toolbar16-*`, `mode17-update-persistence.json` and `mode18-rwd.json` | One host, one architecture, unsigned development releases. Timings are single observations, not an SLA. No model acknowledged the onboarding prompt or a notification; no amd64 product run |

Earlier static checks on 2026-10-25 are saved in private `final-checks.log`. That historical run reported 25 specification tests and 22 API operations; its frontend suite had 75 tests. These figures describe earlier builds.

Final checks for `local-toolbar-modes-18`: the Gateway image built with TypeScript/Vite, all 195 frontend tests passed, and `mise run check` passed 33 specification checks, `gofmt`, `go vet` (host, Linux and Linux e2e), `go test -race -count=1 ./...`, shellcheck and the version-pin check. OpenAPI contains 28 operations. Static and unit checks do not start a browser; the deployed UI evidence is recorded separately below.

### Status by acceptance ID

"Integrated" means the 2026-10-25 demo above. "M0" means the spike runs.

| ID | Status | Evidence and limits |
|---|---|---|
| RT-01 | Integrated and M0 | `rt01/result.txt` has 24 PASS lines for the integrated arm64 image: Debian trixie, no Alpine and no apk, base digest label equal to the Dockerfile pin, every package in `packages.txt` in the dpkg manifest, `chromium-sandbox` absent. M0 passed on both hosts |
| RT-02 | M0, plus an integrated snapshot | Integrated `processes.txt` and `container-boundaries.json`: no Chromium command line has `--no-sandbox`, every process has `NoNewPrivs: 1`, renderers carry 2 seccomp filters against 1 for the browser process, and the Runtime container drops all capabilities and uses the generated seccomp profile. `chrome://sandbox` was not captured again. M0 passed on both hosts |
| RT-03 | M0 only | Viewer and agent shutdown checks ran in SPIKE-02 and SPIKE-04. No integrated run is recorded |
| RT-04 | Partly exercised | Cookie, Local Storage and IndexedDB markers survived the update switch (new containers on cloned volumes) and the paired rollback (`update-six-*.json`, `paired-rollback.json`). A plain container rebuild and the Session Cookie caveat were not tested separately |
| RT-05, RT-07 | No run recorded | |
| RT-06 | M0 partly | SIGKILL of Chromium produced a new Generation and WebSocket close 4002 within 100 ms (SPIKE-04). No integrated run is recorded |
| CT-01, CT-02, CT-05, CT-06, CT-09 | No run recorded | |
| CT-03 | M0 only | WebSocket part, both hosts; the later review changes were rerun on OrbStack only (SPIKE-04) |
| CT-04 | M0, plus one integrated denial | M0: the read-only VNC port ignored forged input on both hosts. Integrated: a viewer identity calling the control endpoint got 403 `LEASE_REQUIRED` (`gateway-denials.json`). Forged RFB writes through the Gateway's noVNC path were not recorded |
| CT-07 | Partly exercised | `gateway-denials.json`: anonymous 401, wrong Origin 403, cross-workspace bearer 404. WebSocket and image routes are not in that file |
| CT-08 | Partly exercised | `container-boundaries.json`: neither container is privileged or mounts a Docker socket. An agent asking for a private original and for a private draft got 403 (`gateway-denials.json`). The audit entries were not checked |
| CT-10 | M0, plus one integrated denial | M0 ran the full status table on both hosts. Integrated: generic CDP with identity only got 403 `LEASE_REQUIRED` |
| TX-01, TX-02 | M0 matrix | CJK and emoji rendering were also exercised in the integrated UI (an agent read back a drawing with a CJK title correctly), but no per-ID matrix was rerun |
| TX-03, UTF-8 data path | Exercised | M0: five cases matched bit for bit on both hosts. Integrated: the Zhuyin round trip below also proves the path from the local field through a Human takeover and Send and paste to a remote textarea |
| TX-03, native Zhuyin IME | Exercised, one input method | `native-ime-probe.json`, result `PASS_ZHUYIN_UTF8_ROUND_TRIP` on `local-final-demo-11`. With the macOS Zhuyin input source selected explicitly, desktop-target keystrokes `s`, `u`, `3`, `Return` produced `你` in the local `#clip-text`. Trusted `compositionstart` and `compositionupdate` events were captured; the `compositionend` the relay observed was synthetic. Then an actual Human takeover and Send and paste put it into a VNC-focused remote textarea, and a fresh Agent lease read the value back independently through CDP: exactly `你`, UTF-8 bytes 228 189 160. The local field was cleared by the send, the Human lease released, the owned tabs closed and the previous foreground app restored |
| TX-03, other IMEs | Not verified | Japanese and Korean native IMEs were not exercised. The Zhuyin run covers one character on one macOS host and input source, and it did not separately check that composition keys fire no shortcut. An earlier probe with window-target keystrokes and implicit input source selection produced ASCII; that was a source selection and routing problem of the test, is not an established product defect, and is superseded. TX-03 as written (Zhuyin, Japanese and Korean) is only partly covered, and nothing here covers other platforms |
| TX-04 | Partly exercised | The UI bundles its fonts (Fontsource packages, notices in `apps/web/public/licenses/`). The downloaded PNG and the clipboard `image/png` had the same SHA-256 and a size of 1920x1080 (`editor-png-export.json`, isolated headless Chrome). A client without Noto fonts installed was not tested separately |
| TX-05 | No run recorded | |
| CA-01 | Partly exercised | Published desktop capture metadata is retained in `.lensdock/evidence/omp-bundle-r1/feedback.json`; the selected-target viewport capture is recorded in `15-targeted-cjk-capture.png` with Runtime page provenance and 1920×993 dimensions. This does not cover every target/navigation race |
| CA-02 to CA-04, CA-07 to CA-10, CA-12 | No isolated run recorded | |
| CA-05 | Partly exercised | `editor-interactions-history.txt` preserves earlier actual pointer checks: Shift multi-selection, group movement, alignment, duplication, group resize, layer ordering, undo/redo, autosave readback, marquee selection and confirmed clear followed by undo restoring all four saved rectangles. The drawing session separately created and published 12 annotations. These runs do not establish exhaustive input-forwarding isolation |
| CA-11 | Partly exercised | `capture-lease-history.txt` and `13-human-page-capture.png` preserve a Human page-capture flow returning to the control stream with Epoch 4 and completed Runtime viewport metadata. An exhaustive stale-epoch reconnect matrix is not established by these UI observations |
| CA-06 | Partly exercised | `drawing-crop.json`: cropping a drawing made a new draft with the drawing provenance kept (`generated-canvas`, no runtime context) and a `parentTransform` of kind `crop`. It carries 0 annotations (annotations are not copied and the UI warns), and the original draft kept its 12. Desktop and viewport crops were not recorded |
| FB-01 | Partly exercised | Opaque redaction is rendered last, and the agent described the solid opaque redaction in the drawing. That a test secret cannot be recovered from the share image, composite, thumbnail and attachments was not checked |
| FB-02, FB-05, FB-07, FB-08, FB-09 | No run recorded | No MCP client without image support was tried |
| FB-03 | Partly exercised | Agent requests for a private original and a private draft got 403. Guessed object IDs and old revisions were not recorded |
| FB-04 | Partly exercised | Claude Code 2.1.310 (model `claude-opus-5-5`) called `feedback_get`, `feedback_image` (annotated) and `feedback_ack` with `imageViewed: true`, and described the drawing correctly; the server shows `ACKNOWLEDGED`, not resolved. An earlier repair flow also used Codex CLI 0.164.0 through `npx` (the installed Codex had an unrelated dyld hang) and the OMP CLI with CDP. Everything ran on the same host as the Gateway; a second host and hash verification were not itemized |
| FB-06 | Partly exercised | A published result revision stayed `QUEUED` and was not reported as read |
| FB-10 | No isolated run recorded | |
| FB-11 | Partly exercised | In the repair flow the first revision was cancelled and a revised second revision was verified by the Human. Published revisions stayed unchanged across the update and rollback runs |
| FB-12 | Live publish/download and automated tests | On `local-annotation-ux-12`, the UI created `fb_0ad427d34f27c4951cde3bdbb08c7e26` with no title or task prose, drew an arrow, text, rectangle and uncommented pin, reviewed the server images and published r1. The published title is `Visual feedback`; description, expected/actual and comments remain empty, intent is `analyze-only`, schema is `0.2.0`. The rebuilt CLI pulled and hash-verified the bundle using the admin identity; state remains READY, not dispatched or acknowledged. Evidence: `annotation-ux-published.json`, `annotation-ux-bundle/`, screenshot `40-visual-only-published.png`. Go race tests cover all four kinds and client version validation; live Agent dispatch/read was not repeated |
| FB-13, FB-14 | Live UI/API and automated tests | Three-operation drawing and desktop flows reached QUEUED on `local-three-step-demo-15`, generation 12. Preview 503, double-click, lost publication response and a real concurrent draft update were exercised; details below. Configured-recipient scoping and missing/ambiguous defaults are covered by Go tests. |
| UP-01, UP-07 | No standalone run recorded | Plan files exist for the exercised releases, but no evidence file isolates `check`, an incomplete release directory or a bare image tag |
| UP-02 | Exercised with development releases | Several updates applied; `local-final-demo-10` and `local-final-demo-11` passed all 8 candidate checks |
| UP-03 | Partly exercised | The paired rollback restored the old images with the pre-update Profile, keyring and database. No exercised release changed the Chromium version, so a rollback after a Chromium upgrade was not demonstrated |
| UP-04 | Partly exercised | Only a failed candidate Runtime (`/bin/false`) was injected, with live VNC and SSE open; the old release and its data were restored. Disk full, a failed backup and a failed migration were not injected |
| UP-05 | Partly exercised | The candidate ran on an internal network without published ports and passed the egress check. No real account was involved, so no double-action case exists |
| UP-06 | Stated, not tested | The plan states that the host kernel and Docker Engine are unchanged. No host update was performed |
| E2E-01 | Parts exercised separately | The repair flow (publish, dispatch, agent read and acknowledgment, Agent lease and CDP repair, result screenshot, Human reject, revised revision, `VERIFIED`) and the update and rollback runs were exercised in separate runs with a fixture instead of a real account. There is no single continuous run, and no login with a test account is recorded |
| RT-08 | Exercised on one host | `mode-startup.json`, `mode-flag.json`: `LENSDOCK_MODE=NOVNC` started in `NOVNC`; after enabling `AGENT` and pausing, a Docker restart kept `AGENT` paused (revision 3) although the initial variable was `NOVNC`; `--mode AGENT` over `LENSDOCK_MODE=NOVNC` started in `AGENT`. `mode17-update-persistence.json`: a managed update to release 17 kept the production policy (paused `AGENT`, revision 6, same timestamp), and the isolated candidate used its own scratch policy. By Docker exec the browser user (uid 10001) could not read `agent-control.json`; uid 10000 saw directory 0700 and file 0600 owned by 10000:10000. `product-init --mode` and the flag-over-variable rule are covered by automated tests. A corrupt policy and a failed write were not injected live; the volume enrollment and the pre-mode rollback limitation are in `UPDATE-RUNBOOK.md` |
| CT-11 | Exercised on one host | `mode16-disconnect.json`, `mode16-denials.json`: Agent lease, target, feedback, bundle and setup requests returned 409 `AGENT_MODE_DISABLED`; a deep editor URL redirected to the workspace; capture, drawing, feedback and pause controls were absent; Human acquire and release still worked. On release 18, `NOVNC` settings/workspace passed 56 layout cases, no collaboration-feed request was issued during the run, and the actual 1920×1080 desktop connected view-only |
| CT-12 | Exercised on one host | A 120 s Agent lease, `lensdock cdp bridge` and a live `Browser.getVersion` (Chrome 156.0.8078.47). One-click pause closed CDP with 4001 within 24 ms, the Bridge exited with 3, and `workspace watch` showed revision 2 paused. Agent acquire got 409 and a feedback list got 200. Under the pause a Human acquired HUMAN, made a screenshot draft, and Send auto-queued it without resuming; after release, explicit resume (revision 3) invalidated the old capability (401) and a fresh lease worked. `mode16-auth.json`: viewer pause 403, cross-workspace Agent state 404, cookie POST pause without CSRF 401 with revision 5 unpaused and unchanged. Pause during a pending page capture, the acquire-versus-toggle race and restart persistence are covered by Go tests, not by a live run. The ack and resolve side of an assigned Agent while paused was not run live |
| CT-13 | Exercised on one host | Switching off with another live CDP connection closed it within 50 ms, the Bridge exited with 3, the open feedback SSE ended, and the policy watch stayed open and saw `NOVNC` at revision 4. `mode16-watch.txt` shows `AGENT` revision 1, pause 2, resume 3, `NOVNC` 4, `AGENT` 5. These are single-run timings, not an SLA. No model received or acknowledged a notification; the onboarding prompt (4585 characters, no credentials, placeholder token path) was verified only as clipboard text equal to the displayed text |
| CA-13 | Exercised on one host | `toolbar16-bundle`, `toolbar16-pixels.json`, `toolbar-clipboard.png`: on a 1920×1080 drawing, the pointer created highlight, spotlight, blur, pixelate, solid redact overlapping blur, a filled rectangle, curved, open and double arrows, smoothed freehand and seven text styles; a curve control was dragged, undo and redo worked, and all 17 annotations saved and reloaded. Clipboard versus download, and published annotated versus download, had 0 differing pixels (compressed clipboard bytes differ because Chrome re-encodes); the solid redaction region had 0 non-black pixels in the clipboard image, download and both shared images; blur and pixelate regions matched between the redacted base and the annotated PNG. Send auto-queued once, and the scoped Agent CLI pulled a hash-verified bundle with schema 0.3.0 without ACK. The annotated PNG was viewed (styles, effects, CJK); the SVG editor was inspected visually, with no exact SVG pixel comparison. Clipboard copy needed the read, write and sanitized-write permissions, and the denied-permission error was honest. Legacy 0.1.0 and 0.2.0 handling, the 100 million pixel bound and field validation are covered by Go and frontend tests, not a live run. The step counter and every text style were not individually re-pulled by an Agent |
| CA-14 | Exercised on one host | Custom color `#1286ab` survived save/reload; frontend tests cover valid `#RRGGBB` and the 12-color bound. `mode18-rwd.json`: 140 deployed cases at 320/390/768/900/1024/1280/1600 px, zh-TW/en-US, 14/20 px, across paused `AGENT` settings/workspace/editor and `NOVNC` settings/workspace; no horizontal overflow or control overlap. All 14 grouped tools were selected at 320 px/20 px in both languages, with menus and style controls inside the viewport. The open palette fit all seven widths in both languages at 20 px. Representative screenshots were visually inspected |
| FB-02 (0.3.0 addendum) | Partly exercised | The editor, the Gateway and the docs say blur and pixelate are not redaction, and a solid redaction placed over a blur stayed fully black in every output (0 non-black pixels). That a known test secret cannot be recovered from blur or pixelate alone was not tested, and the warning text was not compared across languages |

### Other exercised behavior without an acceptance ID

- Standalone drawing: the Human creates a blank PNG capture (64 to 4096 px per side) without a Runtime or lease, draws, publishes feedback schema 0.2.0 and dispatches it. The database upgrade and its paired rollback are recorded in [UPDATE-IMPLEMENTATION.md](UPDATE-IMPLEMENTATION.md#exercised-evidence-and-limits).
- Downloads: a Chromium download landed in `/data/downloads` and stayed after the CDP session detached (`download-policy.json`, one 23-byte file).
- Responsive UI (`responsive-ui.json`, `feedback-field-rwd.json`, `feedback-field-production.json`; final screenshots `32-final-settings-aligned.png` to `37-final-drawing-mobile.png`, all in the private evidence directory): the settings dialog and the feedback form each passed 60 cases (widths 1600, 1280, 1024, 768, 390 and 320 px, font sizes 12, 14, 16, 18 and 20 px, zh-TW and en-US) with no misalignment and no overflow. The dock (3 panels, 6 widths, English at 20 px), the diagnostics tabs (320 px, English at 20 px), the editor (6 widths, both languages at 20 px), the share preview (6 widths, with real server images) and the drawing dialog (320 px at 20 px) also passed. These runs used the Vite preview against the live Gateway; the production image was rechecked only for the feedback form extremes (en-US at 20 px and zh-TW at 14 px, at 1280 and 320 px) after `local-final-demo-11`. Native single-line editable values scroll inside their bounded input by design, so text is not claimed to be untruncated everywhere. At 1280x800 and 14 px the connected VNC canvas filled 75.3245% of the viewport and the frame 77.9047%; the 16:9 source is letterboxed, not cropped or distorted.
- Final screenshots (read visually after the last run): 32 settings, 33 feedback fields, 34 the workspace with CJK content and the connected canvas at 75.3245% of a 1280x800 viewport, 35 the drawing in the `ACKNOWLEDGED` state, 36 the full published share-image modal, and 37 the drawing modal at 320x568 with the whole image and the close button visible and no horizontal overflow. Screenshot 30 shows an intermediate misalignment that was fixed and is not proof.
- Annotation UX revision (`local-annotation-ux-12`, deployment generation 9, same macOS arm64 host): compact horizontal toolbar, no automatic sidebar, contextual style controls and optional collapsed notes/task details. The deployed editor passed 24 layout cases: six widths (1600/1280/1024/768/390/320), two font sizes (14/20), both languages, with no document or button overflow (`annotation-ux-rwd.json`). The expanded advanced form and More tools popover also fit at 320 px / English 20. Expanding, editing, collapsing and reopening the expected-result field preserved its value; it was cleared again before the prose-free publication. Screenshots `38-annotation-toolbar.png`, `39-annotation-mobile.png`, `40-visual-only-published.png` were visually inspected. Deployed verification recorded no browser errors. The development hot-reload run required a reload after locale-module replacement; production assets were then verified directly.
- Checks for the annotation UX revision: the final Gateway image built with TypeScript/Vite; all 75 existing frontend tests passed; `go test ./...` passed; feedback and delivery race tests passed; `gofmt -l` was empty; the regenerated specification report passed 28 tests. The deployment completed its candidate checks and `/readyz` returned `{"ready":true}`. These results do not expand the earlier platform or native-IME coverage.
- Four-operation flow and local demo (`local-four-step-demo-13`, then the mobile-layout correction in `local-four-step-demo-14`, generation 11): fresh browser entry required no token and produced a scoped `demo-human` session. Drawing `fb_97be0ca58e0e1bc349d2d2275857cd36` and desktop `fb_096f4627a66d39327d044256d9ef7822` each reached READY using exactly four UI operations: entry button, arrow drag, preview, explicit safety-confirm publish. The final mobile run published `fb_a16e42c6c866e7a13d6a73ab50a8e9fe` through the same four operations. No form, checkbox checklist, preview-build button or success-dismissal click was needed. READY remains unassigned, not Agent acknowledgment. Evidence: `four-step-demo.json`, `41-four-step-preview.png`, `42-four-step-mobile.png`.
- Failure paths: a synthetic preview-upload 503 prevented publication; Retry recovered. A concurrent draft edit then made confirmation fail with stale ETag rather than publishing unreviewed content (`four-step-preview-safety.json`). A synthetic desktop-draft 503 retained the uploaded capture; retry issued only the draft POST, not another capture or image upload. A 640×360 custom drawing was also created and its disposable draft removed.
- Demo safety: the live session cookie was HttpOnly, SameSite=Strict, with no expiry; anonymous API and missing-CSRF requests returned 401, foreign Origin returned 401, wrong Host returned 421. Explicit logout stayed signed out after reload and token-free re-entry worked. Gateway race tests simulated a 30-day-old demo session remaining valid while an expired normal token session was denied; no real 30-day idle run was performed. Default mode, remote-origin rejection and the session cap are covered by tests. Network deadlines, Agent credentials and control lease TTLs remain enabled.
- Final entry layout: 24 deployed cases (1600/1280/1024/768/390/320 px, 14/20 px fonts, zh-TW/en-US) kept both creation buttons visible with no horizontal overflow or owner-status overlap. A visual check caught the initial 320 px English/20 overlap missed by the original overflow-only measurement; the final run checks overlap explicitly. The final Gateway image passed TypeScript/Vite; 75 frontend tests, Go tests, Gateway race tests and all 28 spec checks passed. These checks do not extend platform coverage beyond the existing arm64 demo.
- Three-operation send (`local-three-step-demo-15`, generation 12): drawing `fb_f51e26a552c1ddecb87398c422931ed2` and desktop capture `fb_381bfe74be0d51b59f6ac156148f5e1c` each used entry, arrow drag and Send to reach QUEUED for the configured workspace Agent. A 390×844 mobile drawing (`fb_3e283c14cd7b4806e281669cd3ed4725`) passed the same three operations. No preview confirmation, task form or recipient field appeared. An existing READY revision (`fb_a16e42c6c866e7a13d6a73ab50a8e9fe`) queued from its Send button with only `{revision:1}` in the dispatch request and no modal. The Agent CLI pulled and hash-verified the first drawing bundle with its scoped token; pulling did not acknowledge it.
- Send failure checks: a double click produced one preview request; an injected preview 503 prevented publication. Retry recovered, then an injected lost response after a successful real publish left the editor retryable. The next click reused the publish key and returned the same revision with one queue transition. In a separate run, a concurrent PATCH caused the real publish to return 412; the updated draft remained DRAFT and the published endpoint returned 404. Delete was ignored while sending. The disposable stale draft was removed and the retry fixture cancelled after verification.
- Single-send editor layout: 24 deployed cases (1600/1280/1024/768/390/320 px, 14/20 px, zh-TW/en-US) had no horizontal overflow, clipped controls or caption/button overlap; Send remained visible. Screenshots `43-three-step-editor.png`, `44-three-step-queued.png`, `45-three-step-mobile.png` and `46-three-step-mobile-drawing.png` were visually inspected. The Gateway image passed TypeScript/Vite, 75 frontend tests passed, and `go test -race ./...`, focused Go vet and all 28 specification checks passed. Evidence is in `three-step-send.json`; these results do not extend platform or native-IME coverage.
- Expanded toolbar and modes (`local-toolbar-modes-16`, then releases 17 and 18): see RT-08, CT-11 to CT-13, CA-13 and CA-14. Both managed updates preserved paused `AGENT` revision 6 and its timestamp. Release 18 supplies the final toolbar, mode-panel and pause-banner layout evidence; earlier responsive measurements remain historical.

### Not verified

- Native Japanese and Korean IMEs, other macOS input sources and other platforms.
- Signed and scanned release distribution; every exercised update used an unsigned development release.
- An integrated run on amd64. The M0 amd64 results are valid for M0 only.
- M4 optional surfaces.
- A continuous E2E-01 run with a test account, a rollback after a Chromium version change, a second agent host, and an MCP client without image support.
- Failure injection beyond a failed candidate Runtime: disk full, an interrupted download, a failed backup, a failed migration on the clone, a host restart during an update and a deliberate candidate network leak.
- One client succeeding does not imply the others: Claude Code, Codex and OMP were exercised through different paths.
- A model reading the onboarding prompt or a notification, with a Client acknowledgment; the policy write failure and a corrupt policy file on a live system; rolling the policy back to a release from before the mode feature (see the runbook).

### Re-running

`mise run check` runs the static checks (spec validator, `gofmt`, `go vet`, Go tests with the race detector, `shellcheck`, pin check) and writes no report. The M0 integration tasks (`e2e:host`, `e2e:sidecar`, `e2e:rt01`, `e2e:sandbox-matrix`, `e2e:vnc-cpu`, `e2e:agent-browser`, `e2e:version-matrix`) need a running Docker engine. The frontend checks are `mise run web:build` and `pnpm test` in `apps/web`. No mise task reproduces the integrated demo, so its results are not repeatable by any command listed here. Keep evidence files in the private directory and out of version control.
