# Update and rollback runbook

Host updates are implemented in the `lensdock update` CLI. This runbook states the policy, the order of operations and what has actually been verified. Exact flags, deployment file fields and recovery procedures are in [UPDATE-IMPLEMENTATION.md](UPDATE-IMPLEMENTATION.md).

## Responsibilities

- Browser and Debian userspace change through a new Runtime image.
- Gateway, noVNC and the UI change through a new Gateway (app) image.
- The host kernel and Docker Engine belong to the host administrator.
- The Runtime and Gateway containers never mount the Docker socket, and the product never will. Only the host CLI applies or rolls back an update, and only with Host Admin authority. An authorized human or a host Agent that a human lets run host `lensdock` commands may exercise it. A browser or container Agent token never grants it, so an Agent token cannot call it.
- Building or testing an update candidate never restarts a browser that someone is working in.

## Command contract

```text
lensdock update check [--workspace <id>]
lensdock update plan --release <id|dir> [--workspace <id>] [--output <file>] [--allow-dev] [--allow-downgrade]
lensdock update apply --plan <plan-file> [--yes] [--approve-switch] [--revoke-leases] [--accept-crash-snapshot] [--dry-run]
lensdock update approve --update <id> --yes
lensdock update abort --update <id> [--keep-candidate]
lensdock update rollback --checkpoint <checkpoint-id> [--yes] [--revoke-leases] [--allow-crash-checkpoint] [--dry-run]
lensdock update status | checkpoints list|verify|prune | branches list|delete
lensdock update cleanup [--scope checkpoints,volumes,generations,staging] [--older-than-days N] [--dry-run] [--yes]
lensdock update expiry [--expire-after-days N]
lensdock update compose -- <up|down|stop|start|restart|ps|logs|config> ...
lensdock admin manager stop
```

Every command accepts `--deployment <file>`.

- `check` only reports. `plan` verifies image digests, signature, compatibility, disk space, backup location and maintenance impact without touching the browser.
- Keep each installed release directory at `<releasesDir>/<releaseId>`. Later plans need the running release's manifest there to verify Chromium and schema compatibility, even when the original plan used an external release directory.
- `apply` asks for confirmation. Commands and logs never print tokens, Profile content or secrets.
- The Gateway and UI have no apply or rollback endpoint. They read only `deployment-status.json`, which holds release, update and checkpoint summaries.

## Managed project lifecycle

The host manager owns deployments whose `deployment.json` has `managedBy` metadata. `admin project list --json` reports each project's ID, root, `stateDir`, origin, configured limits, reservation, measured usage and queue/blocker state. Its deployment file is `<stateDir>/deployment.json`. Supply that exact file to `update --deployment`; do not point at `<stateDir>/product/deployment.json`.

For these deployments:

- Project-bound commands initialize, register and start a workspace automatically by default. `project up` defaults to the current directory. Enable `--require-project-authorization` or `--require-explicit-start` through `admin manager policy` to restore manual registration or startup. `admin product-start --state-dir <stateDir>` also routes through admission; it cannot change the pull policy.
- `check` and `plan` default to the registered project Workspace when `--workspace` is omitted. An explicit Workspace must still belong to the deployment; standalone commands keep their existing default.
- Raw `update compose` permits only `ps`, `logs` and `config`. Use `project up` or `project stop` for lifecycle changes. Managed initialization and direct status-file publication are refused.
- Apply, approve, abort, rollback, checkpoint verification/pruning, branch deletion and cleanup use typed Host Admin operations, which an authorized human or a delegated host Agent may request. Read-only checks use the same fixed Docker engine and helper slot. Missing or mismatched manager ownership fails closed.
- A project holds one lifecycle lock. Workers also share a helper lock, so lifecycle operations are serialized while running project pairs remain independent. Child Docker processes inherit the locks. Helper memory, CPU and process caps are optional; candidates inherit the project's resource profile.
- Apply and rollback reserve a candidate pair before maintenance. Approve and abort reserve at least one project pair before restarting stopped services. A full queue cannot consume the helper allowance needed for an explicit stop. Admission rechecks waiter expiry and operation deadlines after Docker inventory returns. A canceled or disconnected CLI does not cancel admitted work.
- Image pins, generation records, encrypted checkpoints and paired Profile/keyring/database rollback still follow the existing updater. The manager does not make an image-only downgrade safe.

Without `--approve-switch`, a successful candidate verification leaves `apply` waiting for approval with a nonzero CLI exit status. Services remain stopped until approve or abort completes. Printed status, approve and abort commands retain the exact, shell-quoted `--deployment` selection.

The default root is `~/.local/share/lensdock-manager`, selected independently of standalone `LENSDOCK_DEPLOYMENT`. Initialization snapshots the host executable and release bundle and pins a local Unix-socket Docker endpoint and engine ID. Later caller `DOCKER_*`, `COMPOSE_*` or deployment environment overrides cannot retarget workers.

### Resource policy

New managers and projects have no memory, CPU or helper-process caps, aggregate budget or reserved headroom. Uncapped external containers are allowed. Zero means unlimited in each resource dimension; setting a CPU cap does not implicitly add a memory cap or forbid unrelated uncapped containers. Docker health, ownership and lifecycle locks still apply.

Use `lensdock admin manager policy` to opt in. `--memory-mib` and `--cpus` set aggregate budgets. `--runtime-memory-mib`, `--runtime-cpus`, `--gateway-memory-mib` and `--gateway-cpus` set limits for future registrations. A budget requires positive Runtime and Gateway caps in the budgeted dimension. Existing configured policies remain in effect after upgrading.

