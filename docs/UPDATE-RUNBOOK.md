# Update and rollback runbook

Host updates are implemented in the `lensdock update` CLI. This runbook states the policy, the order of operations and what has actually been verified. Exact flags, deployment file fields and recovery procedures are in [UPDATE-IMPLEMENTATION.md](UPDATE-IMPLEMENTATION.md).

## Responsibilities

- Browser and Debian userspace change through a new Runtime image.
- Gateway, noVNC and the UI change through a new Gateway (app) image.
- The host kernel and Docker Engine belong to the host administrator.
- The Runtime and Gateway containers never mount the Docker socket. Only the host CLI applies or rolls back an update, and only with Host Admin authority. An Agent token cannot call it.
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
lensdock update compose -- <up|down|stop|start|restart|ps|logs|config> ...
```

Every command accepts `--deployment <file>`.

- `check` only reports. `plan` verifies image digests, signature, compatibility, disk space, backup location and maintenance impact without touching the browser.
- Keep each installed release directory at `<releasesDir>/<releaseId>`. Later plans need the running release's manifest there to verify Chromium and schema compatibility, even when the original plan used an external release directory.
- `apply` asks for confirmation. Commands and logs never print tokens, Profile content or secrets.
- The Gateway and UI have no apply or rollback endpoint. They read only `deployment-status.json`, which holds release, update and checkpoint summaries.

## Managed project lifecycle

The host manager owns deployments whose `deployment.json` has `managedBy` metadata. `admin project list --json` reports each project's ID, root, `stateDir`, origin, configured limits, reservation, measured usage and queue/blocker state. Its deployment file is `<stateDir>/deployment.json`. Supply that exact file to `update --deployment`; do not point at `<stateDir>/product/deployment.json`.

For these deployments:

- `project up --project <root>` handles admission and first initialization. `admin product-start --state-dir <stateDir>` also routes through admission; it cannot change the pull policy.
- `check` and `plan` default to the registered project Workspace when `--workspace` is omitted. An explicit Workspace must still belong to the deployment; standalone commands keep their existing default.
- Raw `update compose` permits only `ps`, `logs` and `config`. Use `project up` or `project stop --discard-page-state` for lifecycle changes. Managed initialization and direct status-file publication are refused.
- Apply, approve, abort, rollback, checkpoint verification/pruning and branch deletion use typed human-authorized manager operations. Read-only checks use the same fixed Docker engine and bounded helper slot. Missing or mismatched manager ownership fails closed.
- A project holds one lifecycle lock. Workers also share a helper lock, so lifecycle operations are serialized while running project pairs remain independent. Child Docker processes inherit the locks. Helpers have explicit memory, swap and CPU caps; candidates inherit the project's resource profile.
- Apply and rollback reserve a candidate pair before maintenance. Approve and abort reserve at least one project pair before restarting stopped services. A full queue cannot consume the helper allowance needed for an explicit stop. Admission rechecks waiter expiry and operation deadlines after Docker inventory returns. A canceled or disconnected CLI does not cancel admitted work.
- Image pins, generation records, encrypted checkpoints and paired Profile/keyring/database rollback still follow the existing updater. The manager does not make an image-only downgrade safe.

Without `--approve-switch`, a successful candidate verification leaves `apply` waiting for approval with a nonzero CLI exit status. Services remain stopped until approve or abort completes. Printed status, approve and abort commands retain the exact, shell-quoted `--deployment` selection.

The default root is `~/.local/share/lensdock-manager`, selected independently of standalone `LENSDOCK_DEPLOYMENT`. Initialization snapshots the host executable and release bundle and pins a local Unix-socket Docker endpoint and engine ID. Later caller `DOCKER_*`, `COMPOSE_*` or deployment environment overrides cannot retarget workers.

### Resource policy

Defaults reserve 6.75 GiB / 7.5 CPUs for managed projects, 1 GiB / 1 CPU for lifecycle helpers and 2 GiB / 1 CPU for VM/system headroom. Each newly authorized project gets a 2 GiB / 2 CPU Runtime and a 256 MiB / 0.5 CPU Gateway. The engine's physical capacity and external container caps also constrain admission; the configured project budget is not a promise that three pairs fit on every host.

Humans change policy with `lensdock admin manager policy`; `--memory-mib` and `--cpus` set the aggregate project budget, and `--runtime-memory-mib`, `--runtime-cpus`, `--gateway-memory-mib`, `--gateway-cpus` set limits for future authorizations. Existing project limits stay fixed. Policy changes never shrink or evict a running project.

`--helper-memory-mib` changes the shared helper allowance without changing existing Runtime/Gateway limits or invalidating their resource profiles. Helper policy changes are refused while the helper lock is held. Workers load the current allowance after acquiring that lock.

Uncapped external containers block admission unless a human explicitly supplies `--allow-unbounded-external`, `--unbounded-external-memory-mib` and `--unbounded-external-cpus`. This allowance is accounting only: external containers can exceed it. Stop or cap those workloads when a hard resource guarantee is required. An impossible request fails rather than waiting indefinitely; feasible requests wait FIFO. Measured usage can be unavailable and must not be read as zero.

### Stop and recovery

`project stop --project <root> --discard-page-state` keeps persistent volumes and image pins but discards in-memory tabs, forms and network work. It atomically enters maintenance only if no lease, capture or handoff is active and the runtime generation is unchanged. A human override uses `admin project stop --project <root> --discard-page-state --force --expected-generation <observed-generation>`; it revokes only the observed lease and still refuses a changed generation.

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
| 08 | Run the Gateway migration on the cloned database in a container with no network, then start the candidate Runtime and run sandbox, font, CDP and UTF-8/PNG fixture smoke checks | Do not switch. Keep the results |
| 09 | Keep the candidate off the network. Test only local fixtures | Tests with real accounts need explicit human authorization to avoid duplicate requests |
| 10 | After approval, switch to the new images and candidate volumes as a new generation | Keep the old checkpoint for a paired rollback |
| 11 | Re-check health. Humans and Agents request new leases. Browser sessions in the Gateway are gone after a restart and need pairing again | Never resume the last destructive action automatically |

Feedback artifacts live in a retained volume and are not rolled back. After a rollback, the restored database has no feedback created after the checkpoint, so reading it returns 404 while its files remain.

## Rollback rules

A rollback restores the old images, the Profile from before the update, the keyring if needed, and the compatible database checkpoint. Never let an old Chromium open a Profile that a newer release has migrated. The faulty or newer volumes are kept so post-update data can be recovered.

- Show the user the checkpoint time, the state that may be lost, and which artifacts a rollback does not affect. Without a compatible checkpoint, rollback is not guaranteed and the UI must say so. It must not offer a one-click button that looks safe.
- An old Gateway never reads a newer database schema. Rollback restores the older-schema database from the checkpoint and starts the old Gateway on it. A plan is refused when the running database schema is newer than the new release's Gateway schema, unless `--allow-downgrade` is given (still with a paired checkpoint).
- Before a rollback, the CLI keeps both the checkpoint's old images and the currently running images under keep tags. If any is unavailable, nothing is stopped.
- The retained usage policy is not rewound by a compatible rollback. A Runtime release from before usage modes cannot enforce it: do not assume that a saved `NOVNC` mode or a pause protects a workspace running such a Runtime.

## Version and security update policy

Releases pin images by digest and never depend on `latest`. CI may propose Debian security and upstream candidates with package lists and reproducible sources. A major Chromium or Debian upgrade needs a full Profile compatibility and rollback test. An urgent patch still gets the sandbox, persistence and handoff smoke checks, and still takes a backup.

| Item | Location | Also do when updating |
|---|---|---|
| Debian base and Go build image digests | `ARG` lines in `docker/browser/Dockerfile` | Change the Go version in `mise.toml` and `go.mod` too. `mise run check:pins` must pass |
| Development tool versions | `mise.toml`, `mise.lock` | `mise install`, `mise run check` |
| Chromium seccomp profile | moby commit and sha256 pinned in `tools/gen_seccomp.py` | `mise run seccomp:generate`, then `mise run e2e:sandbox-matrix` (the custom profile must reach Ready, the Docker default must give `SANDBOX_PREFLIGHT_FAILED`) |
| Installed package versions | written to `/usr/share/lensdock/dpkg-manifest.tsv` at build time | `mise run e2e:version-matrix` to record the matrix |
| Gateway image base and frontend dependencies | `ARG` lines in `docker/gateway/Dockerfile` (Node, Go, Debian digests) and `apps/web/pnpm-lock.yaml` | `mise run web:build`, `mise run gateway:image`. Font and noVNC licenses live in `apps/web/public/licenses/` |

The deployment file keeps the latest three verified checkpoints by default (`retainCheckpoints`) and sets a minimum free space (`minFreeBytes`). There is no time-based expiry. `update checkpoints prune` removes only checkpoints that no active update or rollback references. Encrypt sensitive backups and keep the key apart from them. Retention expiry must cover private screenshots and their backups. Ordinary file deletion does not promise unrecoverable erasure on every storage medium.

## Adding the runtime policy volume

The usage mode and Agent pause live in the retained `lensdock-runtime-policy` volume, separate from Chromium's Profile. Candidates use an empty scratch policy in `AGENT` mode, while production keeps its saved policy through the switch.

An installation that predates this volume must add the mount, the dataset declaration and an empty volume before planning an update. Follow [Adding the runtime policy volume](UPDATE-IMPLEMENTATION.md#adding-the-runtime-policy-volume).

## Failure cases and verification status

Every failure case needs a defined state, no repeated side effects and a recovery step. Only the first four rows below were run against a real deployment. The rest have implemented checks or guards, but no injected failure has been recorded, so do not read them as verified.

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

Limits of everything above:

- Runs used unsigned development releases (no BuildKit provenance, no scanner output) on one macOS arm64 host with OrbStack. There is no integrated amd64 run and no run of the signed release channel. Native release smoke checks do not establish full product E2E on a platform.
- No real account was used. Profile persistence was proven with `tests/fixtures/handoff.html` markers.
- No exercised release changed the Chromium version, so a rollback after a Chromium upgrade has not been demonstrated, although the Profile is restored from the checkpoint either way.
- Runtime mutual-TLS certificates written by `product-init` expire after 365 days. No rotation procedure is documented or exercised.
- Gateway browser sessions are process-local, so every Gateway restart requires pairing again.

## Managed-project qualification

On 2026-10-26, a disposable manager ran three fresh arm64 Runtime/Gateway pairs on macOS with OrbStack. Four simultaneous requests for A/A/B/C converged to three deployments. Each had its own persistent cookie and published screenshot feedback; real CLI/MCP routing and same-browser pairing/logout were exercised. Separate native Gateway processes covered same-browser demo sign-out isolation without a live Runtime.

The workload used a 20 Hz 1280×720 canvas and a retained 32 MiB byte array in each headful Chromium, followed by concurrent viewport captures, preview uploads and feedback publication. Docker stats samples below combine each Runtime and Gateway:

| Project | Low-load memory, MiB | Highest workload sample, MiB | Enforced pair cap, MiB |
|---|---:|---:|---:|
| A | 318.3 | 374.4 | 2304 |
| B | 282.1 | 358.9 | 2304 |
| C | 284.4 | 361.5 | 2304 |

No OOM was observed. Live inspection also verified eight helper containers at 512 MiB memory/swap and 1 CPU, within the global helper allowance. These are sampled synthetic workloads, not an exhaustive peak or a capacity guarantee for arbitrary sites.

The same run exercised FIFO admission, individual waiter cancellation/expiry, impossible demand, foreign-port refusal, directory replacement, real Git worktrees, caller environment overrides, and explicit stop/restart with persistent Profile/DB fixtures. Pages continued background work for more than 103 seconds with no lease. Killing the CLI or manager preserved admitted work; killing a begun worker blocked replay until human recovery. Concurrent start/stop/update requests did not overlap. Existing standalone port 18081 retained its container/image IDs and remained healthy.

The initial run exercised managed update check and typed checkpoint prune. Candidate admission at full capacity expired before maintenance; an admitted dry-run rejected a missing plan without stopping services. Two existing uncapped external services were left untouched; the user approved an aggregate 512 MiB / 1 CPU allowance for the disposable managers only.

A separate review-fix run exercised real managed apply, approval, abort and paired Profile/database rollback. The development images had distinct identities but the same Chromium version and Gateway schema:

- With only the original apply-result bug restored, the CLI reported success while the updater journal said `awaiting-approval`. The fixed manager returned the waiting-for-approval outcome and deployment-specific recovery commands.
- Approve and abort each waited with zero running project containers while two other pairs filled the budget. Explicitly stopping one pair admitted the waiting operation. The emitted abort command also selected the correct managed deployment despite an unrelated standalone deployment setting.
- A pre-update cookie survived approval. Abort preserved both pre-update and post-update cookies; paired rollback restored only the pre-update cookie. Both encrypted checkpoints were marked clean and included Profile and database datasets.
- Helper memory changed from 1 GiB to 1.5 GiB and back without invalidating the project's resource profile. A policy change during an actual update was refused. Runtime/Gateway memory, swap and CPU caps remained fixed.
- Real CLI reinstatement refused overlapping parent and child grants in both directions, preserving the active grant's credential and leaving the revoked credential absent.

The regression suite also covers expiry during blocked inventory, physical-engine admission for approve/abort, malformed conditional-maintenance requests with an active lease, managed Workspace defaults and shell-safe recovery target selection. Native and Linux `go vet`, Linux E2E-tag vet, and `go test -race -count=1 ./...` passed after all eight review fixes: 27 tested packages and one package without tests.

These runs did not cover a Chromium-version upgrade, an OS reboot, signed-release qualification or amd64 Docker E2E. After explicit stops, all temporary containers, volumes, networks, private fixtures and test image tags were removed. The existing standalone deployment on port 18081 kept the same container/image IDs and remained healthy. No real-account profile or raw screenshot was committed.
