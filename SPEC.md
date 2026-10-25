# LensDock — 人機協作瀏覽器工作站規格書

| 項目 | 定義 |
|---|---|
| 文件版本 | 0.2.0 / 規格與實作對照；0.1.1 修訂啟動順序、擷取租約語意、剪貼簿主路徑、Feedback 狀態介面、CDP Bridge 安全條件；0.2.0 加入獨立繪圖來源、Feedback Schema 0.2.0、進階標註編輯器、精簡預設介面與字級／RWD 行為，並把 M0 限定的敘述改為 M0–M3 實作現況 |
| 文件版本 0.3.0 補充 | Adds two usage modes (`NOVNC` and `AGENT`) with a persisted Agent pause (§9.4), the expanded annotation toolbar with highlighter, spotlight, blur, pixelate, arrow and text styles, smoothed freehand and a custom palette (§8.1, §11.4), feedback schema 0.3.0 (§12.1), and PNG copy and download that are independent of publication. Acceptance IDs RT-08, CT-11 to CT-13, CA-13 and CA-14 are defined in `docs/ACCEPTANCE.md`; evidence status stays in that file and in `docs/IMPLEMENTATION-PLAN.md`. This table row describes requirements and does not claim that they have all been verified |
| 建立日期 | 2026-10-24，Asia/Taipei |
| 專案名稱 | **LensDock** |
| Repository 建議名稱 | `lensdock` |
| 定位 | 可持久化、可觀察、可人工接管、可標註交接的 Agent Browser Workspace |
| 核心組合 | Debian slim + Headful Chromium + noVNC + agent-browser |
| 主要使用者 | 使用 Codex、Claude Code、OMP 的開發者 |
| 本次交付 | 規格、設計決策、API 契約、資料 Schema、測試規劃與規格驗證工具，以及 M0–M3 的整合實作（Gateway、Runtime、Web UI、CLI、MCP、更新與回退）。整合實作只在 macOS arm64／OrbStack 環境演示，Release 為未簽署的開發版；**不是通過正式認證或簽署的 production 產品**，已知限制見 `docs/IMPLEMENTATION-PLAN.md` |

本文的「必須」是驗收要求，「建議」是預設設計，「可選」是不阻擋初版交付的擴充。所有效能數值是待驗證目標，不是實測結果。功能版本 v0.1／v0.2／v1.0 是本專案規劃，不是上游版本。

## 1. 名稱與產品主張

**LensDock：看見、圈出、交接，讓 Agent 在同一個瀏覽器繼續工作。**

Lens 代表畫面觀察與問題定位；Dock 代表瀏覽器身分、持久化資料及工具的停靠點。這是工作名稱，尚未進行商標、網域或套件名稱權利確認。

使用者不應只得到「把瀏覽器裝進 Container」的工具，而應得到一個工作閉環：人工登入 → Agent 操作 → 即時觀察 → 人工接管 → 截圖標註 → 交付問題 → Agent 修正／驗證 → 人工確認。

## 2. 上游專案與採用邊界

