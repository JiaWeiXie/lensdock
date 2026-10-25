# LensDock

[English](README.md)

讓你和 Coding Agent 共用同一個持久化 Chromium 工作站。用 noVNC 觀看真實瀏覽器、隨時接管操作，再把標註截圖交給 Agent，不必分享日常使用的瀏覽器 Profile。

- 有視窗的 Chromium、專用持久化 Profile，以及保持啟用的 Sandbox。
- 人工接管、伺服器強制的 Agent 租約，以及暫停與恢復控制。
- 截圖標註、空白畫布、不透明遮蔽、PNG 複製與下載。
- 用 CLI 或 MCP 交付回饋，透過租約限定的 CDP Bridge 控制瀏覽器。

## 安裝

需要 macOS 或 Linux、amd64 或 arm64 架構、已啟動的 Docker 與 Compose v2。主機必須支援 Chromium 的 User Namespace Sandbox，Docker 主機至少需要 2 CPU，並建議保留約 8 GiB 記憶體。LensDock 不會為了啟動而關閉 Sandbox。

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

### 開啟工作站

開啟 **http://127.0.0.1:18081/**，用 `~/.local/share/lensdock/product/admin-token` 的內容配對。請在本機讀取這個私有檔案，不要貼進 Issue、聊天、網址或截圖。

預設 Workspace 為 `default`。設定與憑證存放在 `~/.local/share/lensdock`；瀏覽器 Profile、下載、資料庫與回饋圖片保存在 Docker Volume。另有 `human-token`、`viewer-token` 與 `agent-token`，分別限制各角色的權限。Web UI、遠端 Chromium 與桌面都預設英文。Web UI 可在設定切換繁體中文，並保留已儲存的選擇。

### Docker 映像

| 元件 | 預設映像 |
| --- | --- |
| Browser Runtime | `ghcr.io/jiaweixie/lensdock/runtime:latest` |
| Gateway 與 Web UI | `ghcr.io/jiaweixie/lensdock/gateway:latest` |

兩個映像都包含 Linux amd64 與 arm64。全新安裝預設拉取 `latest`，首次啟動時固定解析後的映像，避免日後重新啟動時無聲升級 Profile。若要固定版本或 Digest，請在首次初始化前設定 `LENSDOCK_RUNTIME_IMAGE` 與 `LENSDOCK_GATEWAY_IMAGE`，並選擇相容的一組映像。既有部署請使用主機端更新流程。

## 使用方式

1. 先觀看桌面；需要操作時取得控制權，或從 Agent 接管。
2. 用文字與檔案面板貼上本機輸入法完成的文字，或上傳、下載檔案。
3. 擷取畫面或建立繪圖，加上箭頭、圖形、文字或重點標記，再按「送出」。備註可留空。秘密資料請用不透明遮蔽；模糊與馬賽克只有視覺效果。
4. 檢查 Agent 回覆的證據，再標記驗證通過。已排隊、已確認、已解決與驗證通過是不同狀態。

「複製 PNG」與「下載 PNG」會輸出攤平的圖片，不會發布回饋。關閉 UI 不會關閉 Chromium 或清除 Profile。

### 選擇使用模式

預設 `AGENT` 提供標註、回饋與 Agent 瀏覽器控制。`NOVNC` 只提供遠端瀏覽器、人工操作、剪貼簿與檔案傳輸。若首次啟動只需要瀏覽器，請在安裝前設定 `LENSDOCK_MODE=NOVNC`，或在 `product-init` 加上 `--mode NOVNC`。

之後可在設定切換模式。「暫停 Agent」會立即撤銷 Agent 控制權，你仍能截圖與撰寫回饋。「恢復 Agent」只允許申請新租約，不會恢復舊租約。模式與暫停狀態會跨重新啟動保存；修改啟動環境變數不會覆蓋已保存的設定。

### 本機免 Token 展示

僅供可信任的單人本機展示：在部署的 `product/compose.env` 設定 `LENSDOCK_DEMO_NO_AUTH=1`，再重新啟動 Gateway。這會允許 Loopback 上的自動人工配對。一般使用請保留預設值 `0`；不要用 Proxy 或 Tunnel 對外開放展示模式。

