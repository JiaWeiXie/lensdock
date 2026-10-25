# SPIKE-01：Debian Chromium Sandbox

結論：**Debian slim 上的非 root Headful Chromium 可以在 Container 內保持 Sandbox，不需要 `--no-sandbox`、`--privileged` 或 `seccomp=unconfined`。** 採用 User Namespace＋自訂 Seccomp，SUID 變體已刪除，決策見 [ADR-0003](../adr/0003-chromium-sandbox.md)；身分與 Process 樹見 [ADR-0002](../adr/0002-runtime-identity.md)。

對應驗收：RT-01、RT-02 通過（兩台主機）。

## 版本矩陣

| 項目 | OrbStack（開發機） | nuci3（Linux 主機） |
|---|---|---|
| Run ID | `orb-final-20261024T155400Z` | `nuci3-final-20261024T155601Z` |
| 主機 | macOS 26.6.2 arm64，OrbStack VM kernel 7.2.9-orbstack | Ubuntu 22.04.5，kernel 6.8.0-138-generic，x86_64 |
| Docker | Engine 29.8.2，Compose v5.5.0，buildx 0.37.2 | Engine 29.1.3，Compose 2.40.3，無 buildx |
| 安全選項 | seccomp builtin、cgroupns；無 AppArmor | AppArmor（Container 為 `docker-default (enforce)`）、seccomp builtin、cgroupns |
| userns sysctl | `max_user_namespaces=64271` | `unprivileged_userns_clone=1`、`max_user_namespaces=28793`、`apparmor_restrict_unprivileged_userns=0` |
| 映像 | `sha256:2a92259711bd…07bc77`（arm64） | `sha256:0d9b151573ce…8660e5`（amd64） |
| 基底 | `debian:trixie-slim@sha256:62bd423aeee9…f034ad` | 同左 |
| Chromium | 156.0.8078.47-1~deb13u1（`Chrome/156.0.8078.47`） | 同左 |
| 其他套件 | x11vnc 0.9.17-1、xvfb 21.1.16-1.3+deb13u4、openbox 3.6.1-12+b2、tini 0.19.0、fontconfig 2.15.0、fonts-noto-cjk 20240730 | 同左 |
| 原始碼 | 未 Commit；source-tree-sha256 `dc0b6e087e5c…8e6bf1` | 映像與 Binary 由同一棵樹在 Mac 交叉建置後 `docker load` |

amd64 映像在 Mac 以 buildx 建置後傳到 nuci3，因為 nuci3 沒有 buildx。

## 做法

1. **RT-01**（`tests/e2e/rt01-image.sh`）：檢查 `os-release`、沒有 Alpine／apk、基底 Digest Label 等於 Dockerfile 釘選值、`packages.txt` 每個套件都在 dpkg manifest、`chromium-sandbox` 不存在。
2. **RT-02**（`TestRT02`）：讀 `/readyz?detail=1` 的 Supervisor Sandbox 報告、Container 行程表，並經 CDP 打開 `chrome://sandbox`。
3. **Sandbox Matrix**（`tests/e2e/sandbox-matrix.sh`）：同一映像在不同安全設定下啟動，每格收集 readyz、租約嘗試、行程表、`docker inspect` 安全選項、Container Log。

## 結果

| Matrix Cell | OrbStack | nuci3 | 期望 |
|---|---|---|---|
| userns＋自訂 Seccomp＋no-new-privileges | Ready，12 個 Chromium 行程，Sandbox Verified | 同左 | Ready |
| userns＋Docker 預設 Seccomp | 不 Ready，`SANDBOX_PREFLIGHT_FAILED`，0 個 Chromium 行程，租約被拒 | 同左 | 鎖定、不降級 |
| SUID（已刪除；刪除前實測） | Ready，13 個 Chromium 行程 | 同左 | 記錄 |

所有格都沒有出現 `--no-sandbox`。RT-01 兩台都 23 項通過。`chrome://sandbox` 兩台都顯示：

```
Layer 1 Sandbox    Namespace
PID namespaces     Yes
Network namespaces Yes
Seccomp-BPF sandbox Yes
You are adequately sandboxed.
```

## 整合時發現的問題

| 問題 | 原因 | 處理 |
|---|---|---|
| Container 啟動即退出：`chmod /run/user/10001: operation not permitted` | 先 chown 給 10001，root 沒有 `CAP_FOWNER` 就不能再 chmod | 先 chmod 再 chown |
| Supervisor 看不到 Xauthority、dbus Gate 失敗 | Browser Run 目錄 0700 屬 10001，Supervisor 是 10000 | 目錄改 `0710 10001:10000` |
| Sandbox 驗證永遠找不到 Renderer | Chromium 用 setproctitle 改寫子行程 argv，`/proc/<pid>/cmdline` 變成空白分隔的一整串 | 解析時 NUL 與空白都當分隔 |
| `Seccomp: 2` 無法證明 Chromium 的 Filter | Docker 自己的 Filter 已讓所有行程都是 2 | 改要求 Renderer 的 `Seccomp_filters` 多於 Browser 主行程（實測 2 對 1） |
| SUID 變體缺 `CAP_SYS_CHROOT` 時 `Cannot chroot into /proc/` | chrome-sandbox 需要 chroot | 刪除前已記錄；userns 不需要 |

## 未驗證

- Ubuntu 23.10 以後預設 `apparmor_restrict_unprivileged_userns=1` 的主機。
- rootless Docker、Podman、cgroup v1。
- 長時間執行、Chromium 自動更新後 Profile 相容性（M3 範圍）。

## 2026-10-25 補註（整合版，arm64）

上面的結果仍是 2026-10-24 的 M0 結果，沒有更改。整合版（Gateway、UI、更新流程）的補充證據是私有檔案，只在本機保存：

- RT-01 在整合版 arm64 映像重跑：`rt01/result.txt` 有 24 個 PASS。
- 行程快照（`processes.txt`）沒有任何 `--no-sandbox`，Renderer 的 `Seccomp_filters` 為 2、Browser 主行程為 1，全部 `NoNewPrivs: 1`。
- 沒有重跑 Sandbox Matrix，也沒有整合版 amd64。
- 「Chromium 自動更新後 Profile 相容性（M3 範圍）」：M3 更新與回退已用開發版 Release 實測，但實測的 Release 都記錄同一個 Chromium 版本（`156.0.8078.47-1~deb13u1`），所以 Chromium 升版後的回退仍未實測。

完整狀態與限制見 [ACCEPTANCE.md](../ACCEPTANCE.md#evidence-status)。
