# LensDock

[Traditional Chinese](README.zh-TW.md)

A persistent Chromium workspace that you and your coding Agent can use together. Watch the real browser through noVNC, take control when needed, and send annotated screenshots to the Agent without sharing your everyday browser profile.

- Headful Chromium with a dedicated, persistent profile and an enabled sandbox.
- Human takeover and server-enforced Agent leases, with pause and resume controls.
- Screenshot annotation, blank-canvas drawing, solid redaction, and PNG copy/download.
- Feedback delivery through the CLI or MCP; browser control through a lease-scoped CDP bridge.

## Install

You need macOS or Linux on amd64/arm64, a running Docker engine with Compose v2, and a host that supports Chromium's user-namespace sandbox. Give the Docker host at least 2 CPUs and roughly 8 GiB of memory. LensDock does not disable the sandbox to work around an incompatible host.

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

Open **http://127.0.0.1:18081/** and pair with the contents of `~/.local/share/lensdock/product/admin-token`. Read this private file locally; never paste it into an issue, chat, URL, or screenshot.

The default workspace is `default`. Configuration and credentials live under `~/.local/share/lensdock`; the browser profile, downloads, database, and feedback images live in Docker volumes. Separate `human-token`, `viewer-token`, and `agent-token` files limit each role's access. The web UI, remote Chromium, and desktop default to English. The web UI also offers Traditional Chinese in Settings and preserves your saved choice.

### Docker images

| Component | Default image |
| --- | --- |
| Browser Runtime | `ghcr.io/jiaweixie/lensdock/runtime:latest` |
| Gateway and web UI | `ghcr.io/jiaweixie/lensdock/gateway:latest` |

Both images contain Linux amd64 and arm64 variants. New installations pull `latest`; the first start pins the resolved images so a later restart cannot silently upgrade a profile. Set `LENSDOCK_RUNTIME_IMAGE` and `LENSDOCK_GATEWAY_IMAGE` before first initialization to select matching version tags or digests instead. Use the host update workflow for existing installations.

## Multiple local projects

Each authorized project gets its own Runtime/Gateway pair, persistent volumes, credentials, workspace ID and stable loopback origin. A host manager admits starts against a shared resource budget. The existing standalone workspace on port 18081 stays separate; the manager counts its resource limits as external demand.

Initialize the manager once from a release bundle containing the deployment files and both Linux helper binaries. A human must authorize each existing project directory:

```bash
lensdock admin manager init --bundle-dir /path/to/lensdock-release
lensdock admin project authorize "$HOME/code/site-a" "$HOME/code/site-b"
lensdock admin project list
```

The default manager directory is `~/.local/share/lensdock-manager`; set `LENSDOCK_MANAGER_DIR` consistently for a custom location. Initialization copies the host executable and bundle into that private directory and fixes the local Docker engine. It does not install a boot service or manage your application servers or databases.

An Agent can then request its authorized project without Docker access:

```bash
unset LENSDOCK_API LENSDOCK_TOKEN_FILE
lensdock project up --project "$HOME/code/site-a" --wait 10m
lensdock workspace status --project "$HOME/code/site-a"
lensdock feedback list --project "$HOME/code/site-a"
lensdock mcp serve --project "$HOME/code/site-a"
```

`up` returns the verified API origin, workspace and Agent token-file location. Open that origin and pair using the project's `product/human-token`, under the `stateDir` reported by `admin project list --json`. Sessions and demo sign-out markers are separate even when the Gateways share a hostname. Use project-scoped MCP configurations; see [Agent integration](docs/AGENT-INTEGRATION.md#project-scoped-local-agents).

The default limits are 2 GiB / 2 CPUs per Runtime and 256 MiB / 0.5 CPU per Gateway. The manager's project budget is 6.75 GiB / 7.5 CPUs, with a separate 1 GiB / 1 CPU helper allowance and 2 GiB / 1 CPU of system headroom. Physical engine capacity and external container limits can reduce admissible concurrency. Uncapped external containers require an explicit human allowance or admission is refused. An allowance does not constrain those containers.

When full, starts wait FIFO without evicting another browser. Requests for the same project share one operation. Canceling or timing out a wait removes only that caller's interest; admitted work continues. Releasing a lease, closing MCP, pausing the Agent or closing the UI never stops Chromium.

Stopping requires explicit consent to lose in-memory pages and network work:

```bash
lensdock project stop --project "$HOME/code/site-a" --discard-page-state
```

