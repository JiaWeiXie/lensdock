# SPIKE-02：Display 與雙 VNC

結論：**唯讀入口（5901，`-viewonly -nosel`）收到偽造的鍵盤、滑鼠事件時，頁面完全沒有反應；控制入口（5900）同樣的輸入會生效。** amd64 的 CPU 量測觸發了事先登記的規則（建議改成單一 x11vnc＋`-viewpasswd`），但決定**維持雙實例**，理由見[決定](#決定維持雙實例偏離規則)。

對應驗收：CT-04 通過（兩台主機）；RT-03 的 Viewer 部分（`TestRT03Viewers`）通過。版本矩陣同 [SPIKE-01](SPIKE-01-sandbox.md#版本矩陣)。

## 架構

Xvfb `:99`（1920×1080×24）→ openbox → Chromium（Headful、`--ozone-platform=x11`、最大化）。兩個 x11vnc 讀同一個 Display：

| 埠 | 參數 | 用途 |
|---|---|---|
| 5900 | `-localhost -noipv6 -forever` | 控制（M1 由 Gateway＋租約控管） |
| 5901 | 同上＋`-viewonly -nosel` | 唯讀 Viewer |

兩個埠都只在 Container 的 loopback；測試以共用 Network Namespace 的 Sidecar Container 連入（僅測試使用，產品不發布這兩個埠）。

## CT-04：唯讀入口偽造輸入

`TestCT04` 開啟記錄所有輸入事件的 Fixture（`tests/fixtures/input-recorder.html`），對兩個埠送出相同的 RFB PointerEvent 與 KeyEvent。

| 埠 | OrbStack | nuci3 |
|---|---|---|
| 5901 唯讀 | `events: []`、textarea 為空 | 同左 |
| 5900 控制（正向對照） | 有 pointermove／mousemove／keydown 事件，textarea 為 `ab` | 同左 |

正向對照證明測試真的能送進輸入，唯讀的「沒反應」不是測試本身失效。

## VNC CPU 量測

方法（`tests/e2e/vnc-cpu.sh`）：每個情境 60 秒，從 `/readyz?detail=1` 各 Unit 的 `cpuTicks` 差值換算成單一 vCPU 百分比。

- S0：靜態頁，沒有 Viewer。
- S1：靜態頁，5901 一個 Viewer。
- S2：全畫面動畫，5900 與 5901 各一個 Viewer。
- S3：全畫面動畫，5901 三個 Viewer。

事先登記的規則：**amd64 上，S2 的 vnc-view 減 S0 超過單一 vCPU 的 10%，就建議改用單一 x11vnc＋`-viewpasswd`。**

| 主機 | Unit | S0 | S1 | S2 | S3 |
|---|---|---|---|---|---|
| nuci3 amd64 | vnc-view（5901） | 0.07% | 0.65% | 24.37% | 44.56% |
| nuci3 amd64 | vnc-control（5900） | 0.05% | 0.07% | 23.91% | 0.07% |
| OrbStack arm64 | vnc-view（5901） | 0.07% | 0.50% | 4.06% | 9.29% |
| OrbStack arm64 | vnc-control（5900） | 0.07% | 0.08% | 4.03% | 0.02% |

nuci3 S2 同時段：Chromium 120.2%、Xvfb 35.5%、整個 Container 204.7%。量測 Run：nuci3 `nuci3-20261024T153838Z`、OrbStack `orb-20261024T153125Z`（VNC 相關程式碼與最終版相同）。

判定：**amd64 邊際 24.30% > 10%，規則觸發**；arm64 3.99%，依規則不作判定。

## 解讀與建議

S3 比 S2 多兩個 5901 Viewer，vnc-view 多了約 20%。假設每個 Viewer 的成本是線性的，每多一個 Viewer 約 10%，扣掉後一個 x11vnc 實例的固定成本（輪詢、比對 Framebuffer）推估約 14%。所以改成單一 x11vnc，**假設每個 Viewer 成本線性時推估約 14%，尚未以單實例對照驗證**，而不是整整 24%；每個 Viewer 的編碼成本仍在。這個推估有兩個限制：`vnc-cpu.sh` 只量了雙實例情境，沒有單實例對照組；S2→S3 除了 Viewer 數，也同時拿掉了 5900 的連線，兩個變因混在一起。

改成單一實例的代價：

- 唯讀改靠同一個 Server 內的密碼分級（`-viewpasswd`），不再是兩個獨立行程。CT-04 要重做，並確認唯讀 Client 送的 ClientCutText 也被忽略。
- VNC 密碼需要管理與輪替，而且 RFB 密碼只有 8 bytes。

## 決定：維持雙實例（偏離規則）

2026-10-25 決定不照規則改成單一實例，理由：

- 規則的門檻是拿整個 24% 邊際成本來比，但改成單一實例能省下的量，假設每個 Viewer 成本線性時推估約 14%，尚未以單實例對照驗證。依同一推估，這部分重複輪詢只在兩個埠**同時**有 Client 而且畫面持續變動時才明顯；已量到的 S1（靜態頁＋一個 Viewer）只有 0.65%，沒人連線時是 0.07%。
- 雙實例讓唯讀成為獨立行程的啟動參數，不依賴同一個 Server 裡的密碼分級，CT-04 的證據可以直接沿用。
- 單一實例要管理和輪替 VNC 密碼，而且 RFB 密碼只有 8 bytes；Gateway 本來就以租約控管 5900，再多一層密碼沒有帶來額外的安全邊界。

重新評估的條件：Linux 基準主機（§18）上一般使用時的 Container CPU 超出預算，或 M1 Gateway 實作後同時連線數明顯變多。到時候在 `vnc-cpu.sh` 加入單一 x11vnc＋`-viewpasswd` 的對照情境（相同 Viewer 組合），實際量出能省多少，而不是沿用規則裡的邊際值或上面的推估。

## 未驗證

- 單一 x11vnc＋`-viewpasswd` 的 CPU 對照；上面約 14% 的節省量只是線性推估。
- noVNC（WebSocket）路徑；M0 用原生 RFB Client 量測。
- libvncserver 在 RFB 埠接受 WebSocket Upgrade 的風險（頁面 JavaScript 能否打到 5900）；M0 兩個埠只在 loopback，Gateway 階段要驗證。
- 4K、多螢幕、HiDPI。

## 2026-10-25 補註（整合版）

上面的量測與決定仍是 2026-10-24 的 M0 結果，沒有重測。

- 整合版 Runtime 的行程快照仍是兩個 x11vnc：5900 控制、5901 `-viewonly -nosel`，與「維持雙實例」的決定一致。
- noVNC（WebSocket）路徑已由整合版 UI 經 Gateway 使用：1280×800、14 px 時連線畫布佔視窗 75.3245%（見 [ACCEPTANCE.md](../ACCEPTANCE.md#evidence-status)）。上面的 CPU 數字仍是 M0 原生 RFB Client 的量測。
- 整合版 Gateway 對沒有 HUMAN 租約的 viewer 呼叫控制端點回 403 `LEASE_REQUIRED`。偽造 RFB 輸入經 Gateway 的 noVNC 路徑沒有記錄。
- 單實例對照、4K／多螢幕／HiDPI，以及 libvncserver 在 RFB 埠接受 WebSocket Upgrade 的風險，整合版沒有紀錄，仍視為未驗證。
