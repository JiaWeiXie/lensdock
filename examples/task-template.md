# 問題／任務交接

**回饋 ID：** `<feedback-id>`　**版本：** `<revision>`

**Workspace：** `<workspace-id>`　**圖片來源：** `<desktop | page-viewport | drawing>`（`drawing` 是空白畫布上的說明圖，沒有瀏覽器頁面）

## 要做什麼

<只分析／協助重現／修改並驗證，以及本次允許的操作範圍。>

## 問題描述

<指出哪一個編號或框選區域，以及目前看到的狀況。>

## 預期結果

<描述正確行為或畫面，不只說「修好」。>

## 實際結果

<描述錯誤行為、畫面及是否可以穩定重現。>

## 重現步驟

1. <起始頁面與必要前置狀態。>
2. <造成問題的操作。>
3. <觀察到的錯誤。>

## 分享附件

<填入經核准且可實際讀取的圖片／Bundle，不使用只有伺服器自己能讀的絕對路徑。>

## Agent 執行要求

請先讀取圖片及回饋，說明你理解的問題，並以自己的身分明確 ACK（讀取與排隊都不是 ACK）。需要操作 Browser 時，用 `lensdock lease acquire <workspace-id> --mode agent --feedback-id <feedback-id> --operator-token-file <agent-token-file> --output <lease-file>` 取得租約，確認目前 Target 並重新觀察；不要直接使用舊截圖座標點擊，`drawing` 圖片更沒有可點擊的頁面座標。不要讀取未授權原始截圖、Cookies 或 Profile。完成後回報修改摘要、驗證步驟及新的驗證截圖，等待人工確認。
