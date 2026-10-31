# LensDock

[English](README.md)

讓你和 Coding Agent 共用同一個持久化 Chromium 工作站。用 noVNC 觀看真實瀏覽器、隨時接管操作，再把標註截圖交給 Agent，不必分享日常使用的瀏覽器 Profile。

- 有視窗的 Chromium、專用持久化 Profile。
- 操作桌面時自動取得控制權，保留明確的交還、暫停與恢復操作。
- 截圖標註、空白畫布、不透明遮蔽、PNG 複製與下載。
- 用 CLI 或 MCP 交付回饋；Agent 可操作同一個瀏覽器的頁面、原生選單與擴充功能。

## 安裝

需要 macOS 或 Linux、amd64 或 arm64 架構、已啟動的 Docker 與 Compose v2。Docker 主機至少需要 2 CPU，並建議保留約 8 GiB 記憶體。Chromium 自己的 renderer sandbox 預設關閉，任何 Docker 主機都能執行；容器本身仍保有非 root 使用者、唯讀根檔案系統、移除的 capability 與 seccomp profile。要開啟 Chromium sandbox，在 `~/.local/share/lensdock/runtime.env` 設定 `LENSDOCK_CHROMIUM_SANDBOX=true`，再用 `lensdock update compose --deployment ~/.local/share/lensdock/deployment.json -- up -d runtime` 重建 Runtime。開啟後主機必須允許 unprivileged user namespace，否則 Runtime 會維持未就緒，不會自動降級。

### 私有儲存庫存取權

