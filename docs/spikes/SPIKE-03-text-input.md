# SPIKE-03：CJK、Emoji 與 UTF-8 輸入

結論：

- 繁中／簡中／日文／韓文依 `lang` 選到對應的 Noto Sans CJK 字面，Browser 截圖與 VNC 畫面都沒有缺字方框。
- 7 組 Emoji（膚色、旗幟、ZWJ、VS16）都塑形成單一 Noto Color Emoji 字符。
- 文字輸入路徑定為 `PUT /clipboard`（HUMAN 租約）＋控制 VNC 送 Ctrl+V，結果逐位元一致。RFB ClientCutText 路徑不可用。

對應驗收：TX-01、TX-02 通過；TX-03 的 UTF-8 資料路徑通過，M0 時實際 IME 組字未測（見「未驗證」一節）；2026-10-25 整合版已補測注音 IME，見[補註](#2026-10-25-補註整合版)。版本矩陣同 [SPIKE-01](SPIKE-01-sandbox.md#版本矩陣)。

## TX-01：語言對應字型

`TestTX01` 用 `CSS.getPlatformFontsForNode` 讀 `tests/fixtures/cjk-emoji.html` 每一列實際使用的字型。

| `lang` | 期望 | OrbStack | nuci3 |
|---|---|---|---|
| zh-Hant-TW | Noto Sans CJK TC | TC | TC |
| zh-Hans-CN | Noto Sans CJK SC | SC | SC |
| ja | Noto Sans CJK JP | JP | JP |
| ko | Noto Sans CJK KR | KR | KR |

Browser 截圖與 5901 Framebuffer 截圖都存成證據，人工檢查沒有缺字方框。

### 修正前的兩個問題

1. **全部顯示成繁中字形。** 系統 `LANG=zh_TW.UTF-8` 讓 Fontconfig 把泛用 `sans-serif` 直接解析成 Noto Sans CJK TC。TC 涵蓋所有 CJK 字元，Blink 就不會再依元素的 `lang` 做 Fallback，日文、簡中、韓文都用了 TC 字形。修正：Chromium 等子行程加上 `FC_LANG=en`。泛用字族改解析成拉丁字型 Noto Sans，CJK 字元改走依 `lang` 的 Fallback；沒標 `lang` 的文字仍依 zh-TW UI 語系得到 TC。
2. **`zh-Hant-TW` 變成日文字形。** Fontconfig 2.15 不認得 BCP 47 的 Script Subtag，`zh-Hant*`、`zh-Hans*` 會落到預設的 JP。修正：`docker/browser/fontconfig.reference.conf`（安裝為 `/etc/fonts/local.conf`）先把 `zh-hant` 對應到 `zh-tw`、`zh-hans` 對應到 `zh-cn`，再交給既有的 Noto CJK 規則。

## TX-02：Emoji

`TestTX02` 把 Fixture 的 Emoji 列拆成一個序列一個 `span`，逐一查字型。

| 序列 | 結果（兩台相同） |
|---|---|
| 😀 U+1F600 | 1 個 Noto Color Emoji 字符 |
| 👍🏽 膚色 | 1 |
| 👩‍💻 ZWJ | 1 |
| 👨‍👩‍👧‍👦 ZWJ×3 | 1 |
| 🇹🇼 Regional Indicator | 1 |
| ❤️ VS16 | 1 |
| 🏳️‍🌈 VS16＋ZWJ | 1 |

## TX-03：剪貼簿資料路徑

`TestTX03`：以 HUMAN 租約 `PUT /clipboard` 寫入（Runtime 讓 xclip 以 10001 持有 CLIPBOARD Selection），再經 5900 送 Ctrl+V，讀回 textarea 比對位元組。VNC 先連好，204 一回來就立刻送 Ctrl+V，中間不等待。`PUT /clipboard` 回 204 前，Runtime 會另外跑 `xclip -out` 讀回 CLIPBOARD，逐位元組和寫入內容一致才算成功（最多等 2 秒，每 25 ms 讀一次）；擁有者先結束或一直讀不到相同內容就回 502 `UPSTREAM_UNAVAILABLE`。換新內容前也會先等舊擁有者真正結束，避免讀回時讀到舊行程送出的相同內容。204 之後若有其他程式（例如在 Chromium 內複製）搶走 Selection，貼上的就是新內容，這不在保證範圍。

| 案例 | 內容 | OrbStack | nuci3 |
|---|---|---|---|
| fixture-expected-line | 繁中、日文、韓文、😀、👩‍💻 | 一致 | 一致 |
| astral-planes | U+1F600、U+1D49C、U+20000、🎉 | 一致 | 一致 |
| nfd-and-nfc | NFD 與 NFC 並存、韓文字母組合 | 一致 | 一致 |
| zwj-flags-skin-tone | ZWJ、旗幟、膚色、VS16 | 一致 | 一致 |
| newline | 多行＋CJK | 一致 | 一致 |

HUMAN 租約結束後，`xclip -o -selection clipboard` 讀不到內容（`tx03-xclip-after.*`），剪貼簿不會留給下一個持有者。

## RFB ClientCutText（不採用）

`TestSpike03CutText` 只記錄、不判定。透過 5900 送 ClientCutText 再按 Ctrl+V，結果如下：

| 內容 | 頁面收到 |
|---|---|
| ASCII | 空字串 |
| CJK（以 UTF-8 位元組送出） | 空字串 |
| Latin-1 `café` | 空字串 |

另外在 Container 內直接探測：x11vnc 0.9.17 收到 ClientCutText 後確實寫入 `CUT_BUFFER0`（debug log 為 `xcut_receive: recv-ing it`），但連線期間以 `xclip` 讀 PRIMARY 與 CLIPBOARD 都拿不到內容（`target STRING not available`；沒有 Selection Owner 時 xclip 也是這個訊息）。Chromium 只從 Selection 貼上，所以連 ASCII 都進不去；再加上 RFB 協定的 CutText 定義為 Latin-1，這條路不能承載 UTF-8。

決策：文字只走 `PUT /clipboard`，由租約控管。noVNC 的剪貼簿面板改呼叫這支 API，不使用 RFB CutText。

## 未驗證

- **實際 IME 組字**：本機注音／日文／韓文 IME 組字、組字鍵不觸發快捷鍵。需要 noVNC Client UI，屬 M1；M0 只證明 UTF-8 資料路徑逐位元一致。
- 標註編輯器的字型資產與匯出（TX-04、TX-05，M1／M2）。
- 直書、Ruby、雙向文字。

## 2026-10-25 補註（整合版）

上面的結果與「未驗證」清單是 2026-10-24 的 M0 狀態。整合版補測如下，證據是私有檔案 `native-ime-probe.json`（結果 `PASS_ZHUYIN_UTF8_ROUND_TRIP`）：

- 在 macOS 明確選定注音輸入法後，桌面目標的鍵盤輸入 `s`、`u`、`3`、`Return` 在本機 `#clip-text` 組出 `你`。`compositionstart` 與 `compositionupdate` 是可信事件；Relay 觀察到的 `compositionend` 是合成事件。
- 接著由 HUMAN 接管，把文字 Send and paste 到 VNC 取得焦點的遠端 textarea；新的 AGENT 租約以 CDP 獨立讀回，值為 `你`，UTF-8 位元組 228 189 160。
- 較早的探測用視窗目標按鍵並依賴隱含的輸入來源，只得到 ASCII `su3`；那是測試的輸入來源選擇與路由問題，不是已確立的產品缺陷，已被取代。
- 仍未驗證：日文、韓文原生 IME，其他輸入法與平台；注音只測一個字、一台 macOS 主機，也沒有單獨檢查組字鍵不觸發快捷鍵。
- 標註編輯器的匯出（TX-04）：整合版實測下載的 PNG 與剪貼簿 `image/png` SHA-256 相同、尺寸 1920×1080；沒有字型缺失的 Client 對照。TX-05 沒有紀錄。

狀態表見 [ACCEPTANCE.md](../ACCEPTANCE.md#evidence-status)。
