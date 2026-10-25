# 更新與回退 Runbook

狀態：已實作。實際命令、參數與復原步驟以 [UPDATE-IMPLEMENTATION.md](UPDATE-IMPLEMENTATION.md) 為準。目前只以開發版 Release（未簽署，無 BuildKit Provenance 與掃描報告）在 macOS arm64／OrbStack 本機實測；已實測與未實測的項目列在本文最後一節。正式簽署 Release 通道與整合版 amd64 尚未驗證。

## 1. 責任切割

Browser／Debian userspace 更新透過新 Runtime Image；Gateway／noVNC／UI 更新透過新 App Image；主機 Kernel／Docker Engine 由主機管理者處理。Browser Runtime 與 Gateway 不掛 Docker Socket。CI 檢查與建置更新候選不等於可以任意重啟正在工作的 Browser。

## 2. 更新命令契約

```text
lensdock update check [--workspace <id>]
lensdock update plan --release <id|dir> [--workspace <id>] [--output <file>] [--allow-dev] [--allow-downgrade]
lensdock update apply --plan <plan-file> [--yes] [--approve-switch] [--revoke-leases] [--accept-crash-snapshot] [--dry-run]
lensdock update approve --update <id> --yes
lensdock update abort --update <id> [--keep-candidate]
lensdock update rollback --checkpoint <checkpoint-id> [--yes] [--revoke-leases] [--allow-crash-checkpoint] [--dry-run]
lensdock update status | checkpoints list|verify|prune | branches list|delete
lensdock update compose -- <up|down|stop|start|restart|ps|logs|config> ...
```

每個命令都接受 `--deployment <file>`。

`check` 只顯示資訊；`plan` 檢查 Image Digest、簽章、相容性、磁碟、Backup 位置及維護影響，不改動 Browser。`apply` 必須在 Host Admin 執行並確認，不能由一般 Agent Token 呼叫。命令與 Log 不顯示 Token、Profile 內容或 Secret。Gateway 與 UI 沒有 Docker Socket，也沒有套用或回退的端點；它們只讀取唯讀的 `deployment-status.json`。

## 3. 安全更新順序

| 步驟 | 必做動作 | 失敗時 |
|---|---|---|
| 01 | 驗證 Release Manifest、簽章／來源、Digest、Schema 相容性；在停止任何服務之前，以同一 Repository 的 `lensdock-keep-<完整 Digest>` Tag 保留目前執行中的映像 | 停止，不覆蓋現有映像或資料；映像無法保留時尚未停止任何服務 |
| 02 | 確認既有 Profile、DB、Keyring、附件配額與備份空間 | 空間不足拒絕進入更新 |
| 03 | 先停 Gateway（排空 SSE、VNC、CDP 連線後關閉 DB），Workspace 進 MAINTENANCE，拒絕新租約，告知使用者 | 無法鎖定則停止 |
| 04 | 既有控制權排空或明確取消，關閉 CDP／VNC 寫入路徑 | 不繼續發新控制權 |
| 05 | 正常關閉 Chromium（先 `Browser.close`，再訊號），確認 Runtime 與 Gateway Exit Code 為 0、無 `Singleton*` Lock、`profile.exit_type` 為 Normal 或 SessionEnded、SQLite Journal 已清空或為已提交的 PERSIST 標頭 | Crash Snapshot 需另外標示，不能當乾淨備份 |
| 06 | 產生 Profile／Keyring／Gateway DB 的一致性 Checkpoint | 備份失敗則恢復舊服務，或維持鎖定等待處理 |
| 07 | 從 Checkpoint 建立候選 Volume；不與舊 Browser 共寫 | Copy 失敗刪除候選，不刪原資料 |
| 08 | 先在複製的 DB 上以無網路容器執行 Gateway Migration，再讓新映像執行 Sandbox、Font、CDP、UTF-8／PNG Fixture Smoke Test | 不切換，保留測試結果 |
| 09 | 預設禁止候選對外；只測本機 Fixture | 真實帳號測試需人工授權，避免雙發請求 |
| 10 | 經確認才切到新映像與候選 Volume，產生新 Generation | 保留舊 Checkpoint 供成對回退 |
| 11 | 重新取得健康狀態，使用者／Agent 重新申請租約；Gateway 重啟後瀏覽器 Session 失效，需重新配對 | 不自動續跑上次破壞性動作 |

回饋 Artifact 可獨立持久化，但對應 DB Migration 的版本要能指回相同附件。跨 DB 版本回復時不得留下「列得到但讀不到」的分享包。實作上 Artifact 屬於 Retained Volume，不隨回退還原；回退還原的 DB 沒有更新後新增的 Feedback，讀取回 404，檔案仍保留。