For an existing project, stop it and run `lensdock admin project limits --id <project-id>` with the dimensions to change. Omitted dimensions remain unchanged; explicit zero removes that cap. A concurrent change makes the command refuse its stale snapshot rather than overwrite another edit. Running, queued, approval-pending or otherwise unresolved projects cannot be resized. Policy changes never evict a running browser.

Positive CPU quantities must represent at least one nanocpu (`0.000000001` CPU). Smaller positive values are rejected rather than rounded to zero and silently removing a cap.

`--helper-memory-mib`, `--helper-cpus` and `--helper-pids-limit` configure the shared helper allowance. `--headroom-mib` and `--headroom-cpus` reserve engine capacity when admission accounts for that dimension. Helper policy changes are refused while its lock is held; workers reload the current policy after acquiring it.

With an enabled budget, feasible starts wait FIFO and impossible requests fail. `--allow-unbounded-external=false` opts into refusal alongside uncapped external containers in a reserved dimension. Optional `--unbounded-external-memory-mib` and `--unbounded-external-cpus` are accounting allowances, not caps on those containers. Measured usage can be unavailable and must not be read as zero. Waits have no default deadline; `--wait DURATION` opts into one.

Standalone Compose accepts `LENSDOCK_RUNTIME_MEMORY_LIMIT`, `LENSDOCK_RUNTIME_CPUS`, `LENSDOCK_GATEWAY_MEMORY_LIMIT`, `LENSDOCK_GATEWAY_CPUS`, `LENSDOCK_RUNTIME_PIDS_LIMIT` and `LENSDOCK_GATEWAY_PIDS_LIMIT` in `product/compose.env`. Unset or zero is unlimited. `LENSDOCK_HELPER_PIDS_LIMIT` optionally caps standalone helper processes, including the first-start credential helper (`config-init`); product start lists a deployment-owned `helper-policy.yaml` after the copied release Compose file, which stays byte for byte as installed. Managed CPU/memory and helper caps (including the helper PID cap for `config-init`) come from manager policy instead and override any deployment value.

### Optional access and content limits

The loopback, single-workspace Gateway lets a browser enter automatically. Initialization's `--require-pairing`, Gateway JSON `requirePairing`, or `LENSDOCK_REQUIRE_PAIRING=1` opts into token pairing. An explicitly supplied Gateway flag wins over the environment, which wins over JSON; an empty environment value preserves JSON. Remote and multi-workspace access still needs authentication. Logout stays logged out until explicit re-entry.

Gateway JSON `sessionLimits` accepts `ttlSeconds` and `maxSessions`; both default to zero. A positive count cap refuses new sessions instead of evicting another browser. Sessions remain process-local and disappear on restart; local access resumes automatically.

Feedback defaults have no quota or draft expiry. Use the Gateway's `feedbackLimits` object or these `product/compose.env` variables; positive integers enforce limits, zero disables them, and empty values preserve JSON:

| Variable | Restriction |
|---|---|
| `LENSDOCK_FEEDBACK_MAX_IMAGE_BYTES` | Bytes per uploaded or shared image |
| `LENSDOCK_FEEDBACK_MAX_PIXELS` / `LENSDOCK_FEEDBACK_MAX_DIMENSION` | Pixels per image / pixels per side |
| `LENSDOCK_FEEDBACK_MAX_JSON_BYTES` | JSON request body bytes |
| `LENSDOCK_FEEDBACK_DRAFT_TTL_SECONDS` | Draft and unbound-capture lifetime |
| `LENSDOCK_FEEDBACK_MAX_DRAFTS` / `LENSDOCK_FEEDBACK_MAX_OPEN_CAPTURES` | Open drafts / captures per workspace |
| `LENSDOCK_FEEDBACK_MAX_PRIVATE_BYTES` / `LENSDOCK_FEEDBACK_MAX_SHARED_BYTES` | Private / published storage bytes per workspace |
| `LENSDOCK_FEEDBACK_MAX_EFFECT_PIXELS` | Total image-effect pixels per rendered image |

Runtime settings belong in `<stateDir>/runtime.env`: `LENSDOCK_FILE_QUOTA_BYTES`, `LENSDOCK_FILE_MAX_BYTES`, `LENSDOCK_FILE_MAX_FILES`, `LENSDOCK_CAPTURE_MAX_IMAGE_BYTES`, `LENSDOCK_CAPTURE_MAX_IMAGE_PIXELS` and `LENSDOCK_CLIPBOARD_MAX_BYTES`. All default to zero. `LENSDOCK_CHROMIUM_SANDBOX=true` opts in to Chromium's renderer sandbox (default off; see `docker/browser/README.md`). Restart the affected service after changing settings; managed projects use `project stop` and `project up`, not raw Compose lifecycle commands.

CLI feedback delivery and MCP accept `--max-artifact-bytes`, `--max-bundle-bytes` and `--max-json-bytes` (or `LENSDOCK_CLIENT_MAX_ARTIFACT_BYTES`, `LENSDOCK_CLIENT_MAX_BUNDLE_BYTES`, `LENSDOCK_CLIENT_MAX_JSON_BYTES`). MCP also accepts `--max-inline-image-bytes` / `LENSDOCK_MCP_MAX_INLINE_IMAGE_BYTES`. These default to zero; malformed or negative values are errors. Actual RAM, disk and format limits still apply.

