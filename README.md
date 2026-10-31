# LensDock

[Traditional Chinese](README.zh-TW.md)

A persistent Chromium workspace that you and your coding Agent can use together. Watch the real browser through noVNC, take control when needed, and send annotated screenshots to the Agent without sharing your everyday browser profile.

- Headful Chromium with a dedicated and persistent profile.
- Automatic control on desktop interaction, with explicit release, pause and resume.
- Screenshot annotation, blank-canvas drawing, solid redaction, and PNG copy/download.
- Feedback delivery through CLI or MCP; Agent control of the same browser's pages, native menus and extensions.

## Install

You need macOS or Linux on amd64/arm64 and a running Docker engine with Compose v2. Give the Docker host at least 2 CPUs and roughly 8 GiB of memory. Chromium's own renderer sandbox is off by default so any Docker host works; the container keeps its non-root user, read-only root filesystem, dropped capabilities and seccomp profile. To turn the Chromium sandbox on, set `LENSDOCK_CHROMIUM_SANDBOX=true` in `~/.local/share/lensdock/runtime.env` and recreate the Runtime with `lensdock update compose --deployment ~/.local/share/lensdock/deployment.json -- up -d runtime`. It then needs a host that allows unprivileged user namespaces, and the Runtime stays not ready instead of falling back.

### Private repository access