The usage mode and pause live in the retained `lensdock-runtime-policy` volume,
separate from Chromium's Profile. Candidates use an empty scratch policy in
AGENT mode; production keeps its saved policy through the switch. Existing
installations must add the mount, dataset declaration and empty volume before
planning this update. See [Adding the runtime policy volume](UPDATE-IMPLEMENTATION.md#adding-the-runtime-policy-volume).

## 4. 回退規則

回退前保留目前故障／新版本 Volume，便於取回更新後新增資料。預設恢復舊映像、更新前 Profile、必要 Keyring 與相容 DB Checkpoint；不要讓舊 Chromium 直接打開已被新版本遷移的 Profile。

必須向使用者顯示 Checkpoint 時間、可能遺失的更新後狀態，以及哪些 Artifact 不受回退影響。沒有相容 Checkpoint 時不保證可回退，UI 必須明示，不提供看似安全的一鍵按鈕。

DB Schema 跨版本時，回退還原 Checkpoint 內的舊 Schema DB，並以舊 Gateway 啟動；舊 Gateway 絕不讀取較新 Schema 的 DB。執行中 DB Schema 高於新 Release 的 Gateway Schema 時，Plan 會拒絕，除非明確加 `--allow-downgrade`（仍使用成對 Checkpoint）。回退前會再次以同 Repository 的 Keep Tag 保留 Checkpoint 內的舊映像與目前執行中的映像，任一不可用時不停止任何服務。

The retained usage policy is not rewound with a compatible rollback. Releases
that predate usage modes cannot enforce it: do not assume that a saved NOVNC
mode or pause protects a workspace running such an old Runtime.

## 5. 版本與安全更新政策

固定 Release 使用 Digest，不依賴 `latest`。CI 定期取得新的 Debian Security／上游相依候選並產生測試提案，保存套件清單與可重現來源。重大 Chromium／Debian 升版需完整 Profile 相容與回退測試；緊急修補仍至少做 Sandbox、持久化與交接 Smoke Test，不跳過備份。

目前的釘選點與更新方式：

| 項目 | 位置 | 更新時一併做 |
|---|---|---|
| Debian 基底、Go 建置映像 Digest | `docker/browser/Dockerfile` 的 `ARG` | Go 版本須同時改 `mise.toml`、`go.mod`，`mise run check:pins` 必須通過 |
| 開發工具版本 | `mise.toml`、`mise.lock` | `mise install`、`mise run check` |
| Chromium Seccomp Profile | `tools/gen_seccomp.py` 釘選的 moby commit 與 sha256 | `mise run seccomp:generate` 重產，重跑 `mise run e2e:sandbox-matrix`（自訂 Profile 必須 Ready，Docker 預設必須 `SANDBOX_PREFLIGHT_FAILED`） |
| 套件實際版本 | 建置時寫入 `/usr/share/lensdock/dpkg-manifest.tsv` | `mise run e2e:version-matrix` 保存版本矩陣 |
| Gateway 映像基底與前端相依 | `docker/gateway/Dockerfile` 的 `ARG`（Node、Go、Debian Digest）、`apps/web/pnpm-lock.yaml` | `mise run web:build`、`mise run gateway:image`；字型與 noVNC 授權檔在 `apps/web/public/licenses/` |

預設保留最近三個已驗證 Checkpoint（部署檔的 `retainCheckpoints`）；部署檔目前只有數量與最小可用空間（`minFreeBytes`）兩項設定，沒有依時間到期的清理。`update checkpoints prune` 只在沒有有效更新／回退作業引用時才清理。敏感備份必須加密，金鑰不與備份混放。保留策略的到期刪除需涵蓋私有截圖與其備份；一般檔案刪除不宣稱能在所有儲存媒體上完成不可回復抹除。

## 6. 需要實測的失敗案例

更新下載中斷、磁碟滿、Chromium 不正常退出、備份不完整、新版 Sandbox 失敗、字型缺失、DB Migration 失敗、Profile 不相容、舊 Gateway 讀新 Schema、Host 重啟與候選網路外洩。每一種都必須有明確狀態、不可重複副作用與恢復步驟。

### Exercised status (2026-10-25, local macOS arm64 with OrbStack)

Only the cases marked exercised were run against the real deployment, with development releases. The others have an implemented check or guard, but no injected run is recorded; do not read them as verified. Evidence file names are in [UPDATE-IMPLEMENTATION.md](UPDATE-IMPLEMENTATION.md#exercised-evidence-and-limits).

| Case | Status |
|---|---|
| Candidate Runtime never becomes ready (entrypoint `/bin/false`) with a live VNC session and SSE stream open | Exercised. The previous healthy release and its schema 2 database were restored, published feedback was unchanged, the browser reconnected |
| Gateway schema upgrade 1 to 2 and paired rollback to schema 1 | Exercised. The old Gateway ran only on the restored schema 1 database |
| Profile markers and retained artifacts across update and paired rollback | Exercised. Pre-update markers restored, post-update markers absent, retained artifact hashes unchanged |
| Chromium unclean exit, incomplete backup, Sandbox or font failure in the candidate, Profile incompatibility | Guards exist (exit code, lock, `profile.exit_type` and journal checks; decrypt and tar walk before publishing a checkpoint; candidate probe checks). No injected failure was run |
| Disk full, interrupted download | The plan checks free space for the checkpoint and the candidates. Images are local, so there is no pull step. No injected failure was run |
| Failed Gateway migration on the clone | Not injected. The successful migration was exercised |
| Host restart during an update | Journals persist and `abort` rebuilds the run from them. Not exercised |
| Candidate network leak | The probe's egress check and the internal-network check passed for the real candidates. A deliberate leak was not injected |
| Old Gateway reading a newer schema | Prevented by plan and by rollback ordering. Starting an old Gateway against schema 2 on purpose was not run |