| 專案 | 在 LensDock 的角色 | 採用方式 |
|---|---|---|
| [jlesage/docker-chromium](https://github.com/jlesage/docker-chromium) | 參考 GUI Container、持久化 Profile、CDP 接入及服務管理設計 | **架構參考，不直接繼承映像，不整包 Fork** |
| [vercel-labs/agent-browser](https://github.com/vercel-labs/agent-browser) | Coding Agent 的瀏覽器操作 CLI | 固定經驗證版本，經 LensDock 的受控 CDP Gateway 使用 |
| [noVNC/noVNC](https://github.com/novnc/noVNC) | 自訂遠端桌面介面的 RFB Client | 使用公開 RFB API，外層 UI 自行實作 |

jlesage 目前 Dockerfile 使用 Alpine 基底，因此「直接 FROM jlesage/chromium」不符合 Debian slim 要求。本專案需重新組裝 Runtime。agent-browser 的 CDP 控制與 noVNC 的人工桌面共用同一個 Chromium Process，而不是另開一個隱藏瀏覽器。[R01–R05]

上游授權標示、引用方式與查核連結見 `docs/REFERENCES.md`。初版不複製上游應用程式碼；日後若複用，必須記錄來源檔案、Commit、修改內容與授權文件。

## 3. 問題、範圍與非目標

### 3.1 必須解決的問題

| ID | 問題 | 要求 |
|---|---|---|
| G-01 | Agent 干擾本機日常瀏覽器 | 使用獨立 Browser Runtime Container 與專用 Profile |
| G-02 | 登入、MFA、瀏覽器設定無法完全自動化 | 透過自訂 noVNC 介面操作真正的 Headful 桌面 |
| G-03 | 重建 Container 就遺失狀態 | Profile、下載及工作證據分別持久化 |
| G-04 | 無法看見 Agent 做了什麼 | Live View、控制者狀態、作用中 Target 及稽核事件 |
| G-05 | 文字無法精準描述視覺 Bug | 定格截圖、標示、遮蔽及結構化回饋 |
| G-06 | 人與 Agent 同時操作互相干擾 | 伺服器強制的單一寫入者租約與接管流程 |
| G-07 | 更新難、回退容易壞 Profile | 不可變映像、乾淨備份、候選驗證與成對回退 |
| G-08 | Some users want only a remote browser, not Agent collaboration | A startup usage mode `NOVNC` (browser only) or `AGENT` (default). `NOVNC` hides and also disables every Agent and feedback surface on the server; a human can switch in Settings (§9.4) |
| G-09 | A human must be able to stop the Agent at once, for example while taking a screenshot or writing feedback | One-click pause and explicit resume that fence the Agent without leaving `AGENT` mode and without blocking the human feedback flow (§9.4) |
| G-10 | Marking a screenshot needs more than boxes and arrows | Highlighter, spotlight, blur, pixelate, arrow and text styles, smoothed freehand and a custom palette, plus PNG copy and download that need no publication and no Agent (§11.4) |

### 3.2 預設使用情境

第一個完整版本服務單一信任範圍內的開發者／小團隊。每個 Workspace 對應一個 Browser Runtime、一個身分與一份 Profile；多個 Agent 可以輪流工作，但同一 Workspace 同時只有一個控制者。主機先以 Linux Docker Engine + Compose 為驗證基準；Docker Desktop、amd64／arm64 必須各自通過測試，不能只因為能 Build 就宣告支援。

### 3.3 非目標

不自建 LLM 推理框架；不保證繞過網站驗證；不保證 Cookie 永久有效；不把 noVNC 當成瀏覽器 DOM API；不做公開多租戶 SaaS；不保證無中斷更新；不在初版提供任意主機 Terminal 或 Docker 管理權限；不保證 DRM、硬體視訊加速或高幀率影音測試等同實體桌面。

## 4. 技術選型

| 層級 | 建議選型 | 理由與限制 |
|---|---|---|
| Runtime 基底 | `debian:trixie-slim`，Release 鎖定 Digest | 目前 Debian stable 為 13 / trixie；不使用會跨大版移動的 `stable-slim`。[R06–R07] |
| 瀏覽器 | Debian Security 倉庫的 Chromium 套件 | 瀏覽器與系統函式庫一起驗證；不得宣稱永遠等同 Google Chrome 最新版。[R08] |
| 顯示 | Xvfb + Openbox + D-Bus Session | 初版採 X11 路徑；保留完整瀏覽器 UI 與彈窗 |
| 遠端桌面 | x11vnc + noVNC RFB Library | 完整桌面共享；唯讀與控制使用不同後端入口 |
| 前端 | Vite + React + TypeScript strict | 自訂協作介面，不修改 noVNC 內部 Canvas 實作 |
| 標註編輯器 | 自建 React／SVG 編輯層 + Canvas 點陣化匯出；不使用 tldraw、Excalidraw 等白板 SDK | 矩形、橢圓、菱形、直線、箭頭、文字等維持可編輯結構；編輯器匯出的 PNG 不含可編輯中繼資料；字型隨產品打包 |
| Gateway / Runtime Supervisor | Go | 處理 HTTP、WebSocket、租約、CDP 代理、Process 管理；降低 Runtime 相依 |
| Browser 操作 | 固定版本 agent-browser；OMP 原生 CDP 作相容路徑 | 不另啟 browser-use Agent；Agent 仍在原有執行環境 |
| 儲存 | SQLite + 檔案物件儲存 | 初版單 Gateway；多副本、共享 SQLite、不可靠網路檔案系統不是支援部署 |
| Agent 交接 | REST + CLI + MCP Tools | MCP 圖片內容與檔案下載互為補充；不假設工具能推送到任意現有對話 |
| 測試 | Go tests、前端單元測試、Browser E2E、Container Smoke Tests | 契約、控制權、圖片安全、更新及視覺測試分開執行 |

正式 Runtime 映像及 Gateway 映像均以 Debian slim 建置。Node／Go 編譯工具放在多階段 Builder，不要求留在正式 Runtime。實際 Go、Node、前端依賴及 noVNC 版本由首次相容測試後鎖定，不在規格中虛構可用版本。

## 5. 部署架構與信任邊界

### 5.1 服務分工

**Gateway Container** 提供 UI、API、MCP、認證、Workspace 對應、租約、回饋儲存及稽核。**Browser Runtime Container** 提供 Chromium、虛擬桌面、VNC 與受認證的 Runtime Agent。**主機端 Admin CLI** 才能操作 Docker／Compose 與映像更新。

預設每一套部署一個 Gateway，可配置一個或多個事先建立的 Workspace。新增／移除 Runtime 在初版由主機 CLI 管理；不讓 Web UI 透過 Docker Socket 任意建立容器。

| 路徑 | 外部可達性 | 規則 |
|---|---|---|
| Gateway UI / API / WebSocket | 預設只綁主機 Loopback；遠端需 TLS + 身分驗證 | UI、REST、事件、noVNC 使用同源路徑 |
| Gateway → Runtime Agent | 私有網路，雙向認證 | Runtime 身分與 Workspace 綁定；禁止任意代理目標 |
| Runtime → Chromium CDP | Runtime 內部 Loopback，建議 9223 | 不映射主機連接埠 |
| Runtime → VNC Control | Runtime 內部 Loopback，建議 5900 | 僅有效 HUMAN 租約能使用 |
| Runtime → VNC View-only | Runtime 內部 Loopback，建議 5901 | 獨立 `-viewonly` x11vnc 實例；所有觀察者走此路徑 |
| Agent → CDP | Gateway 的租約限定入口或本機 CLI Bridge | 不取得原始 Runtime CDP 位址 |
| Gateway → 更新主機 | 無常駐管理連線 | UI 只能顯示更新資訊；套用需主機 Admin CLI |

VNC 的 `-viewonly` 是後端能力，不能只用前端 `RFB.viewOnly` 宣告已完成權限控制。兩個 VNC 實例共用同一個 Display，各自輪詢 Framebuffer，需在啟動與負載測試中驗證 CPU 成本；若 M0 量測不可接受，可改為單一 x11vnc 以 `-viewpasswd` 區分唯讀連線，但仍必須是後端強制，不得退回只靠前端 `viewOnly`。[R05, R09]

### 5.2 不可破壞的架構條件

1. 任一時間最多一個有效寫入者；重新連線不能復活已撤銷租約。
2. 關閉 noVNC、CLI 或 Agent 不得終止持久化 Chromium。
3. Profile 僅被其 Runtime 掛載為可寫；Gateway 與 Agent 不掛載 Profile。
4. 真實 CDP／VNC 入口不向 Agent 或公網暴露；Gateway 掛掉時，Runtime 在租約失效後停止接受輸入。
5. 回饋發布、Agent 讀取、Agent 開始執行是不同事件，不能以「已送出」暗示 Agent 已處理。
6. 管理權與瀏覽器操作權分離；瀏覽器工作負載無 Docker Socket、主機 HOME 或主機根檔案系統掛載。
7. Usage mode (`NOVNC` or `AGENT`) and Agent pause are policy owned by the Runtime lease manager. They are separate from the control state in §9.1 and are enforced on the server; hiding a button is never the control. A policy that is missing, unreadable or corrupt fails closed (§9.4).

## 6. Runtime、生命週期與資料持久化

### 6.1 啟動順序

建立／檢查 Volume 權限 → 主機相容與 Sandbox Preflight → D-Bus → Xvfb → Window Manager → VNC 唯讀／控制服務 → Chromium → CDP Ready Probe → Runtime Health Ready。VNC 先於 Chromium 啟動，讓人可以在 noVNC 看到 Chromium 啟動失敗、Profile 修復或崩潰對話框。Health Ready 必須同時確認 Chromium 程序存活、CDP `/json/version` 可回應及兩個 VNC 入口可連線，缺一不得宣告 Ready。

Chromium 採 Headful 模式及明確的 `--user-data-dir=/data/profile`，不使用暫存 Profile；UI 關閉不關閉瀏覽器。退出時先撤銷控制權，再給 Chromium 正常關閉的時間，確認程序結束後才進行備份。僅在確認沒有任何有效瀏覽器程序且取得 Profile Lock 時，才能清理意外殘留的 Singleton Lock；不可啟動時無條件刪除。

### 6.2 持久化邊界

| 位置 | 寫入者 | 保存內容 | 更新規則 |
|---|---|---|---|
| Runtime `/data/profile` | 單一 Chromium | Cookies、Local Storage、IndexedDB、Extensions、Preferences 等磁碟狀態 | 更新前乾淨快照 |
| Runtime `/data/downloads` | Chromium；受控檔案服務 | 使用者授權的下載 | 獨立保存、配額與清除 |
| Runtime `/data/keyring` | 相同 Runtime 身分 | 如選用系統金鑰環所需資料 | 與 Profile 成對備份；密鑰分離 |
| Runtime `/data/runtime` | Runtime supervisor only | `agent-control.json` with the usage mode, pause flag and revision | Dedicated volume `lensdock-runtime-policy`, directory mode 0700, file mode 0600, owned by the supervisor user (uid 10000). Chromium (uid 10001) cannot write it. It is a retained dataset: an existing installation must declare the new dataset and volume before an update (see `docs/UPDATE-RUNBOOK.md`) |
| Gateway `/data/db` | Gateway | 租約、Workspace、回饋及工作狀態 | DB Migration 與備份 |
| Gateway `/data/artifacts` | Gateway | 遮蔽後可分享圖像與附件 | 不隨映像刪除 |
| Gateway `/data/private` | 僅授權人員／服務 | 暫存原始截圖與診斷草稿 | 預設發布後清除；未發布草稿自最後修改起 TTL 24 小時，到期前 UI 明確警示，不得無聲刪除使用者仍在編輯的底圖 |
| Browser `/tmp`、X11 Socket、IPC | Runtime | 執行暫存 | 不備份、不跨版本重用 |

Profile 保留不代表所有登入狀態都能跨重啟存活；Session Cookie、服務端撤銷、MFA 與金鑰環相容性必須用實際網站驗證。禁止把「所有瀏覽器資料都保存」解讀成保存 RAM、JS Heap、所有 Session Storage 或網站伺服器狀態。

初版以非 root 固定 UID/GID 執行應用服務，Volume 權限由部署階段準備，不在每次啟動遞迴 `chmod 777`。不把 `--password-store=basic` 或 `--no-sandbox` 當預設。登入憑證即使存在加密機制，仍視整份 Profile 為高敏感資料。

## 7. CJK、Emoji 與文字輸入

### 7.1 顯示要求

基礎套件至少包含 `fontconfig`、`fonts-noto-core`、`fonts-noto-cjk`、`fonts-noto-color-emoji`、`locales`，與固定 Chromium 套件共同建置。這些字型套件提供 CJK 字型與彩色 Emoji；實際字形覆蓋範圍仍依固定版本而定，不保證全部 Unicode 字元。[R10–R12]

系統採 UTF-8；預設瀏覽器介面與應用語言 `zh-TW`，時區 `Asia/Taipei` 可覆寫。Fontconfig 對繁中設 TC 優先；SC／JP／KR 測試頁以語言屬性選用適當字形，不強迫所有 CJK 都用 TC 替代。建置時重建 Font Cache，Health Diagnostics 顯示已安裝字型版本及 `fc-match` 結果。

字型需覆蓋兩個不同執行環境：Container 內 Chromium 的網頁與瀏覽器 UI；使用者端 LensDock 標註介面的文字及 PNG 匯出。不可假設在 Container 裝字型，就能改變使用者筆電上的標註 Canvas。前端必要字型資產需在產品建置時依授權自行打包／提供，離線可用，不依賴第三方 CDN；本規格包不附字型檔。

### 7.2 輸入要求

「看得到中文」不等於「noVNC 能直接使用注音輸入法」。初版提供本機 UTF-8 文字輸入區，支援使用者本機 IME 組字完成後，以明確動作貼入遠端選取欄位。x11vnc 的 RFB ClientCutText 只支援 Latin-1，不支援 noVNC 的 Extended Clipboard 偽編碼，因此預期 VNC 剪貼簿無法承載 CJK；M0 仍須實測並記錄結果，但設計以具租約檢查的 Runtime X11 Clipboard Bridge 為主路徑，VNC 剪貼簿只在實測通過時作為 ASCII 快捷路徑。不得靜默退化成亂碼或逐個 ASCII Key 模擬。

剪貼簿預設手動同步，不自動讀取整個本機剪貼簿，也不把剪貼簿內容記入 Log。人類輸入／剪貼簿 Bridge 需 HUMAN 租約。v0.2 可加入 Container 內 fcitx5 / 中文輸入法選配，不作為初版顯示支援的前置條件。

實作現況：本機 UTF-8 輸入區加「送出並貼上」已實作（需 HUMAN 租約，上限 64 KiB，不記錄內容）。原生 IME 只驗證了 macOS 注音：明確選定注音輸入法並以桌面為目標送出按鍵後，本機輸入區得到「你」並記錄到可信的 `compositionstart`／`compositionupdate`；以真實 HUMAN 接管貼入聚焦的遠端文字區後，由全新 AGENT 租約經獨立 CDP 讀回的值恰為「你」（UTF-8 `E4 BD A0`）。限制：Relay 觀察到的 `compositionend` 是合成事件；日文與韓文原生 IME 未測，因此不宣稱所有平台或 TX-03 所有組合通過。

### 7.3 驗收字串

使用 `tests/fixtures/cjk-emoji.html` 檢查繁中、簡中、日文、韓文、一般 Emoji、膚色修飾、旗幟、Variation Selector 與 ZWJ 家庭／職業組合。比較直接 Browser Screenshot、noVNC 畫面與標註匯出圖片，不能只檢查套件安裝成功。UTF-8 儲存／傳輸不正規化或截斷使用者原文；文字長度限制及游標移動需考量 Grapheme Cluster。

## 8. 自訂 noVNC 介面

### 8.1 頁面組成

頂部顯示 Workspace、Browser 版本、連線狀態、控制者、目標分頁及「接管／交還／截圖」。主區域為遠端桌面。右側為回饋與 Agent 交接面板。底部可展開稽核事件、健康狀態與已授權診斷。設定頁顯示 Profile、儲存用量、語言、更新候選與最後備份狀態。

UI 預設提供 zh-TW，字串抽離以支援 en-US。工具名稱、操作說明、快捷鍵與錯誤訊息一致；不能只靠顏色區別控制者與連線狀態。所有快捷鍵必須可停用，且不得在文字組字期間搶走 IME Key Events。

預設介面採精簡配置：頂部顯示 Workspace、連線與控制者狀態及「新增繪圖」「截圖標註」，兩個建立入口在手機也直接可見；進階頁面擷取、語言等工具由「展開工具」開啟。新增繪圖直接開啟 1600×900 草稿，自訂尺寸在旁邊的選項；截圖標註一次完成桌面擷取與建立草稿。右側 Dock 預設收合為圖示列。全域快捷鍵為 Alt+Shift+K（控制）、C（桌面截圖標註）、F（全螢幕）、M（Fit）、D（Dock），可在設定停用，並且在輸入欄、IME 組字與遠端桌面取得焦點時不攔截。

標註編輯器採繪圖優先：參考 CleanShot 的精簡橫向工具列，常用工具直接顯示，預設箭頭；顏色、線寬與字級依工具或選取物件呈現。編輯時不顯示 Browser 控制資訊列，仍持有 HUMAN 租約時保留交還警告。備註與側欄預設不開啟；圖層、樣式、排列／對齊、匯出、縮放與來源資訊依需要展開。標題、備註與進階任務欄位皆為選填，畫布上的繪圖、標記或文字即可送出；畫布為檢視面，送出旁明示隱私核准，伺服器分享圖仍需產生與驗證。

Toolbar tools (feedback schema 0.3.0): the editor toolbar is a compact horizontal strip with five groups. Select and pan (V, H); Shapes and lines (rectangle R, filled rectangle F, ellipse O, diamond D, line L, arrow A, freehand P); Text and steps (text T, step counter N); Emphasis and hiding (highlighter M, spotlight S, blur B, pixelate I, solid redact X); Crop (C). Line and crop are directly reachable. The visible name of the pin tool is "Step counter" (步驟編號); its wire kind stays `pin`. Filled rectangle is a preset of the ordinary rectangle kind, not a new wire kind. At 900 px and below, the shapes, text and emphasis groups collapse into grouped menus instead of a large side panel. Shortcuts match physical keys and never fire during IME composition or text entry. Controls shown depend on the selected tool or object: color, line width, fill, text size, opacity (highlighter), effect size (blur, pixelate), arrow style (filled, open, double, curved with a draggable curve handle), text style (plain, outline, label, shadow, mono, bold, underline) and smoothing (freehand).

Custom palette: the user can add the current picked color, reuse it for stroke and fill, and remove it. It lives only in the browser `localStorage` (key `lensdock.editor.palette.v1`), holds at most 12 `#RRGGBB` values, and every value read back is validated. There is no screen eyedropper. Copy PNG and Download PNG sit in the editor toolbar next to Send as independent outputs: they need no publication, no Agent and no draft state change, and the toolbar says so. The default arrow flow stays three operations (create or capture, drag, Send) with no Agent ID field and no extra confirmation.

預設箭頭回饋的三個操作是：開啟繪圖／截圖、拖曳標記、送出。按送出即核准當下可見畫布，系統鎖定編輯、儲存草稿、產生並上傳預覽，再以相同 ETag 發布及排隊；不另開預覽確認或要求填 Agent ID。伺服器使用該 Workspace 已設定的預設 Agent，或唯一符合角色與 Workspace 的 Agent；設定缺少或有歧義時保留草稿，發布與排隊皆不發生。雙擊只發起一次流程，回應不明時保留冪等鍵重試；過期 ETag 不發布，須由人類載入最新版本後再核准。成功直接顯示回饋，不需關閉成功對話框。選其他工具增加一個操作，多次編輯與進階設定不包含在此基本操作計數中；已排隊不等於 Agent 已讀取或執行。

Usage mode in the UI: Settings has a Mode panel. A human or administrator switches between `NOVNC` and `AGENT`; a viewer sees the state only. In `NOVNC` the workspace shows only the remote browser and its ordinary human controls (takeover, clipboard, files, resolution). New drawing, Capture and annotate, the feedback Dock, the editor, Send, the pause control and every Agent status are hidden, and a stale editor link returns to the workspace. The Settings panel still lets a human enable `AGENT`. In `AGENT` the Settings panel shows a read-only English onboarding prompt with a one-click Copy button, fetched from `GET /api/v1/workspaces/{workspaceId}/agent/setup`. The prompt contains no token, session cookie or lease, and names its prerequisites: an installed `lensdock` CLI and a private Agent token file that the user provisions through a private channel (the prompt shows only a placeholder path). Whether a model receives or follows the prompt is not guaranteed by LensDock.

Pause in the UI: a prominent Pause and Resume control is in the workspace top toolbar, in Settings and in the editor toolbar, and is hidden in `NOVNC`. Pause takes effect on one click with no confirmation. While paused a banner says that the Agent is waiting, that resuming needs an explicit human action (closing a screenshot or the editor does not resume), and that website actions already dispatched may still finish. Switching to `NOVNC` says in plain text that it disconnects the Agent. The UI reads the policy from the server only (workspace reads plus the `agent/events` stream so other tabs follow). When the policy is missing, malformed or unreachable, collaboration surfaces are hidden, mutations are disabled, and nothing is restored from `localStorage`. An editor that is already open keeps its unsent edits during a transient unknown state but cannot Send.

介面字級可選 12、14、16、18、20 px（預設 14，存在瀏覽器 `localStorage` 的 `lensdock.fontSize`），只改 LensDock 介面文字，不改遠端桌面或匯出圖片。RWD 以實際介面驗證：設定頁與回饋表單在 1600、1280、1024、768、390、320 px 寬、五種字級、zh-TW 與 en-US 下對齊且無溢出；Dock 三個面板、診斷三個分頁、編輯器、分享預覽與新增繪圖對話框也在多種寬度檢查過。寬度 720 px 以下的稽核表格改成標籤／值卡片，Popover 掛在 `body`、以視窗為界並在內部捲動，成對欄位使用字級相對的自動欄寬並在窄螢幕改為單欄。可編輯的單行輸入欄遇到超長內容時在欄內捲動，不是把所有文字無條件完整攤開。預設 1280×800、字級 14 時，遠端畫面約占視窗 75%，來源為 16:9 時留白而不裁切、不變形。這些結果只代表目前整合環境的受測組合，不代表所有瀏覽器與裝置。

RWD status (CA-14): release `local-toolbar-modes-18` passed 140 deployed cases at 320, 390, 768, 900, 1024, 1280 and 1600 px, in zh-TW/en-US at 14/20 px. The cases cover paused `AGENT` settings, workspace and editor plus `NOVNC` settings and workspace, with no horizontal overflow or overlapping controls. All 14 grouped tools were selected at 320 px/20 px in both languages; their menus and style controls fit. The open palette fit all seven widths in both languages. Representative screenshots were visually inspected. These are desktop Chromium viewport checks, not native mobile-device tests.

### 8.2 功能分期

| 分類 | v0.1 必須 | v0.2 / v1.0 擴充 |
|---|---|---|
| 基本連線 | 連線／斷線、有限重試、狀態、全螢幕、縮放、Fit、唯讀監看 | 多 Workspace 檢視與個人配置 |
| 輸入 | 鍵鼠、滾輪、按鍵釋放、手動文字／剪貼簿、常用組合鍵 | 遠端 IME 選配、進階觸控與裝置相容性 |
| 顯示 | 畫質／壓縮控制、實際解析度標示、失連提示 | 遠端解析度調整；須通過 X Server Capability Probe |
| 協作 | 控制權標示、接管、交還、觀察模式、作用中 Target 警告 | 多人觀看、操作時間線與細化角色 |
| 檔案 | 受控上傳／下載目錄、配額與明確下載動作 | 批次傳輸；非任意主機檔案瀏覽 |
| 問題回報 | 截圖、標註、遮蔽、表單、發布、交付狀態 | 多張圖片、完整頁面、比較與回歸報告 |
| 診斷 | Browser／Runtime／CDP／VNC Health | 選用 Console、Network 錯誤、短錄影 |

不直接嵌入整個上游 noVNC App 再以 CSS 隱藏功能；以 RFB 公開 API 建立 Client Adapter。`scaleViewport`、`viewOnly`、剪貼簿及影像匯出等能力由 Adapter 封裝；禁止依賴 `_display` 等私有欄位。`resizeSession` 等操作必須依後端實際支援顯示，不可把所有 API 當作 x11vnc 都能完成。[R04–R05]

noVNC 的機器重開／關機控制不直接暴露；瀏覽器重啟與更新走 LensDock 的明確管理流程。Audio、麥克風、錄影及高幀率串流屬獨立傳輸與效能議題，不宣稱為這套 noVNC UI 的預設能力。

## 9. 控制權模型與人工接管

### 9.1 狀態機

| 狀態 | 人類輸入 | Agent CDP | 備註 |
|---|---|---|---|
| `IDLE` | 禁止 | 禁止 | 可觀察、可申請租約 |
| `HUMAN` | 僅持有人可寫 | 禁止 | 用於登入、MFA、設定、重現問題 |
| `AGENT` | 觀察者不可寫 | 僅持有者可連線 | 允許人類申請接管 |
| `HANDOFF_PENDING` | 暫停 | 停止新指令並撤銷舊路徑 | 排空／斷線後才能換持有人 |
| `CAPTURE` | 暫停 | 禁止外部操作 | 僅 `page-viewport` 擷取需要的短期租約；`desktop` 擷取不進入此狀態。不是頁面 JS 暫停 |
| `MAINTENANCE` | 禁止 | 禁止 | 備份、更新、回復 |
| `ERROR_LOCKED` | 禁止 | 禁止 | 失效安全狀態，需復原 |

租約預設 TTL 30 秒，持有人每 10 秒 Heartbeat；所有數值可配置。授予控制權時發出 `leaseId`、`epoch` 與 `runtimeGeneration`。Runtime 逐次驗證租約／有效期，Gateway 撤銷時關閉舊 CDP／控制 VNC 連線並拒絕舊 Epoch 重連。伺服器重啟產生新 Generation，先進 `IDLE`，不復用之前的寫入權。

### 9.2 接管流程

使用者申請接管 → Gateway 封鎖新 Agent 連線／指令 → 通知 Agent → 在上限時間內排空或中斷控制連線 → Runtime 確認舊租約失效 → 發 HUMAN 租約 → 開啟控制 VNC 連線。正常交還採相同反向流程；緊急停止直接撤銷，不假裝可以撤回已完成的網頁副作用。

Gateway 必須處理同時申請、重送、斷線與舊回應。若無法確認已排空，顯示「可能仍有先前動作的效果」，保持鎖定或由使用者明確接受風險後接管。斷開 CDP 不能停止已排程的頁面 JavaScript、Network Request 或服務端動作，因此不得把狀態名稱解釋成整個網站已凍結。

### 9.3 CDP 權限不是細粒度唯讀權限

Agent 租約授予的是整個 Workspace Browser 的高權限控制，不是特定網站的唯讀能力。`Runtime.evaluate` 等操作可能有副作用，不能只封鎖 `Input.*` 便認為已阻止寫入。[R17]

沒有 AGENT 租約的 Agent 只能讀取已授權的靜態交接資料，不提供泛用「唯讀 CDP」。`Browser.close` 等破壞性方法可額外拒絕以減少誤操作，但方法黑名單不是完整安全邊界；真正的範圍隔離使用不同 Runtime／Profile。

### 9.4 Usage mode and Agent pause

Usage mode is a workspace policy separate from the control state in §9.1 (`IDLE`, `HUMAN`, `AGENT` and so on). It has two values and a pause flag:

| Mode | Meaning |
|---|---|
| `AGENT` (default; keeps existing deployments unchanged) | Full collaboration: Agent leases and CDP, captures, drawings, feedback, Dock |
| `NOVNC` | Browser only. The human uses the shared browser through noVNC (takeover, clipboard, files, resolution). Agent and feedback features are closed |

Startup: the Runtime reads `LENSDOCK_MODE` (`NOVNC` or `AGENT`, case-insensitive, default `AGENT`). `lensdock-runtime supervise --mode NOVNC|AGENT` overrides the variable. The runtime image `ENTRYPOINT` already includes `supervise`, so a Compose `command` or Docker argument list appends `--mode ...`. `lensdock admin product-init --mode NOVNC|AGENT` (or `LENSDOCK_MODE`) is validated before any file is created, the flag wins over the variable, and the result is written to the generated `compose.env`. The variable and flag are the **initial** mode only: they apply while no saved policy exists. After that, the saved mode and pause flag win across restarts, including a saved pause, so a restart cannot silently undo a human's disable or pause. To change an existing deployment, use Settings.

Policy and routes: the authority is `{mode, paused, revision, updatedAt}`, included as `agentControl` in the workspace read. `GET /api/v1/workspaces/{id}/agent` is open to any role scoped to the workspace. `PUT .../agent` (body exactly `{"mode":"NOVNC"|"AGENT"}`), `POST .../agent/pause` and `POST .../agent/resume` (body `{}`) are Human or Administrator only. `GET .../agent/events` is SSE with `event: agent-control`, the policy JSON as data and the revision as id; it carries only that state, and stays open in `NOVNC` and while paused so an Agent can learn about stop and resume. `GET .../agent/setup` returns `{prompt}` to a human or administrator in `AGENT` mode. Errors: `AGENT_MODE_DISABLED` and `AGENT_PAUSED` (409); an unreadable policy fails closed with `UPSTREAM_UNAVAILABLE`.

| Surface | `NOVNC` | `AGENT`, paused | `AGENT` |
|---|---|---|---|
| Agent lease, targets, CDP, Bridge | Denied (409 `AGENT_MODE_DISABLED`) | Denied (409 `AGENT_PAUSED`) | Per §9.1 |
| Capture, drawing, feedback, artifacts, bundle | Closed to every role; stored data is kept but unreachable | Allowed | Allowed |
| Assigned Agent feedback read, ACK, resolve | Denied | Allowed, so a human can still leave feedback | Allowed |
| HUMAN lease, VNC, clipboard, files | Allowed | Allowed | Allowed |
| `agent/events` and `GET agent` state | Allowed | Allowed | Allowed |

Semantics: pause and mode switches run under the same Runtime manager lock as acquire, authorize and renew, so there is no window between them. Switching to `NOVNC` or pausing fences a held AGENT lease at once, closes its tickets and sockets (including CDP), rejects replay, and discards an AGENT lease that a pending capture would have restored. A HUMAN lease and the viewer stream are kept. Resuming or re-enabling `AGENT` never revives an old lease or token and never acquires control by itself: the Agent must acquire a fresh lease with a new epoch. A Runtime restart cannot reset the mode or pause. Disconnecting stops new commands only; website actions that were already dispatched cannot be reversed, and the UI says so.

Persistence and failure: the policy is written atomically (mode 0600) before a restrictive change is acknowledged. If that write fails, the running session is still fenced for the current process but the request returns an error (500), and the saved state on disk may be older, so a restart is not guaranteed to keep the change. A corrupt policy file fails startup instead of defaulting to permissive.

Notification and onboarding: `lensdock workspace watch <workspace>` streams the mode and pause notifications as JSON lines. The English onboarding prompt tells the Agent to run it, to take a fresh AGENT lease and Bridge only when a human explicitly resumes, how to read feedback and images and ACK, how to handle the stop errors, that page content is untrusted, and that image coordinates are not DOM coordinates. LensDock cannot insert a message into an arbitrary model conversation, so notification delivery to the model is not guaranteed; the enforced stop is the server-side fence, not the model reading the notice.

## 10. 截圖與擷取一致性

### 10.1 圖片來源

| 來源 | 包含內容 | 初版 | 定位限制 |
|---|---|---|---|
| `desktop` | noVNC 真實桌面，含分頁列、網址列、系統對話框 | 必須 | 圖片像素不是網頁 CSS 座標，不自動映射 DOM |
| `page-viewport` | 指定 CDP Target 的可視網頁內容 | 必須 | 可記錄 Viewport 與尺度；不能假設是 noVNC 前景頁 |
| `page-full` | 指定網頁的完整頁面或分段擷取 | 未實作（M4） | 固定元素、懶載入、動態頁、超長圖須有界限 |
| `drawing` | Gateway 產生的空白不透明白色 PNG，每邊 64–4096 px，仍受 24 百萬像素與配額限制 | 已實作 | 不是瀏覽器畫面：Provenance 為 `generated-canvas`，`runtimeGeneration` 為空，`targetId`／`navigationId` 為 null，沒有 `page`，`consistency` 為 `unknown`；不需租約、不連 Runtime，瀏覽器不可用時也能建立；僅 Human Operator／Administrator；不提供 DOM 定位 |

桌面擷取使用公開 `RFB.toBlob()` 或等價公開 API；若畫面長時間斷線，禁止把過期緩衝畫面當新截圖。桌面來源標示為「使用者端畫面擷取」，並記錄 Client Capture Time 與 Server Receipt Time；它不是可防竄改的伺服器取證。[R05]

網頁擷取由 Runtime 依明確 Target ID 執行 CDP `Page.captureScreenshot`。記錄擷取前後的 Target、Document／Navigation Generation、Viewport、Scroll、實際影像尺寸與時間；前後不一致則要求重擷取或標示不一致，不能偷偷串接不相關 DOM。[R16]

### 10.2 擷取工作流程

`desktop` 擷取由前端以 `RFB.toBlob()` 從既有唯讀畫面取得，不需任何租約、不改變控制權，只需 Human Operator 身分與 Workspace 授權。`page-viewport` 擷取流程：取得短期 CAPTURE 租約 → 確認來源與目標 → 擷取畫面及最小環境資料 → 記錄一致性結果 → 歸還控制權 → 在獨立靜態圖片上編輯。歸還規則：若擷取前的持有人租約仍在 TTL 內且持有人未釋放，以新 `epoch` 還給原持有人並發事件通知；否則回到 `IDLE`。任何情況都不得復用擷取前的舊 Epoch；進入 CAPTURE 時舊 CDP／控制 VNC 連線已關閉，原持有人必須以新 Epoch 重連。AGENT 租約進行中若人類要求 `page-viewport` 擷取，UI 必須提示會中斷 Agent 連線，並建議改用 `desktop` 擷取或等待 Agent 釋放。長時間標註不占住 Browser 控制租約。

「畫面定格」只代表標註底圖不再變動，**不代表遠端網站停止執行**。CSS 動畫、Video 與背景 Request 可能仍變化，`consistency` 必須能表達 `stable-navigation`、`best-effort` 或 `unknown`，不得聲稱跨畫面與 DOM 的完美原子性。

`drawing` 由 `POST /api/v1/workspaces/{workspaceId}/captures` 帶 `source: "drawing"`、`imageWidthPx`、`imageHeightPx`（皆為 64–4096 的整數，不接受 `targetId`、`leaseId`、`allowInterrupt`）建立，回 202 與已就緒的私有 Capture，之後與其他來源走相同的草稿、預覽、發布流程。裁切繪圖會產生新的 drawing 來源草稿（原圖不變，標註不複製）。

### 10.3 監看正在操作的分頁

Live View 旁顯示 Agent Target 與桌面前景是否一致。`follow-agent` 為可選模式，只在 AGENT 租約且使用者允許時將目標頁帶到前景；人工控制時禁止搶焦點。初版單一作用中任務，背景 Target 則顯示明確提示與單次網頁預覽，不假裝 noVNC 看得見所有分頁。

## 11. 標註編輯器

### 11.1 必須功能

矩形框、箭頭、自由筆、文字、編號 Pin、實心遮蔽、裁切衍生圖、縮放／平移、Undo／Redo、選取／移動／刪除與草稿自動保存。文字使用本機 IME；匯出前等待字型載入完成。每個 Pin 可附註釋，並與回饋步驟對應。

原圖、可編輯標註向量、遮蔽政策、合成圖彼此分離。透明 Highlight 不等於敏感資訊遮蔽；敏感區域使用實心不透明像素覆寫。裁切產生新 `captureId` 或明確 Parent Transform，不改寫舊圖片尺寸導致既有標註漂移。

實作現況：編輯器為自建，不依賴 tldraw、Excalidraw 或其他白板 SDK。工具包含矩形、橢圓、菱形、直線、箭頭、自由筆、文字、編號 Pin、不透明遮蔽、裁切與平移。支援 Shift 多選、框選、群組移動、縮放控制點與線段端點、複製、六種對齊（左、水平置中、右、上、垂直置中、下）、四種圖層指令（置頂、上移一層、下移一層、置底）、單一與批次樣式（矩形、橢圓、菱形可填色）、Undo／Redo（上限 200 步）、800 ms 自動保存（ETag 防衝突）及需確認且可復原的「全部清除」。快捷鍵為 V 選取、R、O、D、L、A、P、T、N、X、C（裁切）、H（平移），在 IME 組字與文字輸入期間不攔截。遮蔽一律在預覽與匯出時最後繪製。

裁切永遠建立新 Capture 與新草稿，舊草稿與原圖不變；標註不會搬移，介面會明確警告需在新草稿重新標註。「複製圖片」與「下載 PNG」輸出攤平的 PNG，遮蔽已燒入，不含可編輯資料或原始場景；在目前整合環境實測，下載檔與剪貼簿 `image/png` 的 SHA-256 相同、尺寸皆為 1920×1080。這是使用者自用的匯出，與 §11.2 的發布流程無關：發布仍由後端重新編碼並覆寫遮蔽。

Later measurement note: in `local-toolbar-modes-16` the real browser clipboard image and the downloaded PNG had zero differing pixels at 1920×1080, but their compressed bytes differed because Chrome re-encodes the clipboard image. SHA-256 equality seen earlier is therefore not a guarantee; compare pixels. Copy needs the browser clipboard read, write and sanitized-write permissions; when they are denied the UI shows an honest error instead of success.

### 11.2 匯出與後端安全處理

前端依來源圖片與結構化標註產生 PNG Preview，以 Draft ETag 綁定後上傳；任何草稿變更都使舊 Preview 失效。後端不信任這個使用者端合成結果，檢查尺寸與資源限制後重新編碼，對分享底圖及合成圖再次實心覆寫遮蔽區。發布前讓使用者看見實際會交付的兩張圖；伺服器通過所有驗證後才原子性發布。以此避免要求 Go 後端自行重造完整 CJK／Emoji 文字排版引擎，同時讓敏感遮蔽不只依賴前端圖層。

### 11.3 座標契約

標註一律使用不可變來源圖片的 Normalized Coordinates，原點在左上角，x／y 在 0–1。儲存 `imageWidthPx`、`imageHeightPx`、來源、Capture ID 與原始／分享圖片雜湊。矩形必須同時檢查 x+w≤1、y+h≤1；JSON Schema 以外還要做跨欄位驗證。

畫面指標須以實際影像內容矩形計算，不能以包含 Letterboxing 的面板計算。縮放後 CSS 尺寸、PNG 像素、遠端桌面像素、瀏覽器 CSS Pixel 及 DPR 是不同座標空間。

對 `page-viewport`：先由 Normalized Coordinates 得到來源像素，再用擷取紀錄的實際 `pixelsPerCssX/Y` 轉為 Viewport CSS Pixel；不可直接乘或除 DPR。手機縮放、不同視覺 Viewport、跨 Frame 必須經目標能力驗證。對 `desktop` 不提供自動 DOM 點擊座標。

`drawing` 沒有瀏覽器頁面，也沒有 `pixelsPerCssX/Y`；標註座標只相對圖片像素，Agent 不得把繪圖標註當成 DOM 定位依據。

標註不是可直接執行的定位器。Agent 動作前必須重新擷取／Observe 當前頁面、檢查 Target 與 Navigation Generation。`@e1` 等上游 Snapshot Ref 可暫存為提示，但不可當成跨重啟／重新整理的永久 Element ID。

### 11.4 Advanced tools, styles and outputs (schema 0.3.0)

| Tool | Wire kind | Behavior and limit |
|---|---|---|
| Highlighter | `highlight` | Translucent colored rectangle (plain alpha fill). Not redaction |
| Spotlight | `spotlight` | Dims the base image outside the union of all spotlight rectangles once, at a fixed 0.6. No per-spotlight setting. Not redaction |
| Blur | `blur` | Two-pass separable box blur of the base image pixels inside the rectangle. Radius `effectSizePx` (default 12). Not redaction |
| Pixelate | `pixelate` | Block average inside the rectangle, blocks anchored at its top-left, edge `effectSizePx` (default 16). Not redaction |
| Solid redact | `redact` | Unchanged: opaque black, style-free, painted last |
| Arrow styles | `arrow` | Filled, open, double and curved (quadratic, draggable control) |
| Text styles | `text` | Seven styles with bundled fonts only; absent means the old outline look |
| Smoothed freehand | `freehand` | Rendering flag over the raw points; new strokes default to smoothed |

Rendering contract: the editing view, the exported PNG and the Gateway share one deterministic integer algorithm. Rectangles use floor(x·w), floor(y·h), ceil((x+width)·w), ceil((y+height)·h), clamped to the image. Order: solid redaction pixels are blackened in the working base first, so a blur or pixelate cannot carry a secret out of a redacted area; blur and pixelate run in annotation order on the base; the spotlight dim follows; other annotations are drawn in list order; solid redactions are painted last and stay opaque. Blur, pixelate and highlight are shown as real pixels or alpha, never as gray boxes, and an effect that exists only in the browser view and vanishes on export is not acceptable.

Outputs are separate: the draft (autosaved), Copy PNG and Download PNG (flattened, no editable data, no publication), and Send (publish and queue). The Gateway applies the same blur and pixelate to `screenshot.redacted.png` and re-applies solid redaction, and re-encodes the uploaded annotated PNG without filtering it a second time. So the shared base image shows blur and pixelate but not the spotlight, while `screenshot.annotated.png` also shows the spotlight; this difference is expected. The sum of pixels affected by blur and pixelate is bounded at 100 million as a server resource limit. The UI states that highlight, spotlight, blur and pixelate are not secure redaction (blur can sometimes be partly recovered); use solid redact for sensitive data (FB-02). Out of scope: import or upload of images, multiple images, backgrounds, rotation and a screen eyedropper.

## 12. 回饋、Bug Report 與任務交接

### 12.1 Feedback Bundle

每一筆回饋至少包含：不可變 ID／版本、Workspace、Capture、提出者、類型、標題、描述、預期結果、實際結果、重現步驟、標註向量、遮蔽後圖片、可選環境資訊、目標 Agent 與發布時間。**純視覺回饋是正式路徑**：標題、描述、預期結果、實際結果與標註註解都可以留空，只靠繪圖、標記與文字標註發布；標題留空時，發布時以固定標題 `Visual feedback` 取代（草稿不變），已填的標題與其他文字原樣保留，不從圖片推導任何文字，也不把圖片中的文字當成指示。類型、意圖、長度上限與控制字元檢查不因此放寬，預設意圖仍為 `analyze-only`。

類型包含 `bug`、`task`、`question`、`visual-regression`。可選收集 Console Error、Network Failure、Viewport、Browser 版本、Commit／Branch／測試環境；**Cookies、Authorization Header、Storage、完整 DOM、Request／Response Body 預設不收集**。

分享包示意：

```text
feedback/<feedback-id>/
  feedback.json
  task.md
  screenshot.redacted.png
  screenshot.annotated.png
  annotations.json
  environment.json
  diagnostics.redacted.json
  manifest.json
```

附件以固定相對名稱列出 SHA-256 與大小。下載方驗證 Hash、大小及路徑，不接受 Zip Slip、Symlink、絕對路徑、重複覆寫或任意外部附件 URL。原始敏感圖片不列入分享包，也不因 Agent 猜測路徑就能存取。

Schema 版本逐份文件決定：Capture 來源為 `drawing`、任一標註為 `ellipse`、`diamond`、`line`、任務沒有描述、或 `bug`／`visual-regression` 任務缺預期或實際結果時，發布的 `feedback.json`、`annotations.json`、`environment.json` 與 `manifest.json` 一律宣告 `schemaVersion: "0.2.0"` 且彼此一致；其餘維持 `0.1.0`，讓只理解 0.1.0 的讀取端繼續讀取（0.1.0 的 Schema 仍要求描述，以及 `bug`／`visual-regression` 的預期與實際結果，標題在兩個版本都必填）。已發布的 Revision 與 Artifact 不可變（資料庫觸發器拒絕改寫與刪除），既有 0.1.0 Bundle 不會被改寫成 0.2.0。`lensdock` CLI 與 MCP 讀取兩個版本，並依該版本的 Schema 驗證任務欄位，遇到其他版本明確拒絕。

Schema `0.3.0` is selected for a document when any annotation uses the kinds `highlight`, `spotlight`, `blur` or `pixelate`, or any of the new fields: arrow `control` and `arrowStyle`, text `textStyle`, `effectSizePx`, `opacity`, or freehand `smoothing`. Then `feedback.json`, `annotations.json`, `environment.json` and `manifest.json` all declare `0.3.0`. Documents that use none of them keep the earlier `0.1.0` or `0.2.0` selection and validation unchanged; the `0.1.0` and `0.2.0` schemas reject `0.3.0`-only data; published records are never rewritten. `lensdock` CLI and MCP readers accept `0.1.0`, `0.2.0` and `0.3.0`.

New fields: highlight color is `style.stroke` (default `#f2a900`) and `opacity` is 0.1 to 0.8 (default 0.35); `effectSizePx` is an integer 2 to 64 in source pixels (default 12 for blur and 16 for pixelate); `arrowStyle` is `filled` (default), `open`, `double` or `curved` (quadratic curve through `geometry.control`); `textStyle` is `outline` (default), `plain`, `label`, `shadow`, `mono`, `bold` or `underline`; `smoothing` is true for new freehand strokes and absent (false) in legacy documents. A field that does not belong to its kind is rejected. Step counter and filled rectangle add no wire kind. When a revision is created from a published one, its blur and pixelate annotations are not carried over, because their effect is already in the shared base image and would otherwise be filtered twice.

### 12.2 發布及處理狀態

`DRAFT → READY → QUEUED → ACKNOWLEDGED → IN_PROGRESS → RESOLVED → VERIFIED`。另有 `CANCELLED`；驗證失敗可重新排入 QUEUED 並保留歷史。狀態轉換與介面一對一：`publish`（DRAFT→READY）、`dispatch`（READY→QUEUED）、`ack`（QUEUED→ACKNOWLEDGED）、Agent 申請附 `feedbackId` 的 AGENT 租約（ACKNOWLEDGED→IN_PROGRESS）、`resolve`（IN_PROGRESS→RESOLVED）、`verify`（RESOLVED→VERIFIED，或帶理由退回 QUEUED；僅人類）、`cancel`（任一非終態→CANCELLED）。沒有對應介面的轉換不存在。內容發布後不可原地修改；修改產生新 Revision、重新遮蔽與確認，舊 Revision 留存於授權／保留政策內。

前端明確顯示「等待 Agent 讀取」、「Agent 已確認收到」、「處理中」、「待人工驗證」，不以單一「Sent」混淆。Agent 聲稱完成時，應提供變更摘要、驗證步驟、新截圖及相關 Commit；最終 VERIFIED 由人類決定。

讀取與排隊都不是 ACK：`feedback list/get/pull` 與 MCP `feedback_list/get/image` 不改變狀態；人類 `dispatch` 只是 READY→QUEUED，指定可讀取該 Revision 的 Agent 身分；只有被指派的 Agent 以自己的 Token 明確 `ack`（CLI 需 `--image-viewed`，MCP 需本 Session 已取得圖片且 `imageViewed=true`）才進入 ACKNOWLEDGED，而且 ACK 不授予瀏覽器控制。Agent 以 `lensdock lease acquire <workspace-id> --mode agent --feedback-id <id> [--feedback-revision <n>] ...` 取得 AGENT 租約後才進入 IN_PROGRESS；`resolve` 只到 RESOLVED，VERIFIED 只有人類 `verify` 能產生。

### 12.3 交付介面

| 介面 | 能力 | 限制 |
|---|---|---|
| 手動匯出／複製任務文字 | 不綁 Agent 平台 | 貼上文字不等於模型已讀圖片，需實際附圖 |
| `lensdock feedback pull` | 在 Agent 執行環境下載核准 Bundle | 只寫入核准目錄，不假設可讀取 Server 本機路徑 |
| LensDock MCP | `feedback_list/get/image/ack/resolve` 等工具 | 圖片需以 Image Content 或 Client 可存取的資源提供 |
| REST / SSE | 佇列、狀態、事件 | SSE 通知不代表訊息已插入既有 Agent 對話 |

MCP 可回傳影像及結構化內容，但各 Client／模型是否看圖需整合測試；必要時回退到下載圖片及手動附件。[R13] 不以 Terminal 貼字、模擬按鍵或未公開對話 API 強行注入 Codex／Claude Code／OMP 的現有會話。

## 13. Agent 整合

### 13.1 Codex / Claude Code

共同最低能力為可執行 LensDock CLI、讀取已下載文字／圖片及執行 agent-browser。MCP 為第一版正式交接介面之一，設定由固定 Client 版本驗證；以官方 MCP Client 文件為準，不把 `codex mcp-server` 當 Client 設定方式。[R14–R15]

操作流程：領取 Feedback → 明確讀取圖片 → ACK → 申請 AGENT 租約 → 取得本機 CDP Bridge → 以 agent-browser 連既有瀏覽器 → 重新 Snapshot → 執行任務 → 提交驗證 → 釋放租約。沒有 CDP 連線時必須失敗，不得啟動本機替代瀏覽器。外部 Browser 模式不執行 `agent-browser install` 來另下載瀏覽器。

### 13.2 OMP

OMP 原生 Browser 支援 CDP 連線，可作控制介面；但仍須使用 LensDock 發出的租約限定 HTTP Discovery Bridge，不能指向 Runtime 真實連接埠。明確指定 Target／名稱，避免 Relay 或本機預設覆蓋設定。OMP 的 Feedback 交付至少支援 CLI；MCP 能力依鎖定版本驗證後再啟用。[R18]

### 13.3 本機 CDP Bridge

Bridge 僅綁 `127.0.0.1` 的可用 Port，將 Client 的 HTTP Discovery 與 WebSocket 轉發到受認證 Gateway。`/json/version`、`/json/list` 回傳的 WebSocket URL 必須重寫成 Agent 真正可達的 Bridge URL，不洩漏 Container DNS／9223。`/json/new`、`/json/close`、`/json/activate` 等會改變 Browser 狀態的 Discovery 端點同樣只對有效 AGENT 租約開放。

Loopback 不是本機隔離邊界：Bridge URL 含每次啟動隨機產生的路徑前綴，Bridge 本身也驗證 Client；多使用者主機改用 Unix Socket 或檔案權限限制。Bridge 保留租約 Heartbeat 與 Token；憑證以權限限制檔案或安全通道傳入，不放永久 Query Token、Prompt 或 CLI 引數。租約撤銷後立即拒絕舊連線及重連。原始 CDP 承載的是高權限操作，只允許使用者信任的 Agent Host。

Gateway → Runtime 的 CDP 代理在轉發 WebSocket 握手時剝除 `Origin` Header；Chromium 對帶有 `Origin` 的 CDP WebSocket 預設拒絕，不以 `--remote-allow-origins=*` 放寬。Chromium 必須以專用 `--user-data-dir` 開啟遠端除錯，新版 Chromium 拒絕在預設 Profile 上啟用 CDP，這與 §6.1 的持久化 Profile 要求一致。

## 14. 隱私、憑證與安全要求

### 14.1 存取控制

本機也預設要求登入／Token 配對；不以 Loopback 當唯一認證。遠端必須 TLS，支援反向代理，WebSocket 檢查 Origin；Browser UI 採 Secure／HttpOnly／SameSite Session Cookie 及 CSRF 防護。API Token 分為 Viewer、Human Operator、Agent、Administrator；Token 授權限定 Workspace 和功能，不共用最高權限 Token。

Runtime 受驗證的管理入口只接受 Gateway 身分。網站 JavaScript、Browser Process、其他 Container 都不能僅憑知道位址取得 Gateway 管理權。Network Policy 需限制主機服務、Link-local／雲端 Metadata 與非必要內網；測試內部開發站台時採明確例外，不假設 Compose 私有網路已阻止所有側向存取。

Usage-mode gating (§9.4) is enforced in the Runtime and the Gateway on the server, for every role. The UI hides surfaces, but a direct request to a closed route is still refused. `viewOnly` is not used as a security mechanism. Observed in the integrated run on `local-toolbar-modes-16` (single runs, not a full matrix): a viewer calling pause got 403, an Agent token for another workspace reading Agent state got 404, and a cookie-authenticated pause without the CSRF token got 401 with the policy revision unchanged. `NOVNC` mode is a feature switch controlled by a human or administrator. It does not isolate the Browser from a human, and it does not replace the sandbox or Network Policy in §14.2.

### 14.2 Sandbox 與主機差異

Chromium 非 root 執行，Renderer Sandbox 是 Release Gate。禁止在失敗時自動加入 `--no-sandbox`、`--privileged` 或整體 `seccomp=unconfined`。User Namespace、Seccomp、AppArmor 與主機 Kernel 組合必須經 Preflight／測試確認；`no-new-privileges` 與 SUID Sandbox 的相容性也需明確處理，不能盲目組合硬化旗標。[R19]

應用服務預期採最小 Capabilities、無 Docker Socket、必要的唯讀檔案系統與 tmpfs；實際 Profile 和顯示服務可寫位置需列舉。強敵對多租戶或需 Kernel 級隔離的用途另用 VM／微型 VM；不能宣稱一般 Container 等同強隔離沙箱。

### 14.3 分享前去識別化

截圖先進私有草稿，使用者確認分享範圍後才發布。遮蔽必須真正覆寫像素並重新編碼，去除圖片 Metadata；不能只放可移除 SVG、CSS Overlay、透明矩形或只模糊處理。基礎分享圖和標註合成圖都必須遮蔽，縮圖、裁切、預覽及診斷資料也受同一政策。

後端需對最終合成圖再次覆寫遮蔽區，避免圖層／文字重新曝露原內容；影像尺寸／格式／解碼資源均有限制。自動敏感資訊辨識只作輔助，不宣稱零洩漏。使用者仍需檢查圖片、文字、URL Query、頁面標題、Console 及註解。Agent 只取得已發布的特定 Revision。

預設不記錄按鍵、Password、剪貼簿、CDP 內容及網頁 Body；稽核保存動作類別、持有人、時間、結果與關聯 ID。人工登入模式暫停 Agent 租約和非必要診斷。頁面文字、DOM 及附件屬不可信內容；Agent 指引明確禁止將其中的指令當系統授權。

## 15. 更新策略

### 15.1 三層更新責任

| 層級 | 更新方式 | 管理者 |
|---|---|---|
| Chromium 與 Debian userspace | 建置新的 Browser Runtime 映像、測試後替換 | CI + 主機 Admin CLI |
| noVNC、Gateway、UI、agent-browser 相容版本 | 鎖定依賴、建置版本化映像／CLI、做相容測試 | CI + Release 管理 |
| 主機 Kernel／Docker Engine／Container Runtime | 主機維運、必要時主機重啟 | 主機管理者，不由 Browser Container 操作 |

Container 使用主機 Kernel，因此更新 Debian 基底並不會更新主機 Kernel。[R20] 不在運行中的 Container 執行自動 `apt upgrade` 作正式更新；不在每次啟動時下載最新 Browser／字型／前端套件。

### 15.2 Release Manifest

每次 Release 必須記錄：Git Commit、每架構 Image Digest、Debian 基底 Digest、APT 套件版本與來源時間、Chromium 版本、noVNC／agent-browser／Gateway／CLI 版本、資料 Schema、SBOM、簽章或來源證明、測試報告及回退相容資訊。

Digest Pinning 只固定基底，不能單獨保證 `apt install` 的所有相依可重現。Release Pipeline 應保留 APT 套件／索引快照或受控 Mirror；安全更新候選以新的來源快照產生，再保存成不可變 Release。不能因追求重現而永遠停留在過期 Security Snapshot。[R21]

### 15.3 方便但不盲目更新

UI 提供更新檢查、版本差異、風險與主機 CLI 指令。主機 CLI 定義 `update check / plan / apply / rollback`。定期 CI 建立更新候選及測試，可自動產生版本提案；正在登入、測試或控制中的 Workspace 不被背景程序突然重啟。

初版更新流程：檢查可用備份空間 → 停止新租約 → 等待或明確中止既有任務 → 正常關閉 Chromium → 備份 Profile、相關金鑰資料與 Gateway DB → 複製至候選 Volume → 用候選映像驗證 → 人工確認切換 → 保存舊映像及更新前快照。候選環境預設只執行本機 Fixture，外部網路封鎖；對真實已登入網站的測試必須另外授權，避免候選與正式環境重複發送操作。

實作現況：主機 CLI 已有 `lensdock update init|check|plan|apply|approve|abort|rollback|status|checkpoints|branches|compose` 與 `lensdock release manifest|keygen|sign|promote|merge|verify`（語法以不帶參數執行 `lensdock` 的輸出為準）。目前產生的 Release 是 `--dev --uncommitted` 開發版，未經 production 簽署與掃描；Web UI 只顯示可複製的主機指令，不會套用更新。

### 15.4 回退必須成對

舊 Chromium 未必能讀取新版本寫過的 Profile，故 **Rollback = 舊映像 + 更新前 Profile／Keyring 快照 + 相容 DB 快照**，不是只把 Image Tag 改回去。原則上保留更新前與更新後資料分支，避免誤覆寫。回退會失去快照後的瀏覽器變更，必須明示 RPO；回饋附件盡量獨立保留，DB Migration 設計採可相容／有快照策略。[R21；回退為本專案設計要求]

實作現況：Release 宣告 Gateway DB schema（`--gateway-db 2 --min-gateway-db 1`）；更新先對 DB 複本做 migration，再啟動候選；回退還原 schema 1 的 DB 與舊 Binary，不會讓舊 Gateway 開啟 schema 2 的 DB。更新維護前，以同一原始 Repository 的 `:lensdock-keep-<完整 digest>` Tag 固定舊映像，避免 Docker 在最後一個 Tag 移動後無法以 digest 取回原映像。

安全修補與瀏覽器／OS 大版更新分不同核准級別；Host OS／Docker 更新另有 Runbook。完整流程見 `docs/UPDATE-RUNBOOK.md`。

## 16. API 與資料契約

核心 REST 使用 `/api/v1`。UUID 型 ID 不含秘密；時間使用 RFC 3339 UTC；UI 顯示使用者時區。重要寫入支援 `Idempotency-Key`；租約／草稿異動支援版本前提，衝突回 409／412。所有錯誤為一致的 JSON 結構，包含 `code`、`message`、`requestId`，不得回傳秘密或原始 CDP 訊息。

| 功能 | 代表性介面 |
|---|---|
| Workspace 狀態 | `GET /api/v1/workspaces/{workspaceId}` |
| 控制租約 | `POST .../leases`（Agent 模式可帶 `feedbackId` 與選用的 `feedbackRevision`）、`POST .../leases/{leaseId}/renew`、`DELETE .../leases/{leaseId}` |
| 人工接管 | `POST .../takeover` |
| 擷取 | `POST .../captures`（`source` 為 `desktop`、`page-viewport` 或 `drawing`）、`PUT /api/v1/captures/{captureId}/image`、`GET /api/v1/captures/{captureId}`、`GET .../image`、`POST .../crop` |
| 草稿 | `POST /api/v1/feedback`、`GET .../{feedbackId}/draft`、`PATCH .../{feedbackId}`、`PUT .../{feedbackId}/draft/preview` |
| 發布／交付 | `POST .../publish`、`POST .../dispatch`、`POST .../ack`、`POST .../resolve`、`POST .../verify`、`POST .../cancel` |
| 分享資源 | `GET /api/v1/feedback/{feedbackId}/artifacts/{name}` |
| Live Events | `GET /api/v1/events`，SSE，需 Workspace 授權 |
| 使用模式／暫停 | `GET`／`PUT /api/v1/workspaces/{workspaceId}/agent`、`POST .../agent/pause`、`POST .../agent/resume`、`GET .../agent/events`（SSE）、`GET .../agent/setup`（見 §9.4） |
| 健康 | `/healthz`、`/readyz`；敏感版本資訊需認證 |

`contracts/openapi.yaml` describes 28 operations: 22 core operations and six usage-mode operations (`getAgentControl`, `setAgentMode`, `pauseAgent`, `resumeAgent`, `streamAgentControl`, `getAgentSetup`). The Gateway implements them all. The Runtime owns leases, workspace state and usage-mode policy, and provides clipboard and CDP routes. Additional Gateway routes include `/api/v1/session` (token pairing creates an in-memory cookie session; restart requires pairing again), `/api/v1/system`, `/api/v1/audit`, file transfer, resolution, targets, crop, revisions, delivery, manifests and bundle downloads. `tools/validate_spec.py` checks unique operation IDs, path parameters, success responses, schemas and local references without pinning an incidental operation count. `schemas/feedback.schema.json` defines immutable published content, not the complete private draft database or mutable processing state.

## 17. 資料模型

| 實體 | 關鍵欄位／規則 |
|---|---|
| Workspace | id、runtimeId、profileVolumeRef、policy、status；不同身分不共用 Volume |
| Lease | id、workspaceId、holderId、mode、epoch、runtimeGeneration、expiresAt、status |
| Capture | id、source、targetId、navigationId、requestedBy、leaseId（`desktop` 與 `drawing` 為空）、priorLeaseId、image dimensions、timestamp、consistency、provenance、private object ref；`drawing` 另有 runtimeGeneration 為空、targetId／navigationId 為 null、無 page |
| Annotation | id、kind、normalized geometry、style、comment；JSON 純資料，禁止 Script／任意 SVG |
| FeedbackRevision | id、revision、capture、sanitized artifacts、task fields、share policy、publishedAt |
| Delivery | feedbackId、revision、agentId、status、acknowledgedAt、result refs；不可冒名 ACK |
| AuditEvent | id、actor、workspaceId、category、timestamp、result、correlationId；無原始秘密 |
| ReleaseRecord | manifest、snapshot refs、test result、status、rollback plan |

Artifact 發布需原子性：所有資源寫入完成與驗證後，才提交 Feedback Revision 與可下載狀態；不能讓 Agent 讀到半成品。重複 publish／dispatch 需由 Idempotency Key 回同一結果。配額滿時阻止新增草稿，不刪正在處理的證據。

## 18. 效能與可靠性目標

基準環境：Linux 主機分配 4 vCPU、8 GiB RAM；單 Workspace、1920×1080、兩個一般網頁、區域網路。初始 Runtime 記憶體上限建議 4 GiB、`/dev/shm` 1 GiB；共享記憶體計入實際使用與容器記憶體壓力，不視為額外免費 RAM。GPU 不為初版必要條件。

| 指標 | 初始驗收目標 |
|---|---|
| 一般桌面截圖可進入編輯 | p95 ≤ 2 秒，不含外網上傳 |
| API 租約與狀態查詢 | p95 ≤ 300 ms，不含人工等待 |
| 正常人工接管 | 舊控制排空後 ≤ 2 秒發新租約；排空上限 5 秒後需明確錯誤／警告 |
| 畫面互動 | 區域網路下以 200 ms 為初始延遲目標；以端到端測試量測 |
| 資料完整性 | 強制重建 Container 後 Volume 不被刪除；正常停止後能恢復指定持久狀態 |
| 瀏覽器崩潰 | Gateway 標示離線、撤銷舊租約；新 Browser Generation 不自動接續破壞性動作 |

初版截圖上限 32 MiB、單張 24 百萬像素、每份回饋 100 個標註、文字欄位有長度上限，並限制解碼記憶體；繪圖每邊 64–4096 px，同樣受像素與大小限制。超限拒絕或要求裁切；v0.2 全頁模式採分段圖與映射，不無限配置大型 Canvas。這些是產品初值，需依量測調整。

## 19. 測試、驗收與可觀測性

必須測試：單一控制者、租約撤銷與舊 Socket、noVNC 唯讀後端、人工登入、Profile 重建、CJK／Emoji、IME Bridge、截圖來源、座標縮放、敏感遮蔽、跨主機交付、Agent 真正讀圖、更新與成對回退。

持續整合分為：靜態與契約檢查 → 映像建置與供應鏈掃描 → Runtime 啟動與 Sandbox 檢查 → UI／CDP／交接 E2E → 升級回退 → 發行。每個報告包含精確版本矩陣；無法執行的測試標示未執行，不能視為通過。

觀測包含 CPU／RAM／磁碟、Browser／Display／VNC／CDP Health、租約持有人、Framebuffer 新鮮度、Queue Depth、截圖延遲、交付 ACK 與 Artifact 失敗。診斷不預設收集網站內容。錯誤必須區分 Runtime 未啟動、CDP 不通、錯誤 Target、字型缺失、權限不足、租約失效與模型不支援圖片。

詳細 Given／When／Then 驗收案例見 `docs/ACCEPTANCE.md`。`mise run validate` 執行的 Python 測試只驗證資料契約與座標公式，不執行 Container 或瀏覽器；整合行為由 Go 與前端測試及實機演示驗證，結果與限制見 `docs/IMPLEMENTATION-PLAN.md`，不得由 Python 測試通過推論整合通過。

## 20. 開發分期與優先順序

| 里程碑 | 範圍 | 出口條件 |
|---|---|---|
| M0：風險驗證 | Debian Sandbox、CJK／Emoji、雙 VNC、UTF-8 Clipboard、CDP Bridge | 技術 Spike 通過且固定相容矩陣 |
| M1：安全 Runtime | Volume、Process Lifecycle、Gateway 認證、單寫入者租約、人工登入與 Agent 控制 | 可在同一瀏覽器安全交替操作；失連失效安全 |
| M2：視覺交接 MVP | 自訂 UI、桌面／Viewport 擷取、標註遮蔽、Feedback Bundle、CLI、MCP | 人類圈出問題 → Agent 讀圖 → 回報驗證，端到端通過 |
| M3：可維運版本 | 不可變 Release、備份、升級、回退、來源證明、健康指標 | 用乾淨與既有 Profile 都能驗證升級／回退 |
| M4：擴充 | 多圖、全頁、診斷、回歸比較、多人觀察、選配輸入法 | 每項獨立安全／效能驗收，不阻擋前面閉環 |

首個對外可用版本至少完成 M0–M3。不要先投入畫面錄影、多人即時共同編輯或大型 Browser Pool，而延後租約、資料安全及回退驗證。實作任務拆解見 `docs/IMPLEMENTATION-PLAN.md`。

實作現況（2026-10-25）：M1–M3 已在 macOS arm64／OrbStack 的整合環境實作，並以實際 Web UI、真實 Claude Code 與 Codex 的圖片讀取／ACK、更新、成對回退與失敗注入演示過。M0 的 amd64 證據只適用於 M0，目前整合版本沒有完整的 amd64 產品 E2E。Release 為未簽署的開發版，不是 production 簽署或掃描過的發行；M4（全頁、多圖、診斷擴充、回歸比較、多人觀察、選配遠端 IME）未實作。不得據此推論所有驗收 ID 全數通過；逐項限制見 `docs/IMPLEMENTATION-PLAN.md`。

The 0.3.0 usage modes, Agent pause, expanded annotation tools and feedback schema were implemented and exercised on macOS arm64 and OrbStack with `local-toolbar-modes-16` (generation 13). Managed updates to releases 17 and 18 preserved paused `AGENT` revision 6 and its timestamp. The final release 18 (generation 15) passed TypeScript/Vite, 195 frontend tests, all-package Go race tests, vet, formatting, shellcheck, version pins and 33 specification checks. Its 140 responsive cases passed (CA-14; see §8.1). This remains one host and unsigned development releases; no complete amd64 product E2E, model acknowledgment of a notification, or model reading of the setup prompt is claimed.

原生 IME 的最終狀態：macOS 注音通過，日文與韓文未測（見 §7.2）。

## 21. 待實作前確認但不阻擋本規格的事項

| 項目 | 本規格預設 | 後續確認方式 |
|---|---|---|
| 目標主機 | Linux + Docker Compose | 列出 Kernel、Docker、CPU 架構做 M0 測試 |
| 遠端存取 | 本機 Loopback 起步；跨主機 TLS／VPN | 決定反向代理與身分提供者 |
| 公開授權 | 尚未選定專案自身 License | 發行前決定，保留上游必要標示 |
| 瀏覽器版本 | Debian Security Chromium | CI 鎖定實際套件與影像 Digest |
| 金鑰環／備份加密 | 敏感 Volume 全面保護；不保存明文主密碼 | 根據主機 Secret Provider 完成部署方案 |
| 多人／多 Agent | 同 Workspace 輪流，身分分 Container | 需要並行時新增 Workspace，不共用 Profile |

## 22. Definition of Done

使用者能在 Debian slim Runtime 中以 noVNC 登入；Codex／Claude Code 經 agent-browser、OMP 經相容 CDP 路徑操作同一個 Browser；CJK／Emoji 在瀏覽器、遠端畫面及標註圖皆通過測試；使用者能真正接管、截圖、標註、遮蔽、發布與追蹤 Agent 是否讀取；更新能透過明確命令完成並以相容快照回退；全部路徑有認證、稽核及驗收報告。

**不滿足任一安全邊界、只顯示看似可用的 UI、或只有 PNG 路徑卻沒有讓 Agent 取得圖片，都不視為完成。**
