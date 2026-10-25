# ADR-0002：Runtime 身分與 Process 樹

狀態：Accepted（M0），2026-10-24。依據 SPIKE-01 在 OrbStack arm64 與 Ubuntu 22.04 amd64 的實測。

## 背景

規格要求 Chromium 以非 root、Headful、持久化 Profile 執行，Container 唯讀、`cap_drop: ALL`、`no-new-privileges`，且不可退化成 `--no-sandbox`。同時 Runtime 需要在啟動時建立 tmpfs 目錄、寫 Xauthority，並在 Browser 重啟或租約結束時對子行程送訊號。

## 決策

Process 樹固定如下：

```
tini -g (PID 1, root)
└─ lensdock-runtime supervise (先 root，完成準備後降為 UID/GID 10000)
   ├─ dbus-daemon --session      (UID 10001)
   ├─ Xvfb :99                   (UID 10001)
   ├─ openbox                    (UID 10001)
   ├─ x11vnc :5901 -viewonly -nosel (UID 10001)
   ├─ x11vnc :5900               (UID 10001)
   └─ chromium                   (UID 10001)
```

1. **root 只做準備。** `PrepareRoot` 以 root 建立 `/run/lensdock`、`/run/user/10001`、`/tmp/.X11-unix`，清掉舊檔，寫 Xauthority（獨占建立、0600、不跟隨 Symlink），再交給 10001。只用到 `CAP_CHOWN`；目錄先 chmod 再 chown，避免需要 `CAP_FOWNER`。
2. **Supervisor 降權。** `DropPrivileges` 把 Bounding Set 縮到 `{SETUID, SETGID}`，清 Supplementary Groups，`setresgid/setresuid(10000)`，Effective = Permitted = `{SETUID, SETGID}`，設定 `NO_NEW_PRIVS` 並關閉 Dumpable，套用到所有 Thread（`AllThreadsSyscall`，因此 Runtime 必須 `CGO_ENABLED=0`）。結果以 `/proc/self/status` 驗證，不符就不啟動。
3. **所有 X／VNC／Chromium／xclip 以 10001 執行**，環境變數為固定 Allowlist（含 `LANG=zh_TW.UTF-8`、`FC_LANG=en`，原因見 SPIKE-03）。Cookie 只存在 Xauthority 檔，不進 argv、env 或 Log。
4. **訊號經 Helper。** Supervisor 沒有 `CAP_KILL`，對 10001 的行程送訊號改由 `lensdock-runtime __signal`（以 10001 執行、透過 pidfd 或 Process Group）完成，不會誤殺 PID 重用後的行程。
5. **Browser Run 目錄為 `0710 10001:10000`。** Supervisor 能走到已知路徑（Xauthority 存在檢查、dbus Socket Gate），但不能列目錄，也讀不到 0600 的 Cookie。
6. **祕密只存 Digest。** Container 內只有 Operator Token 的 SHA-256（`LENSDOCK_OPERATOR_TOKEN_SHA256`）；租約 Token 只以 Digest 存在 Supervisor 記憶體。
7. **對外只有 API。** Compose 只發布 `127.0.0.1:18080 → 8080`。CDP `127.0.0.1:9223`、VNC 5900／5901 只在 Container 的 loopback。

## 實測證據

- 整合時發現並修正兩個問題：root 無 `CAP_FOWNER` 時 chown 後不能 chmod；Supervisor 無法進入 0700 的 Browser 目錄。兩者都以本 ADR 的順序與權限解決，沒有增加 Capability。
- OrbStack 與 nuci3 的 `/readyz?detail=1` 與 `procs.txt`：Supervisor `CapEff` 只有 SETUID／SETGID、`NoNewPrivs: 1`；Chromium 與子行程都是 UID 10001、`NoNewPrivs: 1`。Browser、GPU、Utility 與 Renderer 的 `CapEff` 為 0；Namespace Zygote 在自己建立的 User Namespace 內持有 `CAP_SYS_ADMIN`（`0x200000`），這是 userns Sandbox 的正常行為，對初始 Namespace 無效。

## 代價

- Supervisor 需要一個隱藏 Helper Subcommand 處理訊號，Process 樹多一層。
- Runtime 綁定 `CGO_ENABLED=0`。
- M0 尚未把 5900 納入租約控管（只在 loopback）；Gateway／noVNC 階段要補。