User-data transfers have no default overall deadline. Runtime `LENSDOCK_UPLOAD_READ_TIMEOUT` and `LENSDOCK_CLIPBOARD_READ_TIMEOUT` accept Go durations such as `30m`; zero disables the deadline. Gateway `LENSDOCK_RUNTIME_TRANSFER_TIMEOUT_SECONDS` in `product/compose.env` overrides per-runtime JSON `transferTimeoutSeconds`. CLI/MCP `--json-timeout` and `--artifact-timeout` (or `LENSDOCK_CLIENT_JSON_TIMEOUT` and `LENSDOCK_CLIENT_ARTIFACT_TIMEOUT`) also accept Go durations. Connection handshakes, heartbeats and short control synchronization retain their protocol deadlines.

No-expiry drafts use `expires_at = 0`. Disabling expiry clears earlier finite expiry timestamps; enabling it starts the lifetime of previously nonexpiring drafts. An older Gateway interprets zero as already expired: rollback must restore the compatible pre-update database checkpoint, never run old code against the current database merely because its schema number matches.

### Stop and recovery

`project stop --project <root>` keeps persistent volumes and image pins but discards in-memory tabs, forms and network work. Enable manager policy `--require-stop-confirmation` to require the additional `--discard-page-state` flag. Stop atomically enters maintenance only if no lease, capture or handoff is active and the runtime generation is unchanged. A human override uses `admin project stop --project <root> --force --expected-generation <observed-generation>`; it revokes only the observed lease and still refuses a changed generation.

Conditional maintenance rejects null, wrongly typed or inconsistent `ifIdle` and `expectedGeneration` fields before changing any lease. Only an omitted or explicitly false `ifIdle` with no `expectedGeneration` selects unconditional maintenance.

The registry and worker receipts survive manager restarts. A worker interrupted after its durable begun marker without a receipt leaves the project blocked and its reservation retained. Inspect the project's containers and existing updater journals before using `admin project recover --id <project-id> --operation <operation-id>`. Recovery refuses a live worker or a child still holding the lock; it acknowledges the interrupted operation without replaying it or discarding data.

Human `admin project relocate --id <id> <new-root>` changes the authorized directory while retaining the project identity and origin. `admin project revoke --id <id>` removes Agent authorization without stopping the browser. A foreign service on the reserved port causes refusal, never automatic port reassignment.

Reauthorizing a revoked directory must pass the same overlap check as a new grant. An active parent or child grant blocks reinstatement before credentials or routing change.


## Update sequence

| Step | Action | On failure |
|---|---|---|
| 01 | Verify the release manifest, signature or provenance, digests and schema compatibility. Before stopping anything, keep the running images under a `lensdock-keep-<full digest>` tag in their own repository | Stop. Nothing has been stopped or overwritten |
| 02 | Check Profile, database, keyring, attachment quota and backup space | Refuse the update if space is short |
| 03 | Stop the Gateway after draining SSE, VNC and CDP connections and closing the database. Put the workspace in `MAINTENANCE`, refuse new leases and tell users | Stop if the workspace cannot be locked |
| 04 | Drain or explicitly cancel the current control lease and close the CDP and VNC write paths | Do not issue new control |
| 05 | Close Chromium cleanly (`Browser.close`, then signals). Require exit code 0 from Runtime and Gateway, no `Singleton*` lock, `profile.exit_type` of `Normal` or `SessionEnded`, and no unflushed SQLite journal | A snapshot taken otherwise is a crash snapshot and must be labeled as such |
| 06 | Write one consistent encrypted checkpoint of Profile, keyring and Gateway database | Restore the old services, or stay locked for an operator |
| 07 | Create candidate volumes from the checkpoint. Never share writes with the old browser | Delete the candidate, never the original data |
| 08 | Run the Gateway migration on the cloned database in a container with no network, then start the candidate Runtime and run sandbox (when enabled), font, CDP and UTF-8/PNG fixture smoke checks | Do not switch. Keep the results |
| 09 | Keep the candidate off the network. Test only local fixtures | Tests with real accounts need explicit human authorization to avoid duplicate requests |
| 10 | After approval, switch to the new images and candidate volumes as a new generation | Keep the old checkpoint for a paired rollback |
| 11 | Re-check health. Control leases are gone: the UI acquires control again on the next deliberate desktop gesture and an Agent obtains a new lease. Browser sessions are gone after a Gateway restart; local entry resumes automatically, while required-pairing deployments need pairing again | Never resume the last destructive action automatically |

Feedback artifacts live in a retained volume and are not rolled back. After a rollback, the restored database has no feedback created after the checkpoint, so reading it returns 404 while its files remain.

## Rollback rules

A rollback restores the old images, the Profile from before the update, the keyring if needed, and the compatible database checkpoint. Never let an old Chromium open a Profile that a newer release has migrated. The faulty or newer volumes are kept so post-update data can be recovered.

- Show the user the checkpoint time, the state that may be lost, and which artifacts a rollback does not affect. Without a compatible checkpoint, rollback is not guaranteed and the UI must say so. It must not offer a one-click button that looks safe.
- An old Gateway never reads a newer database schema. Rollback restores the older-schema database from the checkpoint and starts the old Gateway on it. A plan is refused when the running database schema is newer than the new release's Gateway schema, unless `--allow-downgrade` is given (still with a paired checkpoint).
- Before a rollback, the CLI keeps both the checkpoint's old images and the currently running images under keep tags. If any is unavailable, nothing is stopped.
- The retained usage policy is not rewound by a compatible rollback. A Runtime release from before usage modes cannot enforce it: do not assume that a saved `NOVNC` mode or a pause protects a workspace running such a Runtime.

## Version and security update policy