The persistent profile and feedback remain for the next start. An active lease prevents this stop; a human can force it only with the observed runtime generation. Directory aliases resolve to one grant, worktrees get separate grants, and a moved or replaced directory needs human administration. The private manager socket prevents accidental cross-project routing; it does not isolate hostile processes running as your OS user. Resource policy, recovery and measured limits are in the [update runbook](docs/UPDATE-RUNBOOK.md#managed-project-lifecycle).

## Use the workspace

1. Watch without taking control. Acquire control to use the desktop, or take over from the Agent.
2. Use the text-and-files panel to paste text composed with your local IME or transfer files.
3. Capture a screenshot or create a drawing, add arrows, shapes, text, or emphasis, then Send. Notes are optional. Use solid redaction for secrets; blur and pixelate are cosmetic.
4. Review the Agent's response before marking the task verified. Queued, acknowledged, resolved, and verified are separate states.

Copy PNG and Download PNG save the flattened image without publishing it. Closing the UI does not close Chromium or clear its profile.

### Choose a usage mode

`AGENT` is the default: it enables annotation, feedback, and Agent browser control. `NOVNC` provides only the remote browser, human control, clipboard, and file transfer. To start in browser-only mode, set `LENSDOCK_MODE=NOVNC` before installation or pass `--mode NOVNC` to `product-init`.

Change modes in Settings. Pause Agent immediately revokes its control while you continue taking screenshots and writing feedback. Resume allows a new lease; it never restores the old one. Mode and pause survive restarts. Changing the startup environment later does not override saved settings.

### Local demo without token entry

For a trusted, single-user local demo, set `LENSDOCK_DEMO_NO_AUTH=1` in the deployment's `product/compose.env` and restart the Gateway. This allows automatic human pairing on loopback only. Keep the default `0` for normal use; never expose demo mode through a proxy or tunnel.

## Set up an Agent

LensDock works with an existing coding Agent. It does not run an LLM or require an Agent Skill.

1. Install the CLI on the Agent's machine using either method above. Keep Agent collaboration enabled in Settings.
2. Give the Agent only the workspace's `product/agent-token` through a private channel. Preserve file mode `0600`; do not provide the admin token or Docker access.
3. Copy the English setup prompt from Settings into the Agent's instructions. Supply the actual API URL and token-file path separately. The copied prompt contains no credentials.

For an Agent on the same machine:

```bash
export LENSDOCK_API=http://127.0.0.1:18081
export LENSDOCK_TOKEN_FILE="$HOME/.local/share/lensdock/product/agent-token"
lensdock feedback list --workspace default
lensdock workspace watch default
```

Keep `workspace watch` running in its own process. Stop browser work if it reports a pause, `NOVNC`, or loses the connection. After a human resumes, acquire a fresh lease. A notification is a state feed, not proof that a model read it.

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

MCP exposes five feedback tools: list, metadata, image, acknowledgment, and resolution. It does not start a browser, acquire a lease, or run the state watcher. The Agent must view the authorized image before acknowledging it. Browser control uses `lensdock lease acquire` and `lensdock cdp bridge`; connect your browser tool to that bridge, not to an unrestricted Chromium port. See [Agent integration](docs/AGENT-INTEGRATION.md) for the full sequence, Codex TOML, and tool schemas.

For an Agent on another machine, configure an authenticated HTTPS endpoint and the matching Gateway origin. `127.0.0.1` refers to the Agent's own machine. Treat page content and feedback as untrusted input, and observe the current page before acting on screenshot coordinates.

## Operate and update

The Compose commands below are for standalone deployments. Managed projects use `project up` and explicit `project stop`; their raw Compose interface permits only `ps`, `logs` and `config`.

```bash
export LENSDOCK_DEPLOYMENT="$HOME/.local/share/lensdock/deployment.json"
lensdock update compose -- ps
lensdock update compose -- logs --tail 100 gateway runtime
lensdock update compose -- down
lensdock update compose -- up -d --wait
```

`down` keeps data. Do not remove managed volumes, rerun initialization over an existing deployment, or change image tags to roll back. Gateway restarts require pairing again; the browser profile remains.

Updates and rollback must pair the images with compatible profile and database checkpoints. Follow the [update runbook](docs/UPDATE-RUNBOOK.md). Installing a newer CLI does not upgrade a running workspace. GitHub Release assets and OCI build attestations are separate from the updater's signed release-manifest approval.

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

CI/CD follows [comfyui-pwa-client](https://github.com/JiaWeiXie/comfyui-pwa-client): changes on `main` produce versioned GitHub Releases and GHCR images after checks. The release workflow builds both native Linux architectures, runs container smoke checks, and collects image supply-chain evidence. `latest` tracks the newest successful stable release. Release archives include macOS/Linux CLIs, deployment files, Linux update helpers, and checksums.

## Reference and limits

- [Agent integration](docs/AGENT-INTEGRATION.md), [update runbook](docs/UPDATE-RUNBOOK.md), and [web UI details](apps/web/README.md).
- Full human/Agent handoff and paired-update demonstrations were performed on macOS arm64 with OrbStack. Three managed project pairs, scoped browser sessions and lifecycle fault recovery were also exercised there; see the runbook for workload measurements and limits. Native release smoke checks do not establish full product E2E coverage on every platform. Japanese/Korean native IMEs, a unified multi-workspace UI, and cross-machine checkpoint portability remain outside the verified scope.
- Keep the default loopback binding unless you configure TLS and authentication. Never mount the Docker socket into the browser, Gateway, or Agent.
- LensDock's project license has not been selected. Bundled dependencies retain their own licenses; see [references](docs/REFERENCES.md).
