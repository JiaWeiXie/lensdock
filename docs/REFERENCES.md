# 官方參考資料與上游關係

查核日期：2026-10-24。以下是設計依據，不是替 LensDock 執行整合測試的證明；整合測試的執行證據與限制見 [ACCEPTANCE.md](ACCEPTANCE.md#evidence-status)。上游 main／master 與網站內容會改變；正式實作需額外鎖定 Commit／Release／Digest。

## 兩個主要參考專案

**jlesage/docker-chromium** — https://github.com/jlesage/docker-chromium

參考 GUI Container、持久化資料與 CDP Bridge 的拆分。其 Dockerfile 目前使用 Alpine 基底，LensDock 不直接繼承映像；以 Debian slim 自行實作 Runtime。本包沒有複製該專案原始碼。上游 LICENSE 宣告 MIT： https://github.com/jlesage/docker-chromium/blob/master/LICENSE 。

**vercel-labs/agent-browser** — https://github.com/vercel-labs/agent-browser

作為既有瀏覽器的 Agent 控制 CLI；固定版本並驗證 CDP attach／detach、不誤開本機 Browser、Target 選擇與 Bridge 相容性。上游 LICENSE 宣告 Apache-2.0： https://github.com/vercel-labs/agent-browser/blob/main/LICENSE 。不假設上游的 Screenshot Element Labels 等同 LensDock 的人工標註工作流。

noVNC 核心的預設授權為 MPL-2.0，部分資產另有授權；詳見 https://github.com/novnc/noVNC/blob/master/LICENSE.txt 。建議使用未修改核心 Library、自訂外層 UI；任何複用與修改仍須依實際檔案授權處理。本文僅記錄上游宣告，不是法律審查。目前實作以 npm 套件 `@novnc/novnc` 1.7.0 原樣匯入，只用公開 RFB API，授權檔隨 UI 提供（`apps/web/public/licenses/`）。

## 編號索引

| ID | 來源 | 支援的查核事項 |
|---|---|---|
| R01 | https://github.com/jlesage/docker-chromium | GUI、Volume、CDP 設計參考 |
| R02 | https://github.com/jlesage/docker-chromium/blob/master/Dockerfile | Alpine 基底，不能直接符合 Debian slim 要求 |
| R03 | https://github.com/vercel-labs/agent-browser | CLI、CDP、Snapshot 等上游介面 |
| R04 | https://novnc.com/noVNC/docs/LIBRARY.html | 將 RFB Library 整合到自訂 Client |
| R05 | https://novnc.com/noVNC/docs/API.html | viewOnly、scaleViewport、Clipboard、toBlob 等公開 API |
| R06 | https://www.debian.org/releases/ | Debian stable 為 13 / trixie |
| R07 | https://hub.docker.com/_/debian | Debian 官方 slim 映像標籤 |
| R08 | https://packages.debian.org/trixie/chromium | Debian Chromium Security 套件與架構資訊 |
| R09 | https://manpages.debian.org/trixie/x11vnc/x11vnc.1.en.html | VNC 參數、view-only、shared、forever 等服務行為 |
| R10 | https://packages.debian.org/trixie/fonts-noto-cjk | CJK 字型家族 |
| R11 | https://packages.debian.org/trixie/fonts-noto-color-emoji | 彩色 Emoji 字型 |
| R12 | https://packages.debian.org/trixie/fonts-noto-core | Unicode 核心字型套件 |
| R13 | https://modelcontextprotocol.io/specification/2026-07-28/server/tools | MCP Tool 的文字、影像及結構化結果 |
| R14 | https://developers.openai.com/codex/mcp/ | Codex 的 MCP Client 整合 |
| R15 | https://code.claude.com/docs/en/mcp | Claude Code MCP 整合 |
| R16 | https://chromedevtools.github.io/devtools-protocol/tot/Page/ | Page Screenshot、Layout、Navigation 等 CDP 能力 |
| R17 | https://chromedevtools.github.io/devtools-protocol/tot/Runtime/ | Runtime.evaluate 等非唯讀風險 |
| R18 | https://github.com/can1357/oh-my-pi/blob/main/docs/tools/browser.md | OMP 的 CDP、Target、Screenshot 與生命週期 |
| R19 | https://chromium.googlesource.com/chromium/src/+/main/docs/security/apparmor-userns-restrictions.md | User Namespace 與主機 Sandbox 限制 |
| R20 | https://www.docker.com/resources/what-container/ | Container 使用主機 Kernel |
| R21 | https://docs.docker.com/build/building/best-practices/ | 映像重建、版本與 Digest Pinning |
| R22 | https://github.com/jlesage/docker-chromium/blob/master/LICENSE | MIT 授權宣告 |
| R23 | https://github.com/vercel-labs/agent-browser/blob/main/LICENSE | Apache-2.0 授權宣告 |
| R24 | https://github.com/novnc/noVNC/blob/master/LICENSE.txt | MPL-2.0 及個別檔案／資產授權 |
| R25 | https://developer.chrome.com/blog/remote-debugging-port | Chrome 預設資料目錄的遠端偵錯安全限制 |

## 後續實作需維護的來源紀錄

每個真正加入產品的相依，記錄 project、upstream URL、version／commit、checksum、license、修改檔案、用途、發行時所需 Notice。只參考設計與複製原始碼要分開標示。不得把字型檔、上游完整原始碼或預先建置二進位檔無紀錄地塞進 Repository。

## Shipped dependencies and licenses (2026-10-25)

The annotation editor is built in this repository as a React and SVG editing layer with a Canvas raster export. It uses no tldraw or Excalidraw SDK, code or dependency: neither name appears in `apps/web/package.json`, `apps/web/pnpm-lock.yaml` or `go.mod`, so no whiteboard SDK license applies to it.

LensDock's own license has not been chosen (see the root README). This file does not decide it, and a license decision, the SBOM and the third-party notices are still required before an executable product is distributed.

Web UI runtime dependencies (`apps/web/package.json`). The license column is what each installed package declares in its own `package.json`; this is an upstream declaration, not a legal review.

| Project | Version | Declared license | Use |
|---|---|---|---|
| `@novnc/novnc` | 1.7.0 | MPL-2.0 | RFB client library, imported unmodified, public API only |
| `react`, `react-dom` | 19.3.1 | MIT | UI |
| `@fontsource/ibm-plex-sans`, `@fontsource/ibm-plex-mono` | 5.3.0 | OFL-1.1 | UI fonts, served from the bundle |
| `@fontsource/noto-sans-tc`, `-sc`, `-jp`, `-kr` | 5.3.1 | OFL-1.1 | CJK UI and editor export fonts |
| `@fontsource/noto-color-emoji` | 5.3.2 | OFL-1.1 | Emoji in the UI and the editor export |

The font files are redistributed unmodified as WOFF2 from the Fontsource packages, so the UI and the PNG export render the same on machines without those fonts and without a CDN. `apps/web/public/licenses/` holds `NOTICE.txt`, the verbatim OFL texts, `MPL-2.0.txt` and `noVNC-LICENSE.txt`.

Go module dependencies (`go.mod`). The license files below were read in the local module cache; the Go binaries do not yet ship a notice bundle, and the indirect dependencies were not reviewed.

| Module | Version | License file |
|---|---|---|
| `github.com/coder/websocket` | 1.8.15 | Permissive permission notice, Copyright 2025 Coder |
| `github.com/modelcontextprotocol/go-sdk` | 1.8.0 | States a transition from MIT to Apache-2.0; contributions keep their original license, with the Apache-2.0 text included |
| `golang.org/x/sys` | 0.49.0 | Go authors BSD-style license |
| `modernc.org/sqlite` | 1.61.0 | BSD-style license, plus `LICENSE-SQLITE`, `LICENSE-SQLITE_VEC` and `LICENSE-3RD-PARTY.md` |

Image contents. The Runtime image installs Debian trixie packages (Chromium, Xvfb, x11vnc, openbox, tini, xclip, fontconfig, Noto fonts and others from `docker/browser/packages.txt`). The development release generator writes a real SPDX package SBOM of the Runtime image from its dpkg database with `licenseConcluded` and `licenseDeclared` set to `NOASSERTION`, so Debian package licenses are not recorded there. The development manifest has no Gateway image SBOM. Debian package licenses remain to be collected from the packages themselves.

The M0 spike documents keep their original 2026-10-24 results. Where a later run changes what is known, the spike carries a dated note that points to [ACCEPTANCE.md](ACCEPTANCE.md#evidence-status).