Releases pin images by digest and never depend on `latest`. CI may propose Debian security and upstream candidates with package lists and reproducible sources. A major Chromium or Debian upgrade needs a full Profile compatibility and rollback test. An urgent patch still gets the sandbox (when enabled), persistence and handoff smoke checks, and still takes a backup.

| Item | Location | Also do when updating |
|---|---|---|
| Debian base and Go build image digests | `ARG` lines in `docker/browser/Dockerfile` | Change the Go version in `mise.toml` and `go.mod` too. `mise run check:pins` must pass |
| Development tool versions | `mise.toml`, `mise.lock` | `mise install`, `mise run check` |
| Chromium seccomp profile | moby commit and sha256 pinned in `tools/gen_seccomp.py` | `mise run seccomp:generate`, then `mise run e2e:sandbox-matrix` (runs with the sandbox enabled; the custom profile must reach Ready, the Docker default must give `SANDBOX_PREFLIGHT_FAILED`) |
| Installed package versions | written to `/usr/share/lensdock/dpkg-manifest.tsv` at build time | `mise run e2e:version-matrix` to record the matrix |
| Gateway image base and frontend dependencies | `ARG` lines in `docker/gateway/Dockerfile` (Node, Go, Debian digests) and `apps/web/pnpm-lock.yaml` | `mise run web:build`, `mise run gateway:image`. Font and noVNC licenses live in `apps/web/public/licenses/` |

## Retention and expiry of unused old system data

Two independent policies remove old update data, and both yield to protection.

| Setting (`deployment.json`) | Default | Effect |
|---|---|---|
| `expireAfterDays` | `30` | Unused old system data older than this many days expires automatically and in `update cleanup`. An explicit `0` disables age expiry. Valid range 0 to 36500; an absent key means 30 |
| `retainCheckpoints` | `3` | Number of newest clean checkpoints that an explicit `update checkpoints prune` keeps. Minimum 1. It never runs automatically |

`lensdock update expiry` prints the effective policy, the automatic interval and the last real run. `lensdock update expiry --expire-after-days N` stores the age in the deployment file under the deployment lock (for a managed deployment the manager writes it as a typed operation). The next run reads the stored value, so no restart is needed.

### What expires

"Old system data" is only what this deployment's own update logic created and no longer uses:

| Scope | Removes | Age starts at |
|---|---|---|
| `checkpoints` | Complete encrypted checkpoints that no unfinished operation references and that are not the running generation's paired rollback checkpoint | Checkpoint creation |
| `volumes` | Docker volumes that one of this deployment's update journals created or recorded as candidate, scratch or retired, once no running generation or unfinished operation uses them and no container mounts them | The later of the volume's creation label and the retirement or finish of its operation |
| `generations` | `generations/<n>` records below the running generation that hold only `state.json` and `compose.override.yaml` | When the next recorded generation took over |
| `staging` | Incomplete checkpoint directories and saved plan files with no unfinished operation behind them | The creation time in the directory or plan id |

Expiry never touches the running generation or any volume it uses, the checkpoint paired with the running generation, anything that a running, awaiting-approval or held operation references, retained data (downloads, feedback artifacts, private drafts, runtime policy, credentials), images and `lensdock-keep-*` tags, update journals, or volumes it cannot prove this deployment owns. The `io.lensdock.managed` label alone proves nothing, because every deployment on the engine carries it.

Uncertainty keeps data. A missing, malformed or future timestamp, an unreadable generation record, an unrecognized staging name, a checkpoint with unreadable metadata, or a volume whose creating operation has no readable record keeps the item and reports why. Unreadable `state.json` or journals refuse the whole run. If the current state names an update, its producer journal must exist and record the matching generation and a valid paired checkpoint ID; missing or inconsistent references stop both age expiry and count-based `checkpoints prune` before deletion. A pristine generation with no producer remains valid. Only an explicit `checkpoints prune` removes an unprotected malformed checkpoint.

Retired branches hold the only copy of data from a replaced or rolled-back generation. Inspect or recover anything you need from them within the age window, or set `expireAfterDays` to `0` first.

### Count versus age

`retainCheckpoints` and `expireAfterDays` do not read each other. A checkpoint is removed when either policy selects it and never when it is protected:

- Age expiry removes an unprotected checkpoint that is at least `expireAfterDays` old even if it is among the newest `retainCheckpoints`. The count is not a floor for age expiry.
- An explicit `checkpoints prune` removes unprotected clean checkpoints beyond the newest `retainCheckpoints` and every unprotected incomplete leftover, however young. It never runs automatically.
- Protection beats both: the paired rollback checkpoint and every checkpoint an unfinished operation references survive any setting.

CLI age flags distinguish omission from an explicit value: omission uses the saved policy, zero disables age expiry, and every negative value (including `-1`) is rejected before inventory or removal.

### When it runs

| Deployment | Trigger |
|---|---|
| Standalone | After a successful, non-dry-run `update apply` that completes, `update approve`, `update abort` or `update rollback`. Checks, plans, `update compose` and `admin product-start` do not trigger it |
| Managed | Manager housekeeping, once a day per project, only while no operation is queued or running anywhere and the project's `deployment.json` is readable with `expireAfterDays` above 0. A project's first sighting only starts its clock. The task is a typed automatic expire operation that goes through normal admission and the project and helper locks |

Common rules: at most one automatic run per 24 hours, retried at the next opportunity after a failed run; `expireAfterDays` of `0` does nothing; a running update or rollback makes the automatic run skip silently. A standalone problem prints a warning and never changes the command's exit status. Managed housekeeping is invisible in project state. Any other operation for the project cancels a queued housekeeping run. A start request for a running project is answered immediately, while one for a stopped project can get a transient conflict until the run finishes; retry it. A queued run that waits longer than 15 minutes fails and is tried again the next day. Housekeeping needs a running manager. Project-bound commands start it, and no boot service is installed, so nothing expires while it is stopped.