This repository and its GHCR packages require access. Install [GitHub CLI](https://cli.github.com/), then authenticate once:

```bash
gh auth login --scopes read:packages
gh auth token | docker login ghcr.io --username "$(gh api user --jq .login)" --password-stdin
```

Your GitHub account must have access to the repository and packages. Credentials stay in GitHub CLI and Docker's credential store; do not put tokens in URLs or commit them.

### One-line installation

```bash
bash -o pipefail -c 'gh release download --repo JiaWeiXie/lensdock --pattern install.sh --output - | bash'
```

The installer downloads the latest GitHub Release, verifies the archive's SHA-256 checksum, installs `lensdock` in `~/.local/bin`, and starts a new local workspace. Add that directory to your `PATH` if needed. It does not overwrite an existing deployment or upgrade its browser data.

For an Agent machine that only needs the CLI, append `--install-only`:

```bash
bash -o pipefail -c 'gh release download --repo JiaWeiXie/lensdock --pattern install.sh --output - | bash -s -- --install-only'
```

### Install with mise

With [mise](https://mise.jdx.dev/) installed and activated in your shell:

```bash
MISE_GITHUB_TOKEN="$(gh auth token)" mise use -g 'github:JiaWeiXie/lensdock[bin_path=.]@latest'
lensdock admin product-init --state-dir "$HOME/.local/share/lensdock"
lensdock admin product-start --state-dir "$HOME/.local/share/lensdock"
```

`bin_path=.` selects the host CLI rather than the bundled Linux update helpers. On an Agent-only machine, run just the first command. The token is passed only to that mise process, including when GitHub CLI stores it in the system keychain. See [mise's token configuration](https://mise.jdx.dev/dev-tools/github-tokens.html) for other authentication methods.

Mise's default [24-hour release-age policy](https://mise.jdx.dev/security.html#minimum-release-age) can hide a new release from `@latest`. To install a same-day release, replace `@latest` with its exact version, such as `@0.3.0`. This keeps the global release-age protection enabled.

### Open the workspace

Open **http://127.0.0.1:18081/** and pair with the contents of the private `human-token` (or `admin-token`) file under `~/.local/share/lensdock/product`; `product-start` prints the path. Every browser session needs a token. Gateway restarts clear sessions, so pair again after one. Never paste credentials into an issue, chat, URL, or screenshot. To enter a local, single-workspace Gateway without a token, opt in explicitly (see [Optional access and quota settings](#optional-access-and-quota-settings)).

The default workspace is `default`. Configuration and credentials live under `~/.local/share/lensdock`; the browser profile, downloads, database, and feedback images live in Docker volumes. Separate `human-token`, `viewer-token`, and `agent-token` files limit each role's access. The web UI, remote Chromium, and desktop default to English. The web UI also offers Traditional Chinese in Settings and preserves your saved choice.

### Docker images

| Component | Default image |
| --- | --- |
| Browser Runtime | `ghcr.io/jiaweixie/lensdock/runtime:latest` |
| Gateway and web UI | `ghcr.io/jiaweixie/lensdock/gateway:latest` |

Both images contain Linux amd64 and arm64 variants. New installations pull `latest`; the first start pins the resolved images so a later restart cannot silently upgrade a profile. Set `LENSDOCK_RUNTIME_IMAGE` and `LENSDOCK_GATEWAY_IMAGE` before first initialization to select matching version tags or digests instead. Use the host update workflow for existing installations.

## Multiple local projects

Each project gets its own Runtime/Gateway pair, persistent volumes, credentials, workspace ID and stable loopback origin. The standalone workspace on port 18081 stays separate.

With an installed release bundle and Docker running:

```bash
lensdock project up --project "$HOME/code/site-a"
lensdock workspace status --project "$HOME/code/site-a"
lensdock feedback list --project "$HOME/code/site-a"
lensdock mcp serve --project "$HOME/code/site-a"
```

The first project-bound command initializes the host manager, registers the directory and starts the workspace automatically. `project up` defaults to the current directory. Project-bound MCP and feedback commands also start a stopped workspace; a separate `up` is optional. Explicit `--project` ignores stale `LENSDOCK_API` and `LENSDOCK_TOKEN_FILE` values, but cannot be combined with explicit API, workspace or token flags.

The default manager directory is `~/.local/share/lensdock-manager`; set `LENSDOCK_MANAGER_DIR` consistently for a custom location. Initialization copies the host executable and bundle into that private directory and fixes the local Docker engine. It does not install a boot service or manage your application servers or databases. Image selection and initial pull policy use the same `LENSDOCK_RUNTIME_IMAGE`, `LENSDOCK_GATEWAY_IMAGE` and `LENSDOCK_PULL` settings as standalone initialization.

`up` returns the verified API origin, workspace and Agent token-file location. Open that origin and pair with the project's private `human-token` under its state directory (`lensdock admin project list --json` shows `stateDir`; the file is `<stateDir>/product/human-token`). Sessions and local sign-out markers stay separate even when Gateways share a hostname. See [Agent integration](docs/AGENT-INTEGRATION.md#project-scoped-local-agents) for project-scoped MCP configurations.

Use **Project / Workspace** in the toolbar to switch between local browsers. Switching releases your current control lease and opens the selected workspace; it does not stop either browser, reload its pages or interrupt background downloads. A failed lease release keeps you in the current workspace. Each destination uses its own session and credentials.

Started deployments share non-secret discovery records in `~/.local/share/lensdock-destinations`. Set `LENSDOCK_DESTINATIONS_DIR` consistently before initialization to use another directory. Existing installations need the current CLI and Gateway, then one `admin product-start` to add the read-only discovery mount without replacing their copied Compose file or data. Use the normal update workflow for their images; never rerun `product-init` to upgrade.

CPU, memory, helper process limits and aggregate budgets are off by default. Uncapped external containers do not block starts. Optional restrictions are configured with `admin manager policy`; `admin project limits --id ID ...` changes an existing stopped project's caps. Zero removes a cap, and omitted dimensions stay unchanged.

To require manual registration or startup, opt into `--require-project-authorization` or `--require-explicit-start`. Manual setup remains available through `admin manager init` and `admin project authorize`. Existing configured policies and revoked grants are not silently reset.

When an enabled budget is full, starts wait FIFO without evicting another browser. Requests for the same project share one operation. Waiting has no default deadline; use `--wait` when you want one. Canceling a wait affects only that caller; admitted work continues. Releasing a lease, closing MCP, pausing the Agent or closing the UI never stops Chromium.

Stop explicitly when you want to discard in-memory pages and network work:

```bash
lensdock project stop --project "$HOME/code/site-a"
```

Persistent profiles and feedback remain. `--discard-page-state` is required only when `--require-stop-confirmation` is enabled. Active leases still prevent a normal stop; a forced stop requires the observed runtime generation. Moved, replaced or explicitly revoked directories need administration rather than automatic repair. The manager socket prevents accidental cross-project routing, not hostile processes running as your OS user. See the [update runbook](docs/UPDATE-RUNBOOK.md#managed-project-lifecycle) for policy and recovery.

## Use the workspace

1. Click the desktop or start typing into it to request control automatically. If the Agent is working, LensDock hands control over first. The activating gesture is not replayed into the changed page; wait for the control indicator, then continue.
2. Use the text-and-files panel to paste text composed with your local IME or transfer files.
3. Capture a screenshot or create a drawing, add arrows, shapes, text, or emphasis, then Send. Notes are optional. Use solid redaction for secrets; blur and pixelate are cosmetic.
4. Follow Sent, In progress and Done in the feedback panel. Expand details to distinguish queued from read, and Agent completion from your verification.

Copy PNG and Download PNG save the flattened image without publishing it. Closing the UI does not close Chromium or clear its profile.

If the browser window is minimized or missing, right-click the **empty remote desktop** and choose **Restore browser**. It brings back an existing window or opens one in the same Chromium instance and Profile. If the first click acquires control, wait for the control indicator and right-click again. Right-clicking inside a web page still opens Chromium's normal context menu.

### Choose a usage mode

`AGENT` is the default: it enables annotation, feedback, and Agent control of the whole browser, including its address bar, menus, native dialogs and extensions. `NOVNC` provides only the remote browser, human control, clipboard, and file transfer. To start in browser-only mode, set `LENSDOCK_MODE=NOVNC` before installation or pass `--mode NOVNC` to `product-init`.

Change modes in Settings. Pause Agent immediately revokes its control while you continue taking screenshots and writing feedback. Resume allows a new lease; it never restores the old one. Mode and pause survive restarts. Changing the startup environment later does not override saved settings.

### Optional access and quota settings

Every browser session requires a pairing token, and every Gateway whose origin is not loopback, or that serves more than one workspace, can only be used that way. Automatic local entry is an opt-in for a loopback, single-workspace Gateway: pass `--local-auto-entry` at initialization (`admin product-init`, or `admin manager init` for managed projects), or set `LENSDOCK_LOCAL_AUTO_ENTRY=1` in `product/compose.env` and restart the Gateway. It trusts request headers, so any process on the host that can reach the Gateway, including a local proxy, can then open a human session without a token. Browser protections such as Sec-Fetch headers, origin checks and Local Network Access prompts apply to web pages only and do not stop such a process. Use it only on a single-user machine with no such software. `--require-pairing` or `LENSDOCK_REQUIRE_PAIRING=1` always wins over the opt-in. Automatic entries are recorded in the audit trail as `session.local`. A Gateway that was configured without either option now requires pairing after it is updated to this version.

Sessions have no default application expiry or count cap. Feedback images, drafts, file transfers and clipboard text have no default product quotas, and drafts do not expire. Positive limits are opt-in; zero disables them. The [runbook](docs/UPDATE-RUNBOOK.md#resource-policy) lists the configuration surfaces. Available RAM, disk space and browser format limits still apply.

## Set up an Agent

LensDock works with an existing coding Agent. It does not run an LLM or require an Agent Skill.

1. Install the CLI on the Agent's machine using either method above. Keep Agent collaboration enabled in Settings.
2. Give the Agent the workspace's `product/agent-token` through a private channel. Preserve file mode `0600`. Browser work needs no Host Admin credential.
3. Copy the English setup prompt from Settings into the Agent's instructions. Supply the actual API URL and token-file path separately. The copied prompt contains no credentials.

For an Agent on the same machine:

```bash
export LENSDOCK_API=http://127.0.0.1:18081
export LENSDOCK_TOKEN_FILE="$HOME/.local/share/lensdock/product/agent-token"
lensdock feedback list --workspace default
lensdock workspace watch default
```

Keep `workspace watch` running in its own process. Stop browser work if it reports a pause, `NOVNC`, or loses the connection. After a human resumes, the next control command obtains a fresh lease automatically. A notification is a state feed, not proof that a model read it.

### Codex

Resolve the CLI and token file first. With mise, use `mise which lensdock` instead of `command -v lensdock`:

```bash
LENSDOCK_BIN="$(command -v lensdock)"
TOKEN_FILE="$HOME/.local/share/lensdock/product/agent-token"
codex mcp add lensdock -- "$LENSDOCK_BIN" mcp serve --workspace default \
  --api http://127.0.0.1:18081 --token-file "$TOKEN_FILE"
codex mcp get lensdock
```

This writes the server to `~/.codex/config.toml`. Start a new Codex session and run `/mcp` to check the connection.

### Claude Code

Using the same variables:

```bash
claude mcp add --transport stdio --scope user lensdock -- "$LENSDOCK_BIN" mcp serve \
  --workspace default --api http://127.0.0.1:18081 --token-file "$TOKEN_FILE"
claude mcp get lensdock
```

`--scope user` makes the server available across projects. Start a new Claude Code session and run `/mcp`.

### OMP and other MCP clients

For OMP, merge this entry into `~/.omp/agent/mcp.json`, or `.omp/mcp.json` for one project. Other clients that accept `mcpServers` can use the same entry. Replace `/Users/alice` with your home directory and use the absolute CLI path returned above; JSON does not expand `$HOME` or `~`.

```json
{
  "mcpServers": {
    "lensdock": {
      "type": "stdio",
      "command": "/Users/alice/.local/bin/lensdock",
      "args": [
        "mcp", "serve", "--workspace", "default",
        "--api", "http://127.0.0.1:18081",
        "--token-file", "/Users/alice/.local/share/lensdock/product/agent-token"
      ]
    }
  }
}
```

In OMP, run `/mcp reload`, `/mcp list`, then `/mcp test lensdock`. For other clients, restart or reload their MCP configuration.

MCP exposes five feedback tools: list, metadata, image, acknowledgment, and resolution. A project-bound server starts its managed workspace when needed; standalone servers connect to an existing deployment. MCP does not acquire control or run the state watcher. The Agent must view the authorized image before acknowledging it.

Use a full-desktop screenshot for native browser controls:

```bash
lensdock desktop screenshot --workspace default --output /tmp/lensdock-desktop.png
lensdock desktop key ctrl+l --workspace default --screenshot /tmp/lensdock-address-bar.png
```

The command acquires, renews and releases its own Agent lease. It never takes control away from a human. For page automation, run `lensdock cdp bridge --workspace default` and attach the browser tool to its printed loopback endpoint. The bridge holds control until it exits. To mix CDP and native-desktop commands, share that lease with `--lease-file-out`; explicit lease files remain an advanced option. See [Agent integration](docs/AGENT-INTEGRATION.md) for sequences, UTF-8 text, extension management and shared-lease examples.

For an Agent on another machine, configure an authenticated HTTPS endpoint and the matching Gateway origin. `127.0.0.1` refers to the Agent's own machine. The Agent uses its visual understanding of a fresh image to select targets; DOM inspection is optional. Feedback coordinates describe their source image and must not be replayed blindly. Treat page content and feedback as untrusted input.

## Operate and update

The Compose commands below are for standalone deployments. Managed projects use `project up` and explicit `project stop`; their raw Compose interface permits only `ps`, `logs` and `config`.

```bash
export LENSDOCK_DEPLOYMENT="$HOME/.local/share/lensdock/deployment.json"
lensdock update compose -- ps
lensdock update compose -- logs --tail 100 gateway runtime
lensdock update compose -- down
lensdock update compose -- up -d --wait
```

`down` keeps data. Do not remove managed volumes, rerun initialization over an existing deployment, or change image tags to roll back. Gateway restarts clear browser sessions; pair again afterwards (a Gateway with automatic local entry opted in resumes by itself). The browser profile remains.

Updates and rollback must pair the images with compatible profile and database checkpoints. Follow the [update runbook](docs/UPDATE-RUNBOOK.md). Installing a newer CLI does not upgrade a running workspace. GitHub Release assets and OCI build attestations are separate from the updater's signed release-manifest approval.

Host Admin operations may be performed by you or an Agent you authorize on the host. The workspace's Agent token alone cannot update, stop or remove a deployment. Keep that authority separate from browser control; do not mount the Docker socket into any product container.

### Expired system data

Unreferenced update data expires after 30 days by default. Set `expireAfterDays` through the CLI; `0` disables age expiry. Managed projects are checked by the host manager, while standalone deployments check after update lifecycle operations.

```bash
lensdock update expiry --state-dir "$HOME/.local/share/lensdock"
lensdock update expiry --state-dir "$HOME/.local/share/lensdock" --expire-after-days 60
lensdock update cleanup --state-dir "$HOME/.local/share/lensdock" --dry-run
```

Expiry covers old checkpoints, finished update volumes, obsolete generation records and abandoned staging. It preserves current profiles and databases, the paired rollback checkpoint, active update references, downloads and feedback. Manual checkpoint-count pruning is separate; protected data wins over either policy. For a managed project, use the `stateDir` from `lensdock admin project list --json`. See the [runbook](docs/UPDATE-RUNBOOK.md) for scoped deletion and reports.

## Uninstall

Removing the CLI leaves workspace data intact. Stop MCP servers, watchers and bridges first so they cannot restart a managed workspace. Stop each managed workspace with `lensdock project stop --project /absolute/project/path`; stop a standalone deployment with `lensdock update compose --state-dir /absolute/state/path -- down`. Then run:

```bash
lensdock admin manager stop
```

This stops only the host manager and waits for it to exit. It refuses while a worker is active; it neither stops containers nor removes data. Use the same `LENSDOCK_MANAGER_DIR` as the installation.

For a mise installation, remove LensDock's global selection, preview the installed versions, then remove only this tool:

```bash
mise unuse --global --no-prune github:JiaWeiXie/lensdock
mise uninstall --all github:JiaWeiXie/lensdock --dry-run
mise uninstall --all github:JiaWeiXie/lensdock
```

Remove project-specific mise selections separately with `mise unuse --path /exact/mise.toml --no-prune github:JiaWeiXie/lensdock`. Do not delete a shared mise configuration.

For the shell installer, inspect `readlink "$HOME/.local/bin/lensdock"`, then remove the owned symlink and the LensDock version directories under `~/.local/lib/lensdock`. Respect `LENSDOCK_BIN_DIR` and `LENSDOCK_INSTALL_DIR` overrides; a custom installation parent may contain unrelated files.

Keep `~/.local/share/lensdock`, `~/.local/share/lensdock-manager`, custom backup locations and encryption keys if you want to reuse the data. The manager directory contains its own executable copy, credentials and every managed project's state.

To remove data permanently, follow [Uninstall and data removal](docs/UPDATE-RUNBOOK.md#uninstall-and-data-removal) **before deleting deployment metadata or the CLI**. It inventories exact containers, volumes, checkpoints and keys, including update-created volumes that Compose labels omit. Never run global Docker prune or delete the application's project directory. Deleted data has no undelete path; recovery needs a compatible backup and its key.


## Develop and release

```bash
mise install
mise run build
mise run build:linux
mise run web:build
mise run image:build
mise run gateway:image
mise run product:init
mise run product:up
```

Source development uses local `:dev` images and `.lensdock/` state. Run `mise run check` and `mise exec -- pnpm --dir apps/web test` before submitting changes.

Changes on `main` produce versioned GitHub Releases and GHCR images after checks. The release workflow builds both native Linux architectures, runs container smoke checks, and collects image supply-chain evidence. `latest` tracks the newest successful stable release. Release archives include macOS/Linux CLIs, deployment files, Linux update helpers, and checksums.

## Reference and limits

- [Agent integration](docs/AGENT-INTEGRATION.md), [update runbook](docs/UPDATE-RUNBOOK.md), and [web UI details](apps/web/README.md).
- Integrated browser and update workflows have been exercised on macOS arm64 with OrbStack. Native release smoke checks do not establish full product E2E coverage on every platform. Japanese/Korean native IMEs, a unified multi-workspace UI, and cross-machine checkpoint portability remain outside the verified scope.
- Keep the default loopback binding unless you configure TLS and authentication. Never mount the Docker socket into the browser, Gateway, or Agent.
- LensDock's project license has not been selected. Bundled dependencies retain their own licenses; see [references](docs/REFERENCES.md).