此儲存庫與 GHCR 套件需要授權。先安裝 [GitHub CLI](https://cli.github.com/)，再完成一次登入：

```bash
gh auth login --scopes read:packages
gh auth token | docker login ghcr.io --username "$(gh api user --jq .login)" --password-stdin
```

GitHub 帳號必須有儲存庫與套件的存取權。憑證保存在 GitHub CLI 與 Docker 的憑證儲存區；不要放進網址或提交到 Git。

### 一行指令安裝

```bash
bash -o pipefail -c 'gh release download --repo JiaWeiXie/lensdock --pattern install.sh --output - | bash'
```

安裝程式會下載最新的 GitHub Release、驗證壓縮檔的 SHA-256、把 `lensdock` 安裝到 `~/.local/bin`，並啟動新的本機工作站。若該目錄不在 `PATH`，請加入。安裝程式不會覆寫既有部署或升級它的瀏覽器資料。

Agent 主機只需要 CLI 時，加上 `--install-only`：

```bash
bash -o pipefail -c 'gh release download --repo JiaWeiXie/lensdock --pattern install.sh --output - | bash -s -- --install-only'
```

### 透過 mise 安裝

先安裝 [mise](https://mise.jdx.dev/) 並啟用 Shell 整合：

```bash
MISE_GITHUB_TOKEN="$(gh auth token)" mise use -g 'github:JiaWeiXie/lensdock[bin_path=.]@latest'
lensdock admin product-init --state-dir "$HOME/.local/share/lensdock"
lensdock admin product-start --state-dir "$HOME/.local/share/lensdock"
```

`bin_path=.` 會選用主機 CLI，避免選到隨附的 Linux 更新工具。只有 Agent 的主機只需執行第一行。Token 只傳給這次 mise 程序，GitHub CLI 把它存放在系統鑰匙圈時也能使用。其他認證方式見 [mise 的 Token 設定](https://mise.jdx.dev/dev-tools/github-tokens.html)。

mise 預設的 [24 小時發布等待期](https://mise.jdx.dev/security.html#minimum-release-age) 可能讓 `@latest` 暫時找不到新版本。若要安裝當天發布的版本，把 `@latest` 換成明確版本，例如 `@0.3.0`，即可保留全域的發布等待保護。

### 開啟工作站

開啟 **http://127.0.0.1:18081/**，貼上 `~/.local/share/lensdock/product` 下私有 `human-token`（或 `admin-token`）檔案的內容完成配對；`product-start` 會印出檔案路徑。每個瀏覽器 Session 都需要 Token，Gateway 重新啟動會清除 Session，需再配對一次。不要把憑證貼進 Issue、聊天、網址或截圖。若要在 Loopback 單一 Workspace 的 Gateway 免 Token 進入，必須明確選用（見「選用配對與配額」）。

預設 Workspace 為 `default`。設定與憑證存放在 `~/.local/share/lensdock`；瀏覽器 Profile、下載、資料庫與回饋圖片保存在 Docker Volume。另有 `human-token`、`viewer-token` 與 `agent-token`，分別限制各角色的權限。Web UI、遠端 Chromium 與桌面都預設英文。Web UI 可在設定切換繁體中文，並保留已儲存的選擇。

### Docker 映像

| 元件 | 預設映像 |
| --- | --- |
| Browser Runtime | `ghcr.io/jiaweixie/lensdock/runtime:latest` |
| Gateway 與 Web UI | `ghcr.io/jiaweixie/lensdock/gateway:latest` |

兩個映像都包含 Linux amd64 與 arm64。全新安裝預設拉取 `latest`，首次啟動時固定解析後的映像，避免日後重新啟動時無聲升級 Profile。若要固定版本或 Digest，請在首次初始化前設定 `LENSDOCK_RUNTIME_IMAGE` 與 `LENSDOCK_GATEWAY_IMAGE`，並選擇相容的一組映像。既有部署請使用主機端更新流程。

## 多個本機專案

每個專案都有獨立的 Runtime／Gateway、持久化 Volume、憑證、Workspace ID 與固定 Loopback Origin。既有的 18081 獨立工作站不會被遷移。

安裝 Release Bundle 並啟動 Docker 後，直接使用：

```bash
lensdock project up --project "$HOME/code/site-a"
lensdock workspace status --project "$HOME/code/site-a"
lensdock feedback list --project "$HOME/code/site-a"
lensdock mcp serve --project "$HOME/code/site-a"
```

第一次使用專案指令時，會自動初始化 Manager、登記資料夾並啟動工作站。`project up` 未指定路徑時使用目前目錄。帶有 `--project` 的 MCP 與回饋指令也會自動啟動已停止的工作站，不必先執行 `up`。明確指定 `--project` 會忽略過期的 `LENSDOCK_API`、`LENSDOCK_TOKEN_FILE`，但不能同時指定 API、Workspace 或 Token 旗標。

Manager 預設放在 `~/.local/share/lensdock-manager`；若改用其他位置，各程序都要設定一致的 `LENSDOCK_MANAGER_DIR`。初始化會複製主機執行檔與 Bundle，並固定本機 Docker Engine；不會安裝開機服務，也不管理專案的應用程式伺服器或資料庫。首次初始化可用 `LENSDOCK_RUNTIME_IMAGE`、`LENSDOCK_GATEWAY_IMAGE`、`LENSDOCK_PULL` 選擇映像與拉取政策。

`up` 回傳已驗證的 API Origin、Workspace 與 Agent Token 檔案位置。開啟該 Origin，並用該專案狀態目錄下的私有 `human-token` 配對（`lensdock admin project list --json` 會回傳 `stateDir`，檔案為 `<stateDir>/product/human-token`）。不同 Gateway 即使用相同主機名稱，Session 與本機登出狀態也互不影響。MCP 請按專案設定，詳見 [Agent 整合](docs/AGENT-INTEGRATION.md#project-scoped-local-agents)。

用工具列的「專案 / Workspace」切換本機瀏覽器。切換會先釋放目前的控制租約，再開啟選定的工作站；不會停止任一瀏覽器、重新載入頁面或中斷背景下載。釋放租約失敗時會留在原工作站。每個工作站各自使用自己的 Session 與憑證。

已啟動的部署會在 `~/.local/share/lensdock-destinations` 共用不含機密的探索資料。若要改用其他資料夾，請在初始化前一致設定 `LENSDOCK_DESTINATIONS_DIR`。既有部署需更新 CLI 與 Gateway，再執行一次 `admin product-start`，加入唯讀探索掛載；原有 Compose 副本與資料不會被替換。映像仍走正常更新流程，不要用 `product-init` 升級。

CPU、記憶體、工具程序數及共用資源預算預設不設上限；外部容器沒有配額也不會擋住啟動。需要限制時，用 `admin manager policy` 設定。既有專案停止後，可用 `admin project limits --id ID ...` 調整；`0` 移除該上限，未指定的項目維持原值。

若要人工核准專案或手動啟動，可開啟 `--require-project-authorization`、`--require-explicit-start`。原有的 `admin manager init`、`admin project authorize` 仍可使用。既有政策與已撤銷的授權不會被自動重設。

只有主動啟用的預算滿載時才會依 FIFO 等待，不會自動停止其他瀏覽器。同專案的請求共用一個操作。等待預設不逾時，需要時再指定 `--wait`。取消等待只影響該呼叫者，已接納的工作繼續執行。釋放租約、關閉 MCP、暫停 Agent 或關閉 UI 都不會停止 Chromium。

確定要捨棄記憶體中的頁面與網路工作時，明確停止：

```bash
lensdock project stop --project "$HOME/code/site-a"
```

持久化 Profile 與回饋會保留。只有開啟 `--require-stop-confirmation` 才需要 `--discard-page-state`。有租約時仍拒絕一般停止，強制停止則需提供觀察到的 Runtime Generation。搬移、替換或明確撤銷的資料夾需要人工處理，不會自動修復授權。Manager Socket 可防止誤連專案，無法隔離同一 OS 使用者下的惡意程序。政策與復原方式見 [更新 Runbook](docs/UPDATE-RUNBOOK.md#managed-project-lifecycle)。

## 使用方式

1. 點擊桌面或在桌面輸入時，自動申請控制權。若 Agent 正在操作，先完成交接。觸發交接的第一個動作不會重播到已改變的頁面；看到控制狀態就緒後再繼續。
2. 用文字與檔案面板貼上本機輸入法完成的文字，或上傳、下載檔案。
3. 擷取畫面或建立繪圖，加上箭頭、圖形、文字或重點標記，再按「送出」。備註可留空。秘密資料請用不透明遮蔽；模糊與馬賽克只有視覺效果。
4. 回饋面板顯示「已送出／處理中／已完成」。展開細節可區分排隊與已讀，以及 Agent 完成與你的驗證結果。

「複製 PNG」與「下載 PNG」會輸出攤平的圖片，不會發布回饋。關閉 UI 不會關閉 Chromium 或清除 Profile。

瀏覽器視窗最小化或不見時，在**遠端空白桌面**按右鍵，再選 **Restore browser**。它會叫回原有視窗，或在同一個 Chromium 與 Profile 中開啟視窗。若第一次點擊正在取得控制權，等控制狀態就緒後再按一次右鍵。網頁內的右鍵仍開啟 Chromium 原本的選單。

### 選擇使用模式

預設 `AGENT` 提供標註、回饋與完整瀏覽器控制，包含網址列、選單、原生對話框及擴充功能。`NOVNC` 只提供遠端瀏覽器、人工操作、剪貼簿與檔案傳輸。若首次啟動只需要瀏覽器，請在安裝前設定 `LENSDOCK_MODE=NOVNC`，或在 `product-init` 加上 `--mode NOVNC`。

之後可在設定切換模式。「暫停 Agent」會立即撤銷 Agent 控制權，你仍能截圖與撰寫回饋。「恢復 Agent」只允許申請新租約，不會恢復舊租約。模式與暫停狀態會跨重新啟動保存；修改啟動環境變數不會覆蓋已保存的設定。

### 選用配對與配額

所有瀏覽器 Session 預設都需要配對 Token；Origin 不是 Loopback 或服務多個 Workspace 的 Gateway 只能以此方式使用。免 Token 的本機自動進入需明確選用，僅限 Loopback 單一 Workspace 的 Gateway：初始化時加上 `--local-auto-entry`（`admin product-init`，受管理專案用 `admin manager init`），或在 `product/compose.env` 設定 `LENSDOCK_LOCAL_AUTO_ENTRY=1` 後重新啟動 Gateway。它信任請求標頭，因此主機上任何連得到 Gateway 的程序（包括本機 Proxy）都能不用 Token 開啟 human Session。Sec-Fetch 標頭、Origin 檢查與瀏覽器的 Local Network Access 提示只約束網頁，擋不住這類程序。只適合沒有這類軟體的單人電腦。`--require-pairing` 或 `LENSDOCK_REQUIRE_PAIRING=1` 永遠優先。自動進入會以 `session.local` 記錄在稽核紀錄。尚未設定任何一個選項的既有 Gateway，更新到此版本後即需配對。

Session 預設沒有應用程式層級的期限或數量上限。回饋圖片、草稿、檔案傳輸與剪貼簿不設產品配額，草稿也不會到期。需要限制時才填入正值，`0` 表示停用。設定位置見 [Runbook](docs/UPDATE-RUNBOOK.md#resource-policy)。實際可用的記憶體、磁碟空間及瀏覽器格式限制仍適用。

### 共用主機或伺服器

預設值適合一個人在自己的電腦上使用。在共用主機、伺服器或雲端 VM，請用伺服器設定檔初始化：`lensdock admin product-init --profile server`（受管理專案用 `lensdock admin manager init --profile server`）。它會開啟 Chromium renderer sandbox、要求配對，並為 Runtime 與 Gateway 設定記憶體、CPU 與程序數上限。上限（Runtime 4 GiB、2 CPU、2048 個程序；Gateway 1 GiB、1 CPU、512 個程序）只是起點，請在實際主機量測。獨立部署可編輯 `product/compose.env`；受管理專案以 manager policy 為準，初始化的資源旗標會覆寫設定檔預設值，明確指定 `0` 代表不限，未指定的項目則保留伺服器預設值。後續可用 `lensdock admin manager policy` 調整新專案的預設值，用 `lensdock admin project limits` 調整已停止的既有專案；修改 compose.env 無法覆寫受管理的 resources.yaml。設定檔只在建立 `runtime.env` 時寫入 `LENSDOCK_CHROMIUM_SANDBOX=true`；既有的狀態目錄請自行加上這行，否則初始化會停止。伺服器設定檔只接受空白／註解行及單行 `KEY=plain-value`，sandbox 設定必須以標準格式出現一次；export、冒號分隔、引號、多行值及變數插值一律拒絕，也不會改寫檔案，請先整理格式再重試。Sandbox 需要主機允許非特權 user namespace，否則 Runtime 會維持未就緒，不會退回無 sandbox。設定檔不限制網路出口：Runtime 仍可連到網際網路、主機服務、區網與 `169.254.169.254`，請加上主機防火牆規則（見 `docker/browser/README.md`），不要把 Agent 不該碰的帳號放進 Agent 可租用的 Profile，並考慮 `LENSDOCK_CDP_DENY=cookies`（[Agent 整合](docs/AGENT-INTEGRATION.md)）。

## 設置 Agent

LensDock 連接你現有的 Coding Agent，不執行 LLM，也不要求安裝 Agent Skill。

1. 用上述任一方式在 Agent 主機安裝 CLI，並在設定保持開啟 Agent 協作。
2. 透過私有管道提供該 Workspace 的 `product/agent-token`，保留檔案權限 `0600`。瀏覽器操作不需要 Host Admin 憑證。
3. 從設定複製英文提示詞，加入 Agent 指示。另行提供實際 API URL 與 Token 檔案路徑；複製的提示詞不含憑證。

Agent 與 LensDock 在同一台主機時：

```bash
export LENSDOCK_API=http://127.0.0.1:18081
export LENSDOCK_TOKEN_FILE="$HOME/.local/share/lensdock/product/agent-token"
lensdock feedback list --workspace default
lensdock workspace watch default
```

讓 `workspace watch` 在獨立程序持續執行。收到暫停、`NOVNC` 或連線中斷時，停止瀏覽器工作。人工恢復後，下一次操作指令會自動取得新租約。通知只回報狀態，不能證明模型已讀取。

### Codex

先取得 CLI 與 Token 檔案路徑。透過 mise 安裝時，把 `command -v lensdock` 換成 `mise which lensdock`：

```bash
LENSDOCK_BIN="$(command -v lensdock)"
TOKEN_FILE="$HOME/.local/share/lensdock/product/agent-token"
codex mcp add lensdock -- "$LENSDOCK_BIN" mcp serve --workspace default \
  --api http://127.0.0.1:18081 --token-file "$TOKEN_FILE"
codex mcp get lensdock
```

設定會寫入 `~/.codex/config.toml`。開啟新的 Codex Session，執行 `/mcp` 檢查連線。

### Claude Code

沿用上面的變數：

```bash
claude mcp add --transport stdio --scope user lensdock -- "$LENSDOCK_BIN" mcp serve \
  --workspace default --api http://127.0.0.1:18081 --token-file "$TOKEN_FILE"
claude mcp get lensdock
```

`--scope user` 讓所有專案都能使用這個 Server。開啟新的 Claude Code Session，執行 `/mcp`。

### OMP 與其他 MCP Client

OMP 請把以下項目合併到 `~/.omp/agent/mcp.json`；只供單一專案使用時，放在 `.omp/mcp.json`。支援 `mcpServers` 的其他 Client 也可使用相同設定。把 `/Users/alice` 換成自己的家目錄，並使用上面取得的 CLI 絕對路徑；JSON 不會展開 `$HOME` 或 `~`。

```json
{
  "mcpServers": {
    "lensdock": {
      "type": "stdio",
      "command": "/Users/alice/.local/bin/lensdock",
      "args": [
        "mcp", "serve", "--workspace", "default",
        "--api", "http://127.0.0.1:18081",
        "--token-file", "/Users/alice/.local/share/lensdock/product/agent-token"
      ]
    }
  }
}
```

OMP 依序執行 `/mcp reload`、`/mcp list`、`/mcp test lensdock`。其他 Client 請重新啟動或重新載入 MCP 設定。

MCP 提供五種回饋工具：清單、Metadata、圖片、確認與解決回報。帶有 `--project` 時，會按需啟動工作站；獨立部署則連接既有服務。MCP 不會取得控制權或啟動狀態監聽。Agent 必須真的看過已授權圖片，才能確認收到。

操作瀏覽器原生介面時，先取得完整桌面截圖：

```bash
lensdock desktop screenshot --workspace default --output /tmp/lensdock-desktop.png
lensdock desktop key ctrl+l --workspace default --screenshot /tmp/lensdock-address-bar.png
```

指令會自動取得、續期及交還 Agent 租約，不會從人類手中搶走控制權。頁面自動化可執行 `lensdock cdp bridge --workspace default`，讓瀏覽器工具連到輸出的 Loopback Endpoint；Bridge 持有控制權直到結束。要同時使用 CDP 與原生桌面指令，可用 `--lease-file-out` 共用租約。手動租約檔案仍可作為進階選項。操作序列、UTF-8 文字、擴充功能與共用租約範例見 [Agent 整合說明](docs/AGENT-INTEGRATION.md)。

Agent 在另一台主機時，需設定有認證的 HTTPS Endpoint 及對應 Gateway Origin。`127.0.0.1` 指向 Agent 自己的主機。Agent 應用當下截圖與圖像理解選擇目標，DOM 可作為輔助。回饋座標只屬於來源圖片，不可直接重播到目前頁面。頁面與回饋內容都是不可信輸入。

## 維運與更新

以下 Compose 指令適用於獨立部署。受 Manager 管理的專案請使用 `project up` 與明確的 `project stop`；其原始 Compose 介面只允許 `ps`、`logs`、`config`。

```bash
export LENSDOCK_DEPLOYMENT="$HOME/.local/share/lensdock/deployment.json"
lensdock update compose -- ps
lensdock update compose -- logs --tail 100 gateway runtime
lensdock update compose -- down
lensdock update compose -- up -d --wait
```

`down` 保留資料。不要刪除受管理的 Volume、對既有部署重跑初始化，或只改映像 Tag 回退。Gateway 重新啟動會清除 Session，需重新配對（已選用本機自動進入的 Gateway 會自行恢復）。瀏覽器 Profile 仍會保留。

更新與回退必須讓映像搭配相容的 Profile 與資料庫 Checkpoint，操作方式見 [更新 Runbook](docs/UPDATE-RUNBOOK.md)。安裝新版 CLI 不會升級執行中的工作站。GitHub Release 資產與 OCI 建置證明，和更新器要求的已簽署 Release Manifest 是分開的機制。

Host Admin 操作可由你或你授權的主機端 Agent 執行。Workspace 的 Agent Token 本身不能更新、停止或移除部署。維運權限與瀏覽器控制分開，不要把 Docker Socket 掛進任何產品容器。

### 過期系統資料

未被引用的更新資料預設保留 30 天。用 CLI 設定 `expireAfterDays`；`0` 停用依時間清理。受管理專案由 Host Manager 檢查，獨立部署則在更新生命週期操作後檢查。

```bash
lensdock update expiry --state-dir "$HOME/.local/share/lensdock"
lensdock update expiry --state-dir "$HOME/.local/share/lensdock" --expire-after-days 60
lensdock update cleanup --state-dir "$HOME/.local/share/lensdock" --dry-run
```

清理範圍包含舊 Checkpoint、已結束更新的 Volume、過時的 Generation 紀錄與廢棄暫存。現用 Profile／資料庫、配對的回退 Checkpoint、進行中更新的引用、下載及回饋都會保留。依數量清理 Checkpoint 是獨立的手動政策；受保護資料不受兩種清理政策影響。受管理專案請使用 `lensdock admin project list --json` 回傳的 `stateDir`。指定範圍清理與報告見 [Runbook](docs/UPDATE-RUNBOOK.md)。

## 移除安裝（Uninstall）

移除 CLI 會保留工作區資料。先結束 MCP Server、Watcher 與 Bridge，避免它們重新啟動受管理工作區。受管理專案用 `lensdock project stop --project /absolute/project/path` 停止；獨立部署用 `lensdock update compose --state-dir /absolute/state/path -- down`。接著執行：

```bash
lensdock admin manager stop
```

這只會停止 Host Manager 並等待它結束；有 Worker 執行中時會拒絕。它不會停止容器或刪除資料。自訂安裝位置請沿用相同的 `LENSDOCK_MANAGER_DIR`。

透過 mise 安裝時，先移除 LensDock 的全域選擇、預覽版本，再只移除這個工具：

```bash
mise unuse --global --no-prune github:JiaWeiXie/lensdock
mise uninstall --all github:JiaWeiXie/lensdock --dry-run
mise uninstall --all github:JiaWeiXie/lensdock
```

專案層級的設定另用 `mise unuse --path /exact/mise.toml --no-prune github:JiaWeiXie/lensdock` 移除；不要刪掉共用的 mise 設定檔。

Shell 安裝程式使用者先檢查 `readlink "$HOME/.local/bin/lensdock"`，再移除所屬的 Symbolic Link 與 `~/.local/lib/lensdock` 下的 LensDock 版本目錄。若設定過 `LENSDOCK_BIN_DIR`、`LENSDOCK_INSTALL_DIR`，請使用實際位置；自訂安裝父目錄可能還有其他檔案，不可整個刪除。

若要重新使用資料，請保留 `~/.local/share/lensdock`、`~/.local/share/lensdock-manager`、自訂備份位置及加密金鑰。Manager 目錄包含自己的執行檔副本、憑證與所有受管理專案的狀態。

若要永久刪除資料，請在刪除部署 Metadata 或 CLI **之前**，依照 [Uninstall and data removal](docs/UPDATE-RUNBOOK.md#uninstall-and-data-removal) 清點容器、Volume、Checkpoint 與金鑰。更新建立的 Volume 可能沒有 Compose 標籤，不能只靠標籤清單。不要執行全域 Docker prune，也不要刪除應用程式的專案資料夾。刪除後沒有復原按鈕，只能用相容備份及其金鑰還原。


## 開發與發版

```bash
mise install
mise run build
mise run build:linux
mise run web:build
mise run image:build
mise run gateway:image
mise run product:init
mise run product:up
```

原始碼開發使用本機 `:dev` 映像與 `.lensdock/` 狀態目錄。提交前請執行 `mise run check` 與 `mise exec -- pnpm --dir apps/web test`。

`main` 的變更通過檢查後，產生版本化的 GitHub Release 與 GHCR 映像。流程在兩種原生 Linux 架構建置、執行容器 Smoke Check，並收集映像供應鏈證據。`latest` 指向最新成功的穩定發行。Release 壓縮檔包含 macOS／Linux CLI、部署設定、Linux 更新工具與 Checksum。

## 參考文件與限制

- [Agent 整合](docs/AGENT-INTEGRATION.md)、[更新 Runbook](docs/UPDATE-RUNBOOK.md)、[Web UI 細節](apps/web/README.md)。
- 整合瀏覽器與更新流程已在 macOS arm64／OrbStack 實測。原生發版 Smoke Check 不代表每個平台都通過完整產品 E2E。日文／韓文原生輸入法、統一的多 Workspace UI 與跨主機 Checkpoint 移轉仍不在已驗證範圍。
- 未設定 TLS 與認證前，保持預設 Loopback 綁定。不可把 Docker Socket 掛進瀏覽器、Gateway 或 Agent。
- LensDock 尚未選定專案授權；隨附依賴各自保留原授權，見 [參考來源](docs/REFERENCES.md)。