### Preview and explicit cleanup

```bash
lensdock update cleanup --deployment "$DEPLOYMENT" --dry-run
lensdock update cleanup --deployment "$DEPLOYMENT" --scope volumes,staging --older-than-days 7 --dry-run
lensdock update cleanup --deployment "$DEPLOYMENT" --scope checkpoints --yes
```

`--scope` takes a comma-separated subset of the four scopes (default all). `--older-than-days N` replaces `expireAfterDays` for this run only. Because `0` disables expiry, the most aggressive explicit setting is `1`. Without `--dry-run` the command prints the same preview, asks for confirmation (`--yes` skips the prompt), then removes. For a managed deployment the removal runs through the manager as a typed operation.

Each preview or result is a separate JSON line (`dryRun`, `afterDays`, `scopes`, `items`, `scopeErrors` and the counts `expired`, `wouldExpire`, `kept`, `failed`). A real cleanup prints the preview before its final result; managed invocations also report the operation's completion. An item's `outcome` is `would-expire`, `expired`, `kept` (with the reason) or `failed` (with the error). Items that are young and need no attention are not listed. Real runs are recorded in `<stateDir>/state/expiry-last.json`; previews are not. To remove one named volume or the checkpoints beyond the count instead, use `branches delete VOLUME` and `checkpoints prune`.

### Errors and refusals

| Situation | Result |
|---|---|
| An update or rollback holds the deployment lock | An explicit command fails with an operation-in-progress error; the automatic run skips silently |
| State or journals unreadable, or `state.json` missing although completed operations exist | Nothing is inspected or deleted; the error names the cause |
| One scope's inventory fails (backup directory, Docker listing, generation records) | That scope reports under `scopeErrors`. An automatic run still processes the other scopes. An explicit run refuses to remove anything while the preview has an error and items to expire |
| A volume is mounted by any container, even one created after the preview | Kept with `in use by N container(s)` |
| Removal fails | The item is `failed` with its error, the command exits nonzero and the other items continue |
| `--older-than-days 0` or `expireAfterDays` 0 | Nothing is inspected; the report says `disabled` |
| Age outside 0 to 36500, a negative `--older-than-days` or an unknown scope | Refused as an error; the scope error lists the known scopes |

Ordinary file deletion and `docker volume rm` do not promise unrecoverable erasure on every storage medium. Checkpoints are encrypted; keep the backup key apart from them, and never delete it while checkpoints you still need remain. Feedback artifacts and private drafts follow their own limits (see the feedback limits above) and are not part of system expiry.

## Adding the runtime policy volume

The usage mode and Agent pause live in the retained `lensdock-runtime-policy` volume, separate from Chromium's Profile. Candidates use an empty scratch policy in `AGENT` mode, while production keeps its saved policy through the switch.

