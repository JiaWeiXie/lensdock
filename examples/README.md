# 範例用途

`feedback.example.json` 是人工編寫的合成資料，符合已發布 Feedback Revision Schema，且仍然有效：它是 `schemaVersion` `0.1.0`、`page-viewport` 來源、只用矩形／Pin／遮蔽標註、並填了描述與預期／實際結果的 Revision，Schema 繼續接受這種舊版形狀，所以沒有改動。圖片大小、Hash、Target、時間與 Browser 版本欄位只是契約測試值，不代表曾經擷取真實網頁；本包沒有對應 PNG。`drawing` 來源、橢圓、菱形、直線標註，或沒有描述（純視覺回饋）的任務需要 `0.2.0`，這些變體由 `tools/validate_spec.py` 從本範例衍生並測試，沒有另外存成範例檔。

`task-template.md` 提供中文任務表單，可手動填寫。真實分享包內的 `task.md` 由 Gateway 在發布時產生，會把人類撰寫的欄位放進 Code Fence 並明示為不受信任的資料；不要用範本取代它。真實分享包需要通過圖像去識別化、Hash／檔案大小驗證、存取控制與發布流程；不得直接使用本範例的 Human Approval 欄位代替人類確認。
