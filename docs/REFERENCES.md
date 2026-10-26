# References and third-party licenses

Upstream sources below were last checked on 2026-10-24. They are design references, not proof that LensDock passes any integration test. Upstream branches and websites change, so pin a commit, release or digest before relying on one.

## Upstream projects

**vercel-labs/agent-browser** (https://github.com/vercel-labs/agent-browser). A CLI that controls an existing browser. LensDock pins a version and checks CDP attach and detach, that no local browser is launched by mistake, target selection and bridge compatibility. Upstream declares the Apache-2.0 license: https://github.com/vercel-labs/agent-browser/blob/main/LICENSE. Its screenshot element labels are not assumed to equal LensDock's human annotation workflow.

**noVNC** (https://github.com/novnc/noVNC). The core is MPL-2.0, and some assets carry other licenses (https://github.com/novnc/noVNC/blob/master/LICENSE.txt). LensDock imports the npm package `@novnc/novnc` 1.7.0 unmodified, uses only the public RFB API and ships the license files with the UI (`apps/web/public/licenses/`). Any reuse or modification still has to follow each file's actual license. This file records upstream declarations and is not a legal review.

## Other sources

| Source | Used for |
|---|---|
| https://novnc.com/noVNC/docs/LIBRARY.html and https://novnc.com/noVNC/docs/API.html | Integrating the RFB library into a custom client; public API (`viewOnly`, `scaleViewport`, clipboard, `toBlob`) |
| https://www.debian.org/releases/ and https://hub.docker.com/_/debian | Debian stable (13, trixie) and official slim image tags |
| https://packages.debian.org/trixie/chromium | Debian Chromium security package and architectures |
| https://manpages.debian.org/trixie/x11vnc/x11vnc.1.en.html | x11vnc options such as view-only, shared and forever |
| https://packages.debian.org/trixie/fonts-noto-cjk, `fonts-noto-color-emoji`, `fonts-noto-core` | CJK, color emoji and core Unicode font packages |
| https://modelcontextprotocol.io/specification/2026-07-28/server/tools | MCP tool text, image and structured results |
| https://developers.openai.com/codex/mcp/ and https://code.claude.com/docs/en/mcp | Codex and Claude Code MCP client configuration |
| https://github.com/can1357/oh-my-pi/blob/main/docs/tools/browser.md | OMP CDP, target, screenshot and lifecycle behavior |
| https://chromedevtools.github.io/devtools-protocol/tot/Browser/, `/Extensions/`, `/Page/` and `/Runtime/` | Browser lifecycle, extension management, page automation and evaluation |
| https://developer.chrome.com/blog/remote-debugging-port | Chrome's restriction on remote debugging with the default data directory |
| https://chromium.googlesource.com/chromium/src/+/main/docs/security/apparmor-userns-restrictions.md | User-namespace sandbox limits on hosts with AppArmor |
| https://www.docker.com/resources/what-container/ | Containers use the host kernel |
| https://docs.docker.com/build/building/best-practices/ | Image rebuilds, versions and digest pinning |
| https://github.com/rfbproto/rfbproto/blob/master/rfbproto.rst | RFB wire protocol for the Agent's lease-bound native-desktop client |
| https://mise.jdx.dev/cli/unuse.html and https://mise.jdx.dev/cli/uninstall.html | Removing LensDock tool configuration and installed CLI versions |
| https://docs.docker.com/reference/cli/docker/volume/rm/ and https://docs.docker.com/reference/cli/docker/volume/ls/ | Exact volume removal, in-use refusal and label-based inventory |

## Recording a new dependency

For every dependency that ships in the product, record the project, upstream URL, version or commit, checksum, license, modified files, purpose and any notice required at distribution. Mark a design-only reference separately from copied source. Never add font files, full upstream source or prebuilt binaries to the repository without such a record.

## Shipped dependencies and licenses (2026-10-25)

The annotation editor uses repository-owned React/SVG code and Canvas raster export. The package manifests list its dependencies.

LensDock's own license has not been selected. Bundled dependencies retain their licenses; release packaging must account for their notices and SBOM inputs.

Web UI runtime dependencies (`apps/web/package.json`). The license column is what each installed package declares in its own `package.json`: an upstream declaration, not a legal review.

| Project | Version | Declared license | Use |
|---|---|---|---|
| `@novnc/novnc` | 1.7.0 | MPL-2.0 | RFB client library, imported unmodified, public API only |
| `react`, `react-dom` | 19.3.1 | MIT | UI |
| `@fontsource/ibm-plex-sans`, `@fontsource/ibm-plex-mono` | 5.3.0 | OFL-1.1 | UI fonts, served from the bundle |
| `@fontsource/noto-sans-tc`, `-sc`, `-jp`, `-kr` | 5.3.1 | OFL-1.1 | CJK UI and editor export fonts |
| `@fontsource/noto-color-emoji` | 5.3.2 | OFL-1.1 | Emoji in the UI and the editor export |

The font files are redistributed unmodified as WOFF2 from the Fontsource packages, so the UI and the PNG export render the same on machines without those fonts and without a CDN. `apps/web/public/licenses/` holds `NOTICE.txt`, the verbatim OFL texts, `MPL-2.0.txt` and `noVNC-LICENSE.txt`.

Go module dependencies (`go.mod`). The license files below were read in the local module cache. The Go binaries do not yet ship a notice bundle, and indirect dependencies were not reviewed.

| Module | Version | License file |
|---|---|---|
| `github.com/coder/websocket` | 1.8.15 | Permissive permission notice, Copyright 2025 Coder |
| `github.com/modelcontextprotocol/go-sdk` | 1.8.0 | States a transition from MIT to Apache-2.0; contributions keep their original license, with the Apache-2.0 text included |
| `golang.org/x/sys` | 0.49.0 | Go authors BSD-style license |
| `modernc.org/sqlite` | 1.61.0 | BSD-style license, plus `LICENSE-SQLITE`, `LICENSE-SQLITE_VEC` and `LICENSE-3RD-PARTY.md` |

Image contents. The Runtime image installs Debian trixie packages (Chromium, Xvfb, x11vnc, openbox, tini, xclip, fontconfig, Noto fonts and others from `docker/browser/packages.txt`). The development release generator writes a real SPDX package SBOM of the Runtime image from its dpkg database, with `licenseConcluded` and `licenseDeclared` set to `NOASSERTION`, so Debian package licenses are not recorded there. The development manifest has no Gateway image SBOM. Debian package licenses remain to be collected from the packages themselves.