An installation that predates this volume must add the mount, the dataset declaration and an empty volume before planning an update. Follow [Adding the runtime policy volume](UPDATE-IMPLEMENTATION.md#adding-the-runtime-policy-volume).

## Uninstall and data removal

LensDock keeps its data in several places, and removing the program does not remove any of them. Decide first what you want.

| Goal | Do | Result |
|---|---|---|
| Remove the CLI, keep everything else | [Remove only the CLI](#remove-only-the-cli) | Containers, volumes, state, backups and keys stay. A later install reuses them |
| Stop LensDock and remove its containers, keep all data | Steps 1 to 5 of the [full removal](#full-removal-sequence) (standalone: steps 1 to 3) | Browser Profile, feedback, downloads, state and backups stay and can be started again |
| Remove all LensDock data | The whole [full removal](#full-removal-sequence) | Irreversible loss of the Profile, feedback artifacts, downloads, backups and keys |

What exists and who owns it:

| Data | Standalone location | Managed location (`<manager>` = `${LENSDOCK_MANAGER_DIR:-$HOME/.local/share/lensdock-manager}`) |
|---|---|---|
| Deployment record and state | `<state>/deployment.json`, `<state>/product/` (default `<state>` is `~/.local/share/lensdock`) | `<manager>/projects/<id>/`, same layout |
| Tokens, Runtime certificates, configuration, private release copy, image pins | inside `product/` | inside `projects/<id>/product/` |
| Update state, journals, generations, plans | `product/state`, `updates`, `generations`, `plans` | the same under each project |
| Encrypted checkpoints and their key | `product/backups`, `product/keys/backup.key` unless `backupDir` or `backupKeyFile` point elsewhere | same, per project |
| Manager registry, admin token, locks, logs, grants | none | `<manager>` itself |
| Profile, Gateway database, downloads, feedback, drafts, runtime policy, credentials | Docker volumes | Docker volumes |
| Runtime and Gateway images, `lensdock-keep-*` tags | Docker image store, shared by every deployment on the engine | same |
| CLI | `~/.local/bin/lensdock` link to `~/.local/lib/lensdock/<version>/lensdock` (installer), or the mise install | same |
| Your project folders | never written by removal and never removed by it | same |

Never remove a project's source directory (`root` in `admin project list`): it is yours. A release deployment holds a private copy of its release bundle in `product/release`, so it never depends on where the CLI is installed.

### Remove only the CLI

This keeps the data and reinstalling later reconnects to it. Containers that run keep running. For a quiet system first complete steps 2 to 4 of the [full removal](#full-removal-sequence).

Installer-based install (the default paths; if you set `LENSDOCK_BIN_DIR` or `LENSDOCK_INSTALL_DIR` use the same values):

```bash
lib="${LENSDOCK_INSTALL_DIR:-$HOME/.local/lib/lensdock}"
link="${LENSDOCK_BIN_DIR:-$HOME/.local/bin}/lensdock"
readlink "$link"        # must point below "$lib"; otherwise this link is not the installer's
rm "$link"              # removes only the link
ls -A "$lib"            # one directory per installed version, plus leftover .stage.* if an install was interrupted
rm -rf -- "$lib/EXACT_VERSION_FROM_THE_LIST"
rmdir "$lib"            # removes the directory only if it is empty
```

Replace `EXACT_VERSION_FROM_THE_LIST` with a LensDock version directory you inspected. Inspect interrupted `.stage.*` directories individually and remove only ones whose contents belong to this installer; do not delete them with a wildcard in a shared directory. `LENSDOCK_BIN_DIR` and `LENSDOCK_INSTALL_DIR` can be shared with other tools, so never `rm -rf` the override directory itself, `~/.local/bin` or `~/.local/lib`.

mise install (`mise use -g 'github:JiaWeiXie/lensdock[bin_path=.]@latest'`), checking the tool name that `mise ls --global` prints:

```bash
mise unuse --global --no-prune github:JiaWeiXie/lensdock    # edits the config only; keeps the installation
mise uninstall --all github:JiaWeiXie/lensdock --dry-run    # shows exactly what would be removed
mise uninstall --all github:JiaWeiXie/lensdock              # removes only this tool's installed versions
```

Also remove the `lensdock` MCP server entry from every Agent client you configured (for example `claude mcp remove lensdock --scope user`, the `[mcp_servers.lensdock]` table of Codex, or the OMP `mcp.json` entry); see [Agent integration](AGENT-INTEGRATION.md).

To reinstall, run the installer again (`--install-only` skips initialization) or `mise use`. An existing deployment is never changed by the installer; start it with `lensdock admin product-start --state-dir <state>`. Managed projects start again with `lensdock project up --project <root>`, which restarts the manager from its own directory. Keep the state directory and the volumes together (see [Reinstall compatibility](#reinstall-compatibility)).

### Full removal sequence

Run every step in order. Use the deployment's `deployment.json`, not a port or a name, to identify what you are removing, because other LensDock deployments and unrelated Docker resources can share the engine. Do not run a global `docker system prune` or `docker volume prune`, never use `docker volume rm --force`, and do not remove images that another deployment still pins.

Set the variables for one deployment. Standalone:

```bash
DEPLOYMENT="$HOME/.local/share/lensdock/deployment.json"   # or the file you pass to --deployment
```

Managed (repeat for each project that `admin project list --json` reports):

```bash
lensdock admin project list --json | jq -r '.projects[] | [.id, .root, .stateDir, .composeProject, .state] | @tsv'
DEPLOYMENT="<stateDir from the list>/deployment.json"
```

**1. Inventory, read-only, before anything is stopped.**

```bash
jq '{stateDir, project: .compose.project, backupDir, backupKeyFile, releasesDir, managedBy}' "$DEPLOYMENT"
```

Record these values. The default `backupDir` is `<stateDir>/backups` and the default `backupKeyFile` is `<stateDir>/keys/backup.key`, where `<stateDir>` is the deployment's `stateDir` (`<state>/product`). A `backupDir`, `backupKeyFile` or `releasesDir` outside `stateDir` is not removed with it; you decide its fate in step 7. Relative paths resolve against the deployment file's directory.

Then collect the exact Docker volumes of this deployment (needs `jq`). No single source is complete, because the update logic clones volumes under new names that Compose labels do not describe, so write the union of these four to `volumes.txt`:

```bash
STATE="$(jq -r .stateDir "$DEPLOYMENT")"
PROJECT="$(jq -r .compose.project "$DEPLOYMENT")"
(
  set -euo pipefail
  {
  # a. Volumes of the running generation; a new deployment may have no state.json.
  if test -e "$STATE/state/state.json"; then
    jq -r '.components[].volumes[]' "$STATE/state/state.json"
  fi
  # b. Compose-created volumes of the project: the first-start volumes
  docker volume ls --filter "label=com.docker.compose.project=$PROJECT" --format '{{.Name}}'
  # c. Branches of this deployment: retired generations, failed candidates, scratch volumes
  lensdock update branches list --deployment "$DEPLOYMENT" | jq -r '.branches[].volume'
  # d. Volumes mounted by this project's containers (run before they are removed)
  for c in $(docker ps -a -q --filter "label=com.docker.compose.project=$PROJECT"); do
    docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}{{"\n"}}{{end}}{{end}}' "$c"
  done
  } | sort -u > volumes.txt
  cat volumes.txt
)
```

Do not continue if any inventory command fails. Use the resolved absolute `stateDir` and Compose project recorded in step 1; if the JSON omits them or stores relative paths, resolve those defaults before running this block. Review the list by eye. The profile, `gateway-db`, downloads, runtime-policy, `gateway-private`, `gateway-artifacts` and both credential volumes should be present, once per generation that still exists.

Never add volumes by prefix, by the shared `io.lensdock.managed` label alone or by a partial name: every deployment on the engine carries that label. `update branches list` already limits itself to volumes that this deployment's journals or running generation account for. For a managed project the Docker commands must reach the engine the manager pinned; compare `docker info --format '{{.ID}}'` with `engineId` in `<manager>/config.json` and use the `dockerContext` or `dockerHost` recorded there when they differ.

Also run `lensdock update checkpoints list --deployment "$DEPLOYMENT"` to see the encrypted checkpoints you are about to lose, and note any custom state directory you passed with `--state-dir`.

**2. Stop everything that can reach or restart a workspace.** End Agent client sessions or remove their MCP entries, stop `lensdock mcp serve`, `lensdock workspace watch` and `lensdock cdp bridge` processes, and release any lease files you hold (`lensdock lease release --lease-file <file>`). Project-bound commands enroll and start a project automatically, so a stray process can restart one while you work. Finish any running update first (`lensdock update status`); do not remove data during `awaiting-approval` or `failed-held` operations.

**3. Stop the projects.** Stopping closes Chromium through its normal shutdown and discards open tabs, forms and network work.

```bash
lensdock project stop --project <root>                    # add --discard-page-state when the manager policy requires it
lensdock project status --project <root>
```

If a human holds a lease or capture, stop is refused; end it in the UI, or use the Host Admin override `lensdock admin project stop --project <root> --force --expected-generation <generation>`. An interrupted operation blocks removal until `admin project recover` or an explicit abort. Optionally run `lensdock admin project revoke --id <id>` for each project: automatic enrollment never revives a revoked directory, so no later command restarts it. Revoking does not delete data.

For a standalone deployment use instead:

```bash
lensdock update compose --deployment "$DEPLOYMENT" -- down    # removes containers and networks; -v, --volumes and --rmi are refused
```

**4. Stop the manager.** Only when you have no managed project left to keep running:

```bash
lensdock admin manager stop
```

It stops only the manager daemon. It waits until its handlers finish and the singleton lock is released, refuses while a worker or a lifecycle helper is active (wait and retry), and neither stops containers nor deletes anything. Any later `project`, `workspace` or MCP command with `--project` starts the manager again, so from here on issue no project-bound commands. If `LENSDOCK_MANAGER_DIR` is set, use the same value for every command.

**5. Remove containers and networks of managed projects.** The manager stops containers but leaves them and their networks in place. For each managed project (`$PROJECT` is `composeProject`, `$ID` is `id`):

```bash
docker ps -a --filter "label=com.docker.compose.project=$PROJECT" --filter "label=io.lensdock.project-id=$ID" --format '{{.ID}} {{.Names}} {{.State}}'
docker rm <container id> ...            # only the listed ids; a running container is refused
docker network ls --filter "label=com.docker.compose.project=$PROJECT" --format '{{.ID}} {{.Name}}'
docker network rm <network id> ...      # refused while a container is attached
```

If `docker ps` shows a container that does not carry both labels, it is not LensDock's: leave it. A project whose `docker ps` listing is empty needs nothing here. A standalone deployment was cleaned by `down` in step 3.

**6. Remove the volumes.** This is irreversible: it deletes the Profile, logins, Gateway database, feedback, downloads and credentials. Compare `volumes.txt` with `docker volume ls` once more, then:

```bash
docker volume inspect $(cat volumes.txt)       # every name must exist; stop if a name is unfamiliar
for v in $(cat volumes.txt); do docker volume rm "$v"; done
```

`docker volume rm` refuses a volume that a container still uses; resolve that container rather than forcing. A volume owned only by another deployment must never be removed. `inUse` in `branches list` means referenced by the current generation, even after stopping; permanent removal requires the full stopped-container and backup review above. To remove only retired and failed-update volumes while keeping live data, use `update cleanup --scope volumes` or `update branches delete VOLUME` instead (see [Retention and expiry](#retention-and-expiry-of-unused-old-system-data)).

Images are shared. Remove the Runtime and Gateway images only when no LensDock deployment on this engine remains: list them with `docker image ls --filter 'reference=ghcr.io/jiaweixie/lensdock/*'` (and any local `lensdock/*:dev` build), untag the `lensdock-keep-*` tags and remove the images by explicit reference (`docker image rm <repository:tag>`). Docker refuses an image that a container uses, and a deployment that pins `repository@digest` fails to start if you remove its image. LensDock's own commands never remove images.

**7. Remove the state directories.** Only after steps 3 to 6 and only if you want to lose the deployment record, tokens, certificates, state, backups and keys.

```bash
rm -rf -- "<state>"                    # standalone: the directory that held deployment.json
rm -rf -- "<manager>"                  # managed: the whole manager directory, all projects at once
```

Check the path before running: it must be the directory that you recorded in step 1 and must contain `deployment.json` (standalone) or `config.json` and `registry.json` (manager). Never use a parent such as `~/.local/share`. Managed projects have no per-project deletion command; removing a single `projects/<id>` directory by hand from a manager that remains in use is not supported, so either remove the whole manager directory or keep the project revoked and stopped.

A `backupDir`, `backupKeyFile` or `releasesDir` that you recorded outside the state directory is still on disk. Delete it separately only when you no longer need those checkpoints. A checkpoint is encrypted with its key, so deleting the key makes every remaining checkpoint permanently unreadable and deleting the checkpoints leaves a useless key; remove them together unless you are keeping both.

**8. Remove the CLI and client configuration.** Follow [Remove only the CLI](#remove-only-the-cli). Do it last, because steps 2 to 4 need the binary.

**9. Verify.** Each of these should report nothing LensDock-related:

```bash
docker ps -a --filter "label=com.docker.compose.project=$PROJECT"
docker volume inspect $(cat volumes.txt)       # every name should be "No such volume"
test ! -e "<state or manager directory>" && echo removed
command -v lensdock
```

### Reinstall compatibility

- To keep data for a reinstall, keep the state directory (`deployment.json`, `product/`, tokens, certificates, `images.pin.yaml`, `state`, `updates`, `generations`, `keys`, `backups`) together with the Docker volumes. The credential volumes were written from `product/`, and checkpoints need the key. Never delete `product/` to force a fresh initialization: the deployment would no longer match its credential volumes.
- If you delete the volumes but keep the state directory, the deployment record describes volumes that no longer exist. Remove or move the whole state directory before initializing again.
- If you delete the state directory but keep the volumes, their credentials no longer match any deployment. Treat those volumes as orphans and remove them, or restore the state directory from a backup of your own.
- Keep a private copy of `backupKeyFile` together with any `backupDir` you want to restore later. Never store the key next to the encrypted checkpoints.
- Removal is irreversible. LensDock offers no undo, and the Docker engine's volume deletion is not a secure erase.

## Failure cases and verification status

Every failure case needs a defined state, no repeated side effects and a recovery step. The first four rows record earlier update and rollback runs. The dated rows record the current acceptance runs; unexercised cases remain explicit. These results do not certify later changes.

| Case | Status |
|---|---|
| Candidate Runtime never becomes ready (entrypoint `/bin/false`) while a VNC session and an SSE stream are open | Exercised. The previous healthy release and its database were restored, published feedback was unchanged, the browser reconnected |
| Gateway schema upgrade 1 to 2, then paired rollback to schema 1 | Exercised. The old Gateway ran only on the restored schema 1 database |
| Profile markers and retained artifacts across an update and a paired rollback | Exercised. Pre-update markers were restored, post-update markers were absent, artifact hashes were unchanged |
| Update while the Agent is paused | Exercised. The candidate used its own scratch policy and production stayed paused with the same revision |
| Chromium unclean exit, incomplete backup, sandbox or font failure in the candidate, incompatible Profile | Guards exist (exit code, lock, `profile.exit_type` and journal checks, decrypt and tar walk before a checkpoint is published, candidate probe). Not injected |
| Disk full, interrupted download | The plan checks free space for the checkpoint and candidates. Images are local, so there is no pull step. Not injected |
| Failed Gateway migration on the clone | Not injected. The successful migration was exercised |
| Host restart during an update | Journals persist and `abort` rebuilds the run from them. Not exercised |
| Candidate network leak | The probe's egress check and the internal-network check passed for the real candidates. A leak was not injected |
| Old Gateway reading a newer schema | Prevented by the plan and by rollback ordering. Not run on purpose |
| Automatic control and feedback states | Exercised on 2026-10-26. A desktop gesture acquired control and fenced the Agent's old CDP connection with 4001. The acquisition gesture was not replayed; human control renewed beyond its 30-second TTL. Release blocked stale keyboard reacquisition. Pause refused new Agent commands; resume left the workspace idle. Published feedback stayed Sent after ACK, then moved through In progress, Done awaiting verification and explicit human verification |
| Full native Agent control | Exercised on 2026-10-26 with headful Chromium 156.0.8078.47. Native address-bar entry, menu, drag, scroll and an unpacked extension popup worked under the same lease as CDP. UTF-8 and long ASCII input were checked against extension storage. `Browser.close` restarted Chromium with a new generation; a persistent test cookie survived in the dedicated Profile |
| Expiry and cleanup | Exercised on 2026-10-26 in isolated deployments. Real Docker volumes and age-stamped fixture state covered preview, policy `0`, deletion and protection across all four scopes. Current and container-held volumes, paired checkpoints, young data and foreign ownership were retained. Managed policy writes and scheduled cleanup ran through the real daemon and helper. The scheduler timestamp was aged while the daemon was stopped; this was not an unattended 24-hour soak |
| Installer uninstall and reinstall | Exercised on 2026-10-26 with a locally packaged release in private paths. Removing the exact CLI link and version kept running containers, all eight volumes and deployment state. Reinstalling left the deployment byte-for-byte unchanged. The mise commands were checked against its documentation, not run against the user's global installation |
| Full standalone and managed removal | Exercised on 2026-10-26. Inventory found each deployment's eight volumes. Standalone `compose down` retained volumes until explicit deletion. Managed stop, revoke and manager shutdown preceded removal of the exact stopped containers, networks, volumes and manager state. The project source marker survived. Manager shutdown was also idempotent and left running project containers untouched when used alone |

Limits of everything above:

- Runs used unsigned development releases (no BuildKit provenance, no scanner output). Browser and runtime end-to-end runs covered macOS arm64 with OrbStack only. There is no integrated amd64 run and no run of the signed release channel. Native release smoke checks do not establish full product E2E on a platform.
- Managed projects were exercised on that platform with the manager's admission, helper and recovery paths, including real apply, approval, abort and paired rollback. A Chromium-version upgrade and an OS reboot were not.
- No real account was used. Profile persistence was proven with `tests/fixtures/handoff.html` markers.
- No exercised release changed the Chromium version, so a rollback after a Chromium upgrade has not been demonstrated, although the Profile is restored from the checkpoint either way.
- Runtime mutual-TLS certificates written by `product-init` expire after 365 days. No rotation procedure is documented or exercised.
- Gateway browser sessions are process-local. Required-pairing deployments need pairing again after a restart; default local access resumes automatically.
- Do not overwrite a local image tag that a deployment pinned by digest. The old digest then stops resolving locally and a restart fails at `config-init`. Use a unique tag for each local build, and rely on the `lensdock-keep-*` tags for retained generations.
- Measured memory and capacity numbers from earlier synthetic workloads are not a capacity guarantee. Default managed projects set no CPU, memory or process caps; see [Resource policy](#resource-policy).