## 設置 Agent

LensDock 連接你現有的 Coding Agent，不執行 LLM，也不要求安裝 Agent Skill。

1. 用上述任一方式在 Agent 主機安裝 CLI，並在設定保持開啟 Agent 協作。
2. 透過私有管道提供該 Workspace 的 `product/agent-token`，保留檔案權限 `0600`。不要提供 Admin Token 或 Docker 存取權。
3. 從設定複製英文提示詞，加入 Agent 指示。另行提供實際 API URL 與 Token 檔案路徑；複製的提示詞不含憑證。

Agent 與 LensDock 在同一台主機時：

```bash
export LENSDOCK_API=http://127.0.0.1:18081
export LENSDOCK_TOKEN_FILE="$HOME/.local/share/lensdock/product/agent-token"
lensdock feedback list --workspace default
lensdock workspace watch default
```

讓 `workspace watch` 在獨立程序持續執行。收到暫停、`NOVNC` 或連線中斷時，停止瀏覽器工作。人工恢復後必須申請新租約。通知只回報狀態，不能證明模型已讀取。

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

MCP 提供五種回饋工具：清單、Metadata、圖片、確認與解決回報。它不會啟動瀏覽器、申請租約或執行狀態監聽。Agent 必須真的看過已授權圖片，才能確認收到。瀏覽器操作使用 `lensdock lease acquire` 與 `lensdock cdp bridge`；瀏覽器工具應連接這個 Bridge，不可直接使用未受限制的 Chromium Port。完整流程、Codex TOML 與工具 Schema 見 [Agent 整合說明](docs/AGENT-INTEGRATION.md)。

Agent 在另一台主機時，需設定有認證的 HTTPS Endpoint 及對應 Gateway Origin。`127.0.0.1` 指向 Agent 自己的主機。頁面與回饋內容都是不可信輸入；依截圖座標操作之前，必須重新觀察目前頁面。

## 維運與更新

```bash
export LENSDOCK_DEPLOYMENT="$HOME/.local/share/lensdock/deployment.json"
lensdock update compose -- ps
lensdock update compose -- logs --tail 100 gateway runtime
lensdock update compose -- down
lensdock update compose -- up -d --wait
```

`down` 保留資料。不要刪除受管理的 Volume、對既有部署重跑初始化，或只改映像 Tag 回退。Gateway 重新啟動後需要重新配對，但瀏覽器 Profile 仍會保留。

更新與回退必須讓映像搭配相容的 Profile 與資料庫 Checkpoint，操作方式見 [更新 Runbook](docs/UPDATE-RUNBOOK.md)。安裝新版 CLI 不會升級執行中的工作站。GitHub Release 資產與 OCI 建置證明，和更新器要求的已簽署 Release Manifest 是分開的機制。

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

CI/CD 參考 [comfyui-pwa-client](https://github.com/JiaWeiXie/comfyui-pwa-client)：`main` 的變更通過檢查後，產生版本化的 GitHub Release 與 GHCR 映像。流程在兩種原生 Linux 架構建置、執行容器 Smoke Check，並收集映像供應鏈證據。`latest` 指向最新成功的穩定發行。Release 壓縮檔包含 macOS／Linux CLI、部署設定、Linux 更新工具與 Checksum。

## 參考文件與限制

- [Agent 整合](docs/AGENT-INTEGRATION.md)、[更新 Runbook](docs/UPDATE-RUNBOOK.md)、[Web UI 細節](apps/web/README.md)。
- 完整人機交接與成對更新示範是在 macOS arm64／OrbStack 執行。原生發版 Smoke Check 不代表每個平台都通過完整產品 E2E。日文／韓文原生輸入法、多 Workspace 管理與跨主機 Checkpoint 移轉仍不在已驗證範圍。
- 未設定 TLS 與認證前，保持預設 Loopback 綁定。不可把 Docker Socket 掛進瀏覽器、Gateway 或 Agent。
- LensDock 尚未選定專案授權；隨附依賴各自保留原授權，見 [參考來源](docs/REFERENCES.md)。
