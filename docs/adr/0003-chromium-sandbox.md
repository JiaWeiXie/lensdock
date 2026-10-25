# ADR-0003：Chromium Sandbox 採 User Namespace＋自訂 Seccomp

狀態：Accepted（M0），2026-10-24。依據 SPIKE-01 Sandbox Matrix（見 `docs/spikes/SPIKE-01-sandbox.md`）。

## 背景

Chromium 在 Container 內有兩條不需要 `--no-sandbox` 的路：

- **userns**：非特權 User Namespace 建立 Renderer Sandbox，以 `--disable-setuid-sandbox` 啟動。Docker 預設 Seccomp 會擋掉所需的 `clone`／`unshare` Namespace 旗標，因此需要自訂 Profile。
- **SUID**：安裝 `chromium-sandbox`（root setuid 的 `chrome-sandbox`），以 `--disable-namespace-sandbox` 啟動。

M0 兩者都建置、都在 OrbStack arm64 與 Ubuntu 22.04 amd64 實測，結果兩者都能 Ready、`chrome://sandbox` 都顯示 Adequately Sandboxed，都沒有出現 `--no-sandbox`。

## 決策

只保留 **userns＋`docker/browser/seccomp/chromium.json`**，刪除 SUID 變體（Build Arg、`compose.suid.yaml`、Supervisor 的模式切換、`chromium-sandbox` 套件、相關測試與 Matrix Cell）。

Seccomp Profile 由 `tools/gen_seccomp.py` 從釘選的 moby `docker-v29.9.2` 預設 Profile（commit 與 sha256 都驗證）產生，只做以下差異：

| Syscall | 變更 |
|---|---|
| `clone`（含 s390 參數位置） | 允許，但不得帶 `CLONE_NEWNS／NEWUTS／NEWIPC／NEWCGROUP`；`NEWUSER／NEWPID／NEWNET` 可通過 |
| `unshare` | 同上旗標遮罩 |
| `clone3` | 維持 moby 的 `ERRNO(ENOSYS)`；glibc 會退回 `clone`，讓上面的旗標遮罩真正生效 |
| `setns`、`chroot` | 允許（`chroot` 實測必要） |

Supervisor 在啟動 Chromium 前先跑 userns Preflight；失敗時 Runtime 鎖為 `SANDBOX_PREFLIGHT_FAILED`、不啟動 Chromium，**沒有任何降級路徑**。啟動後再以 `/proc` 證明 Sandbox：每個 Renderer 必須在巢狀 PID Namespace（NSpid ≥ 2 層），且 `Seccomp_filters` 必須多於 Browser 主行程。只看 `Seccomp: 2` 不夠，因為 Docker 自己的 Filter 已讓 Container 內所有行程都是 2。

## 理由

| 面向 | userns＋自訂 Seccomp | SUID |
|---|---|---|
| Container Bounding Set | `CHOWN SETUID SETGID SETPCAP KILL`；Supervisor 降權後只剩 `SETUID SETGID`，只有 root 的 tini 保留 `KILL` 以轉發停止訊號 | 必須額外保留 `SYS_ADMIN`、`SYS_CHROOT` |
| `no-new-privileges` | 開啟 | 必須關閉 |
| 映像內 setuid root 檔案 | 無 | `chrome-sandbox` |
| Seccomp | 自訂（moby 預設＋上表差異） | 只能用 Docker 預設 |
| 維護成本 | Profile 需隨 moby 升版重產 | 少一份 Profile |

SUID 變體要把整個 Container 的權限上限拉高到 `CAP_SYS_ADMIN`，並關掉 `no-new-privileges`，換來的只是不用維護一份可重現產生的 Seccomp Profile。這不划算。

## 主機需求

- 必須允許非特權 User Namespace：`kernel.unprivileged_userns_clone=1`（若存在）、`user.max_user_namespaces > 0`。
- nuci3（Ubuntu 22.04、kernel 6.8）`kernel.apparmor_restrict_unprivileged_userns=0`。Ubuntu 23.10 之後預設為 1；這時 Container 內的 userns 是否被 AppArmor 擋下，**M0 沒有測試**。若被擋，Runtime 會以 `SANDBOX_PREFLIGHT_FAILED` 拒絕 Ready，處理方式是調整主機或 AppArmor Profile，不是改用 `--no-sandbox`。

## 代價與後續

- moby 預設 Profile 升版時，要用 `mise run seccomp:generate` 重產並重跑 Sandbox Matrix。
- Matrix 保留兩格作為回歸：自訂 Profile 必須 Ready；Docker 預設 Profile 必須 `SANDBOX_PREFLIGHT_FAILED` 且沒有 Chromium 行程。

## 2026-10-25 補註（整合版）

- 上表的 Bounding Set 現在多了 `KILL`（M0 與產品 Compose 都有）：乾淨停止需要 root 的 tini 把停止訊號轉給 Supervisor。其餘結論不變：沒有 `SYS_ADMIN`、`no-new-privileges` 開啟、沒有 setuid 檔案。
- 目前固定的 Chromium argv（`internal/supervisor/argv.go`）不含 `--disable-setuid-sandbox`；映像沒有 `chrome-sandbox`，單元測試禁止 `--no-sandbox` 與 `--disable-namespace-sandbox` 等旗標。整合版行程快照（`processes.txt`，私有證據）中沒有任何 `--no-sandbox`，Renderer 的 `Seccomp_filters` 為 2、Browser 主行程為 1，全部 `NoNewPrivs: 1`。
- 這只是 arm64／OrbStack 的快照；`chrome://sandbox` 未重新擷取，Ubuntu 23.10 以後的 AppArmor 限制仍未測。整合版狀態見 [ACCEPTANCE.md](../ACCEPTANCE.md#evidence-status)。
