# ADR-0001：核心架構決策

狀態：Proposed，2026-10-24。隨 M0 驗證結果調整，但不能無紀錄地弱化安全邊界。

## 決策 A：自行建置 Debian Runtime，而非繼承 jlesage 映像

需求指定 Debian slim，jlesage 現有基底為 Alpine。只參考功能切分，重新組裝 Debian Chromium、Display、VNC、持久化及 Process 監督。代價是自己負責 Package、安全更新與相容性；好處是基底可控、功能邊界明確。來源：R01、R02、R06–R08。

## 決策 B：noVNC 只作 Library，不維護大型 Fork

以公開 RFB API 封裝 Client Adapter，協作功能放在 React App。避免耦合上游內部 Canvas 與 UI 結構，讓 noVNC 可獨立升版。新增螢幕擷取使用公開 API；後端 Capability 不足時禁用對應按鈕，不靠私有欄位硬做。來源：R04–R05。

## 決策 C：一個 Workspace 只有一個控制者

Named Session、不同分頁或不同 MCP Client 不是帳號隔離。人與 Agent 經租約、Epoch、Generation 交替操作；背景 Viewer 使用後端唯讀 VNC。需要並行身分時，使用不同 Runtime／Volume。代價是單 Workspace 並行度有限；優點是人工接管與歸責可靠。

## 決策 D：截圖是不可變證據，不是即時點擊指令

桌面與網頁擷取分開標示，所有標註以來源圖片正規化座標保存。Agent 動作前重新 Observe，不能直接相信舊 Pixel／Element Ref。Published Feedback Revision 不可變；工作狀態另存在 Delivery。代價是多一層資料模型，換來一致的協作、稽核及跨 Agent 交接。

## 決策 E：更新使用新映像；回退使用相容資料快照

不使用運行中 apt upgrade 或不看工作狀態的自動重啟。更新前乾淨停 Browser、保留 Profile／Keyring／DB、候選驗證再切換。回退不保證新版 Profile 可被舊 Browser 讀取。代價是需要維護窗口、更多磁碟與快照管理；優點是故障範圍可界定。

## 決策 F：Host Admin 與 Browser 控制權分離

Gateway 可顯示更新資訊，但不取得 Docker Socket。Host CLI 掌握部署、快照、切換；Agent 僅能控制所授權的 Browser。多一個管理入口是刻意設計，不用「Web UI 一鍵萬用」換取整台主機的暴露。
