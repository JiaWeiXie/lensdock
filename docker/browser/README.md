# Browser Runtime 映像

這個目錄是 Browser Runtime 映像。M0 在 OrbStack（arm64）與 Ubuntu 22.04（amd64）通過 SPIKE-01～04 的整合測試。產品 Compose（`docker/compose.yaml`）把此映像與 Gateway 組在一起，目前只以開發版（未簽署、無 Provenance 與掃描）Release 在 macOS arm64／OrbStack 做過整合實測；整合版 amd64 與簽署的正式 Release 尚未驗證。更新與回退見 [UPDATE-IMPLEMENTATION.md](../../docs/UPDATE-IMPLEMENTATION.md)，實測範圍見 [ACCEPTANCE.md](../../docs/ACCEPTANCE.md#evidence-status)。

## 檔案

| 檔案 | 用途 |
|---|---|
| `Dockerfile` | 三段建置：Go 編譯 Runtime、Debian 安裝套件、組合最終映像 |
| `packages.txt` | 安裝的套件名稱；實際版本在建置時寫入 `/usr/share/lensdock/dpkg-manifest.tsv` |
| `fontconfig.reference.conf` | 安裝成 `/etc/fonts/local.conf`；處理 BCP 47 `zh-Hant`／`zh-Hans` 與 Emoji 後備 |
| `seccomp/chromium.json` | 由 `tools/gen_seccomp.py` 產生，勿手改 |
| `compose.yaml` | 單獨啟動 Runtime（M0；userns Sandbox、唯讀根目錄、`cap_drop: ALL`）。完整產品用 `docker/compose.yaml` |
| `downloads-policy.json` | 安裝成 Chromium 託管政策，下載落在持久的 `/data/downloads` |
| `openbox/` | 視窗管理器設定 |

## 使用

```bash
mise run image:build      # 建置 lensdock/runtime:dev；LENSDOCK_ARCH 預設跟主機架構，可用環境變數覆寫
mise run runtime:init     # 產生 .lensdock/operator-token 與 runtime.env（0600）
mise run runtime:up       # 啟動並等待 healthy
mise run runtime:down     # 停止；Profile／Downloads Volume 保留
```

M0 的 API 只發布在 `127.0.0.1:18080`。CDP（9223）與 VNC（5900 控制、5901 唯讀）只在 Container 的 loopback。映像的 `HEALTHCHECK` 依 `LENSDOCK_LISTEN` 的埠探測 `/readyz`，改監聽埠時不必另外調整。產品 Compose 完全不發布 Runtime 的埠：Gateway 只發布 `127.0.0.1:18081`，並以 mutual TLS 經內部 `control` 網路連 Runtime。

## Usage mode and policy volume

The Runtime has two usage modes: `AGENT` (default, so existing deployments keep their behavior) and `NOVNC` (a plain remote browser with every Agent feature disabled). The README describes what each mode does for users; this section covers how the image takes the setting.

**Choose the initial mode.**

- `LENSDOCK_MODE=NOVNC` or `AGENT` in the container environment. The value is case-insensitive and any other value stops startup. Both Compose files pass `LENSDOCK_MODE: ${LENSDOCK_MODE:-AGENT}`, so set the variable in the environment of the Compose command.
- `--mode NOVNC|AGENT` as container arguments. The image `ENTRYPOINT` is `tini -g -- lensdock-runtime supervise`, so Docker or Compose appends your arguments to it and `--mode` reaches `supervise`. Do not repeat `supervise`. When both are present, the flag overrides `LENSDOCK_MODE`.

```yaml
# Compose: arguments are appended to the ENTRYPOINT
services:
  runtime:
    command: ["--mode", "NOVNC"]
```

```text
docker run [options as in docker/browser/compose.yaml] lensdock/runtime:dev --mode NOVNC
```

**Initial means once.** The flag and the environment variable apply only until a policy has been saved. After that, the saved mode and pause state win, including after a restart, so a container started with `LENSDOCK_MODE=AGENT` stays `NOVNC` or paused if a user chose that in Settings. Change a saved policy through the Settings page or the workspace API, not through the startup setting.

**Policy directory and security.** The policy is stored in `agent-control.json` inside `/data/runtime` (override with an absolute, clean `LENSDOCK_POLICY_DIR`). Both Compose files mount the dedicated `lensdock-runtime-policy` volume there, and the Dockerfile creates the directory with mode `0700` for uid 10000.

- The supervisor (uid 10000) owns the directory and writes the file with mode `0600`. Chromium and the other browser processes run as uid 10001 and cannot read or write the policy. A host `docker exec` check confirmed this for uid 10001 and confirmed `0700` and `0600` ownership by 10000:10000 for uid 10000.
- The directory must already exist as a mount point; the read-only root filesystem never gets it created. A fresh root-owned volume is handed to the supervisor with mode `0700` before privileges are dropped. A directory with any other owner or mode is refused.
- A corrupt policy file stops startup instead of defaulting to `AGENT`. Writes are atomic. If a pause or disable cannot be saved, the running process still fences the Agent for that session, but the saved state may be older after a restart.
- The policy volume is separate from `/data/profile`. Do not remove it with `down -v` or a volume prune: without a saved policy, the initial mode applies again.
- An existing installation must [enroll the policy volume](../../docs/UPDATE-IMPLEMENTATION.md#adding-the-runtime-policy-volume) through the update workflow before upgrading. A Runtime image from before this feature does not read or enforce the policy, so rolling back to one drops the mode and pause restrictions.

## 建置行為

- 基底與 Go 建置映像都釘選 Multi-arch Index Digest；`mise run check:pins` 確認 Go 版本在 `mise.toml`、`go.mod`、Dockerfile 一致。
- 同一個 RUN 內 `apt-get update` 與 `apt-get install --no-install-recommends`，最終映像沒有 curl／wget、編譯器或 apt 索引。
- 映像內不得有 `/usr/lib/chromium/chrome-sandbox`（建置時檢查）。
- Runtime Binary 以 `CGO_ENABLED=0` 建置（降權時需要 `AllThreadsSyscall`）。
- `.dockerignore` 採允許清單，`.lensdock/`、`.git`、`tests/` 不會進入建置 Context。

## 執行期安全設定

- Sandbox：非特權 User Namespace＋自訂 Seccomp（[ADR-0003](../../docs/adr/0003-chromium-sandbox.md)）。Preflight 失敗時 Runtime 鎖為 `SANDBOX_PREFLIGHT_FAILED`，不啟動 Chromium，不會退回 `--no-sandbox`。
- 身分：tini → Supervisor 以 root 準備目錄後降為 UID 10000；X、VNC、Chromium、xclip 以 UID 10001 執行（[ADR-0002](../../docs/adr/0002-runtime-identity.md)）。
- Compose：`read_only: true`、`no-new-privileges`、`cap_drop: ALL` 後只加回 `CHOWN SETUID SETGID SETPCAP KILL`。只有 root 的 tini 保留 `KILL`（轉發停止訊號），Supervisor 降權後只剩 `SETUID SETGID`。
- Chromium 參數固定，沒有設定入口能加旗標；不使用 `--remote-allow-origins`（Proxy 拒絕帶 Origin 的請求，上游連線不帶 Origin），也不預設 `--disable-gpu`。
- 出站網路：預設不限制，Container 走 Docker 預設 bridge，Chromium 可連到網際網路、主機上的服務、區網與雲端 Metadata（`169.254.169.254`）。產品 Compose 的 Runtime 同時在內部 `control` 網路與一般的 `browser-egress` 網路上，出站同樣不限制；只有更新時的候選 Runtime 在沒有出站路由的內部網路上驗證。這是刻意的本機使用預設，方便測主機或區網上的開發服務（Container 內的 `localhost` 是 Container 自己，不是主機）。SPEC §14.1 的出站限制尚未實作，沒有內建的嚴格模式開關；在共用主機或雲端 VM 上執行時，請在主機層加規則（例如 Linux 的 `DOCKER-USER` iptables Chain 擋 `169.254.0.0/16` 與主機位址）。OrbStack／Docker Desktop 的網路由 VM 處理，這類主機規則不適用。
- 正常停止：Supervisor 先送 `Browser.close`（等待上限 3 秒）再退回訊號，因為 SIGTERM 路徑不清除 Profile Lock。整合版實測 `profile.exit_type` 為 Normal 且沒有 `Singleton*` Lock（`normal-shutdown.json`，私有證據）；SIGKILL 後的重啟也測過。更新流程以同樣的條件（Exit Code、Lock、`profile.exit_type`、SQLite Journal）判斷 Checkpoint 是否乾淨。

## 仍需檢查

- Ubuntu 23.10 以後 `apparmor_restrict_unprivileged_userns=1` 的主機。
- 「未正確關閉」提示的畫面行為沒有記錄。
- GPU／Software Rendering 的支援矩陣。
