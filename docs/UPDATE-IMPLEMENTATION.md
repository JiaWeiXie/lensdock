# Host update implementation

Command-level procedure for the policy in [UPDATE-RUNBOOK.md](UPDATE-RUNBOOK.md). Everything runs on the host. Runtime, Gateway and browser containers never mount the Docker socket. The commands need Host Admin authority: an authorized human, or a host Agent that a human lets run host `lensdock` commands. A browser or container Agent token never carries it. Removal and uninstall are in [Uninstall and data removal](UPDATE-RUNBOOK.md#uninstall-and-data-removal).

Examples call `lensdock` from `PATH` and read the deployment file from `LENSDOCK_DEPLOYMENT`:

```bash
export LENSDOCK_DEPLOYMENT="$HOME/.local/share/lensdock/deployment.json"
```

In a source checkout, run `mise run build` and substitute `bin/lensdock`; a checkout initialized with the default state directory uses `.lensdock/deployment.json`.

Releases built from a working tree are development releases: unsigned, with no BuildKit provenance and no scanner output. The signed release channel is described under [Release evidence](#release-evidence). Verification status and limits are in the [runbook](UPDATE-RUNBOOK.md#failure-cases-and-verification-status).

## Deployment contract

`lensdock update` reads `--deployment FILE`, then `$LENSDOCK_DEPLOYMENT`, then `<state-dir>/deployment.json`. Relative paths resolve against the file's directory, and unknown fields are errors. `docker compose` always runs with the configured project, base files, `envFiles`, project directory and the current generation override, so a product's custom state directory, port, origin and workspace are preserved.

Product initialization writes this file once per state directory and refuses to overwrite an existing `product/` directory. Do not rerun it on a managed deployment and do not delete `product/` to force it: that directory holds the pairing tokens, the Runtime certificates and the Gateway and Runtime configuration. The shape it writes:

```json
{
  "version": 1,
  "stateDir": "/absolute/state/product",
  "publicStatusFile": "/absolute/state/product/public/deployment-status.json",
  "compose": {
    "project": "lensdock-product",
    "files": ["/repo/docker/compose.yaml"],
    "envFiles": ["/absolute/state/product/compose.env"]
  },
  "linuxCli": {
    "amd64": "/repo/bin/linux-amd64/lensdock",
    "arm64": "/repo/bin/linux-arm64/lensdock"
  },
  "workspaces": {
    "default": {
      "runtimeService": "runtime",
      "operatorTokenFile": "/absolute/state/product/operator-token",
      "probe": {
        "url": "https://127.0.0.1:8080",
        "caFile": "/absolute/state/product/ca.crt",
        "certFile": "/absolute/state/product/gateway.crt",
        "keyFile": "/absolute/state/product/gateway.key"
      },
      "datasets": [
        {"key": "lensdock-profile", "kind": "profile", "role": "snapshot", "mountPath": "/data/profile"},
        {"key": "lensdock-downloads", "kind": "downloads", "role": "retained", "mountPath": "/data/downloads"},
        {"key": "lensdock-runtime-policy", "kind": "other", "role": "retained", "candidateAccess": "scratch", "mountPath": "/data/runtime"},
        {"key": "runtime-credentials", "kind": "other", "role": "retained", "mountPath": "/run/lensdock-credentials"}
      ]
    }
  },
  "gateway": {
    "service": "gateway",
    "datasets": [
      {"key": "gateway-db", "kind": "db", "role": "snapshot", "mountPath": "/data/db"},
      {"key": "gateway-private", "kind": "private", "role": "retained", "mountPath": "/data/private"},
      {"key": "gateway-artifacts", "kind": "artifacts", "role": "retained", "mountPath": "/data/artifacts"},
      {"key": "gateway-credentials", "kind": "other", "role": "retained", "mountPath": "/run/lensdock"}
    ],
    "migrateArgv": ["migrate", "--data-dir", "/data"],
    "readyUrl": "http://127.0.0.1:18081/readyz"
  }
}
```

Optional keys and their defaults:

| Key | Default |
|---|---|
| `backupDir` | `<stateDir>/backups` |
| `releasesDir` | `<stateDir>/releases` |
| `backupKeyFile` | `<stateDir>/keys/backup.key` (private 0600 file outside `backupDir`) |
| `retainCheckpoints` | 3 (newest clean checkpoints an explicit `checkpoints prune` keeps; at least 1) |
| `expireAfterDays` | 30 (age of unused old system data for expiry; an explicit `0` disables it; 0 to 36500; set with `update expiry --expire-after-days N`) |
| `minFreeBytes` | 512 MiB |
| `stopGraceSeconds` | 40 |
| `drainTimeoutSeconds` | 45 |
| `healthTimeoutSeconds` | 180 |
| `gateway.user` | `10000:10000` (the migration container) |
| `trustedKeys` | none; required to admit a `release` channel manifest |

Each dataset has a role:

- `snapshot` volumes are encrypted into every checkpoint, cloned for the candidate and restored as a pair on rollback. They never set `candidateAccess`.
- `retained` volumes are never copied or rolled back. The new generation reattaches them. `candidateAccess` decides what verification containers see: `scratch` is a new empty volume, `readonly` is the real volume mounted read-only. The defaults are `scratch` for `downloads` and `private`, and `readonly` for `artifacts` and `other`.

`candidateAccess` describes the candidate only. The live Gateway mounts `gateway-artifacts` writable because it stores published feedback there, while Gateway and Runtime credentials are mounted read-only. The Gateway migration container mounts every Gateway dataset according to `candidateAccess`, so the writable live artifacts mount does not block a read-only migration. The candidate Runtime inherits the Runtime service's compose mounts: any dataset shared read-only with it, such as the Runtime credentials, must be mounted `:ro` in compose, or the plan reports a blocker.

The setup-only `config-init` service is not owned by the updater and is never started by candidate verification. It writes the credential volumes during the one-time bootstrap.

The profile and Gateway database must be named volumes, never bind mounts. Downloads, feedback artifacts, private drafts and the runtime usage policy are not rolled back. Runtime and Gateway credentials are retained configuration, not backup content. The backup key is a private 0600 file outside the backup directory. The public status directory is a read-only bind mount into the Gateway and is not a dataset.

Initialize the backup key without replacing an existing one:

```bash
lensdock update init --deployment "$LENSDOCK_DEPLOYMENT"
```

For the standalone Runtime compose only, `update init` can also create the runtime, profile, downloads and runtime-policy scaffold. Product initialization writes its full deployment JSON before this command runs.

### Adding the runtime policy volume

New installations declare `lensdock-runtime-policy:/data/runtime`. The Runtime stores `agent-control.json` there, with mode `0600` in a `0700` directory owned by UID/GID `10000:10000`. Chromium runs as UID `10001` and cannot read or change the policy. Never put it in the browser-owned Profile or Downloads volume.

For an existing installation, keep its current generation and data:

1. Add the runtime-policy mount and named volume declaration from the current Compose file. Add the `lensdock-runtime-policy` dataset shown above to the workspace in the deployment JSON, including `"candidateAccess": "scratch"`.
2. Create the new empty volume before planning. For the default product project:

   ```bash
   docker volume create \
     --label com.docker.compose.project=lensdock-product \
     --label com.docker.compose.volume=lensdock-runtime-policy \
     lensdock-product_lensdock-runtime-policy
   ```

   Use the actual Compose project name if it differs. Preserve existing volume mappings; do not rerun `product-init`, delete `state.json`, or replace the active generation override.
3. Build and plan the release normally. Planning resolves newly declared retained datasets from Compose without changing the active generation, and capacity checks require the volume to exist. A successful switch persists the new mapping.

Candidate verification gets an empty policy volume and `LENSDOCK_MODE=AGENT`, so it can exercise CDP without changing the live workspace's policy. The live Runtime reattaches the retained volume: a saved `NOVNC` mode or pause still applies after the update, regardless of the startup mode variable.

A compatible rollback keeps this current policy instead of restoring an older decision from the Profile checkpoint. A Runtime release from before usage modes does not enforce the saved policy. Do not rely on `NOVNC` or pause as protection when rolling back to such a release; keep Agent access disabled outside that deployment until a compatible Runtime is running.

## Operating the stack

After the first managed generation, start, stop and inspect the stack only through the CLI. The CLI layers the generated override for the current generation, which pins exact image digests and maps each volume key to that generation's volume. A raw `docker compose` call omits the override and would start the baseline images on baseline volumes.

```bash
lensdock update compose --deployment "$LENSDOCK_DEPLOYMENT" -- up -d --wait
lensdock update compose --deployment "$LENSDOCK_DEPLOYMENT" -- ps
lensdock update compose --deployment "$LENSDOCK_DEPLOYMENT" -- logs --tail 100 gateway
lensdock update compose --deployment "$LENSDOCK_DEPLOYMENT" -- restart gateway
lensdock update compose --deployment "$LENSDOCK_DEPLOYMENT" -- down
```

The first compose argument must be one of `up`, `down`, `stop`, `start`, `restart`, `ps`, `logs` or `config`. Global Compose flags are refused because the deployment owns the project, files and env files. `down -v`, `--volumes`, `--rmi`, `up --build` and `up --pull` are also refused, because a generation runs images already verified by digest (`--pull=never` is accepted). `mise run product:up` and `mise run product:down` use the same command; `product:up` also runs the setup-only `config-init` step that prepares the credential volumes, which belongs to the first start. Restart a running deployment with `update compose`, not by repeating the first-start task.

The Gateway keeps browser sessions in memory. After any Gateway restart (an update switch, a rollback, `restart gateway`, a host reboot) every browser session is gone. Default local access resumes automatically. If pairing is required, open the UI and pair again with the human or viewer token from the private product directory; never put the token in a URL. Control leases do not survive a Runtime restart either, so humans and Agents acquire new ones.

### Stopping the host manager

```bash
lensdock admin manager stop
```

The command stops only the manager daemon of `${LENSDOCK_MANAGER_DIR:-~/.local/share/lensdock-manager}`. It asks the manager to shut down, then waits until the manager's handlers have returned and its singleton lock (`manager.lock`) is free. The manager refuses while any operation has an active worker, while a lifecycle helper holds the shared helper lock, or while any project's lock is held, and the command reports which one. Wait and retry. It never stops project containers, never removes data or images, and prints `"containersStopped": false`. With no manager running it returns at once. Stop the projects first with `project stop` if you want their browsers stopped as well.

The manager runs only while someone keeps it running: project-bound commands (`project up`, `workspace status --project`, MCP with `--project`, and so on) start it again, and no boot service is installed.

## Release evidence

An image tag is not a release. `manifest.json` pins each architecture by exact `repository@sha256:digest`, the Debian base digest, every dpkg package and version, the Chromium, noVNC, agent-browser, Gateway and CLI versions, the database schema (`gatewayDb` and the oldest `minGatewayDb` it can migrate from), the Linux CLI hash, rollback policy, tests and evidence files. Release-channel manifests also require:

- an SPDX or CycloneDX SBOM for every image;
- a BuildKit SLSA in-toto provenance statement bound to the platform manifest digest;
- a Trivy JSON scan of the exact index digest with zero critical findings;
- a real passed test report with commands, platforms and timestamps;
- `manifest.sig`, an Ed25519 signature by a key trusted by the deployment.

`tools/release/platform-evidence.sh` is the native amd64/arm64 evidence job. It uses Docker Buildx `--attest type=sbom` and `--attest type=provenance,mode=max,version=v1`, exports the actual attestations, scans the exact digest with an installed `trivy`, builds the exact Linux CLI with `mise exec -- go`, and creates one single-architecture dev manifest. It needs explicit registry refs because OCI attestations are preserved in a registry, not in the classic Docker image store. Running it is the explicit release publication action; `update check`, `plan` and `apply` never run it.

After both native jobs and a test run produce their evidence:

```bash
RELEASE_ID=2026.10.25 \
AMD64_DIR=out/release/2026.10.25/amd64 \
ARM64_DIR=out/release/2026.10.25/arm64 \
TEST_REPORT=out/acceptance/release-test-report.json \
TEST_SUMMARY='<what the report actually covers>' \
KEY_FILE=/offline/lensdock-release.key \
LENSDOCK_BIN=bin/lensdock \
OUTPUT=.lensdock/product/releases/2026.10.25 \
  tools/release/assemble-release.sh
```

Assembly merges architecture manifests, validates and hashes real evidence, promotes from `dev` to `release`, signs the exact manifest bytes, and verifies with the signing key. It does not run tests or claim they passed, and an invalid or failed test report cannot be promoted. `release manifest` generates only `--dev` manifests; the release channel exists only through `release promote` and this script.

The test report is real machine output in this minimum form. Set `passed` to true only when every listed command succeeded, and list only commands that ran:

```json
{
  "schemaVersion": 1,
  "releaseId": "2026.10.25",
  "passed": true,
  "startedAt": "2026-10-25T01:00:00Z",
  "finishedAt": "2026-10-25T01:25:00Z",
  "commands": ["mise run check", "mise run e2e:host", "mise run e2e:sidecar"],
  "platforms": ["linux/amd64", "linux/arm64"]
}
```

No single mise task produces a promotable integrated test report.

## Building a local development release

For a local, non-publishable release, build the images and the Linux CLI for the Docker engine architecture, then record them. The manifest holds a real SPDX package SBOM of the Runtime image (its dpkg database and the Go binary). It has no Gateway SBOM, provenance, scan, test report or signature, and it invents none of them. `LENSDOCK_ARCH` defaults to the host architecture, which matches the engine on OrbStack and Docker Desktop; set it explicitly if the engine differs. The manifest needs an immutable local image digest, so the engine must use Docker's containerd image store (or the images must be pushed or loaded with a digest); otherwise the command fails.

```bash
mise run build:linux
mise run image:build
mise run gateway:image
mkdir -p .lensdock/product/releases
bin/lensdock release manifest --dev --uncommitted \
  --id local-update-1 \
  --output .lensdock/product/releases/local-update-1 \
  --runtime-image lensdock/runtime:dev \
  --gateway-image lensdock/gateway:dev \
  --linux-cli bin/linux-arm64/lensdock \
  --agent-browser-version 0.39.2 \
  --cli-version development \
  --gateway-version development \
  --gateway-db 2 --min-gateway-db 1
```

Notes on the flags:

- `--output` must not exist yet, and its parent must. The command creates one directory, so run `mkdir -p` on the parent first. An existing output directory is refused so evidence is never overwritten.
- `--uncommitted` records a working tree with uncommitted changes (or a repository without a commit to name). It cannot be combined with `--git-commit` or `--source-clean`, and such a release can never be promoted.
- `--linux-cli` is the Linux binary for the image architecture (`bin/linux-arm64/lensdock` here, `bin/linux-amd64/lensdock` on an amd64 engine). The manifest covers that single architecture, and the plan checks its SHA-256 again before maintenance starts.
- With a Gateway image, `--gateway-db` and `--min-gateway-db` are required. Set them to the schema the Gateway migrates to and the oldest schema it can migrate from. A release whose Gateway cannot read the running schema is refused by the plan, and a downgrade needs `--allow-downgrade`.
- Without `--gateway-image` the release covers the Runtime only; omit both schema flags then.

A plan admits a dev release only with `--allow-dev`, and prints warnings for the missing SBOM, provenance, scan, test report and signature, so the release is visibly not supply-chain verified.

## Check, dry plan and apply

`check` reads release directories and writes the sanitized status that the Gateway mounts read-only. It never pulls, stops or restarts anything:

```bash
lensdock update check --deployment "$LENSDOCK_DEPLOYMENT" --workspace default
```

Incomplete release directories stay visible with `issues` and `risk.level = "blocked"`. A bare image tag, or a manifest missing evidence or a signature, is rejected.

A dry plan checks the manifest, signature and evidence, exact local image digests, architecture, schema compatibility, the Linux verifier hash, current lease and maintenance impact, named volume mapping, the compose mounts the candidate inherits, the backup key, and both backup and Docker free space (Docker Compose 2.24 or newer is required). It also checks that each running image can be kept for restore and rollback (see [Image retention](#image-retention)). It does not modify the browser and does not tag anything.

```bash
mkdir -p .lensdock/product/plans
lensdock update plan \
  --deployment "$LENSDOCK_DEPLOYMENT" \
  --release local-update-1 --workspace default --allow-dev \
  --output .lensdock/product/plans/local-update-1.json
lensdock update apply \
  --deployment "$LENSDOCK_DEPLOYMENT" \
  --plan .lensdock/product/plans/local-update-1.json --dry-run
```

The parent of `--output` must exist. Without `--output` the plan is written to `<stateDir>/plans/<plan-id>.json`, which the CLI creates. A plan file is never overwritten, so choose a new name for every plan. A plan with blockers is printed and no applicable file is written. A plan is valid for 24 hours and is rejected when the deployment changed since it was made.

Start maintenance and candidate verification. `--yes` confirms stopping and checkpointing; it does **not** approve the switch:

```bash
lensdock update apply \
  --deployment "$LENSDOCK_DEPLOYMENT" \
  --plan .lensdock/product/plans/local-update-1.json --yes
```

The command runs these steps in order, each journaled under `<stateDir>/updates/<id>/`:

1. Verify the release, the exact local image digests and the Linux verifier hash, then keep the running images (see [Image retention](#image-retention)). If an image cannot be kept, nothing has been stopped yet.
2. Check free space for the checkpoint and the candidates.
3. Stop the Gateway, then fence the Runtime in `MAINTENANCE` so no new lease is issued.
4. Wait for the current lease to drain. A lease still held after the drain timeout stops the update unless `--revoke-leases` was given. Revoking cannot undo what an Agent already sent to a website.
5. Stop the Runtime cleanly and require a clean result (see [Clean stop](#clean-stop)).
6. Encrypt Profile, keyring and Gateway database into one authenticated checkpoint. Decrypt every checkpoint file and walk each tar before publishing it.
7. Create new candidate volumes from the checkpoint and compare complete content and metadata fingerprints.
8. Run the Gateway migration on the cloned database in a container with no network, a read-only root, no capabilities and the configured user. Retained datasets follow their `candidateAccess`, so the migration sees feedback artifacts and credentials read-only. Then start the candidate Runtime in its own Compose project on the cloned volumes, with no published ports, its own operator token, and its Runtime TLS variables forced empty so the local probe speaks plain HTTP. Its scratch usage policy starts in `AGENT` mode; the saved production mode and pause are untouched. The probe verifies readiness, the Chromium sandbox (skipped when `LENSDOCK_CHROMIUM_SANDBOX` is off, a failure when it is on and unverified), fonts, exact dpkg versions, Chromium, CDP and a built-in loopback-only UTF-8/PNG fixture.
   Candidate reports use `reason: "chromium-sandbox-disabled"` for the permitted sandbox skip. Admission uses that code, not the human-readable diagnostic text. A missing or unknown skip reason still blocks switching.
9. Require the probe's egress check to pass (public addresses, DNS and the cloud metadata address are unreachable) and the candidate network to be internal. Then stop the candidate, check that its Profile has no lock files and a clean `profile.exit_type`, and remove its scratch volumes. The cloned volumes stay: they become the live volumes at the switch.
10. Switch, after approval (below).
11. Start the new generation and require health.

If any step before the switch fails, the previous Runtime and Gateway restart on their original volumes and the journal records `aborted`. The encrypted checkpoint stays.

After verification, the command prints the exact checkpoint and version diff and asks for explicit switch approval. If stdin is not interactive it exits in `awaiting-approval`. Inspect and finish it with:

```bash
lensdock update status --deployment "$LENSDOCK_DEPLOYMENT"
lensdock update approve --deployment "$LENSDOCK_DEPLOYMENT" --update UPDATE_ID --yes
# or leave the old data untouched and restart it:
lensdock update abort --deployment "$LENSDOCK_DEPLOYMENT" --update UPDATE_ID
```

`apply --approve-switch` approves the switch in the same invocation, after candidate verification passed. Use it only when no one needs to read the diff.

The switch writes a new generation override with exact image digests and new snapshot volume names, then starts Runtime and Gateway and requires health. Every old control lease is gone and nothing resumes automatically. Because the Gateway restarts, browser sessions are gone too; default local access resumes automatically, and a deployment that requires pairing must pair the UI again.

### Image retention

A generation runs its images by `repository@digest`. When `docker build -t repository:dev` is repeated, the tag moves to the new image and the old image keeps no name in that repository. The engine then cannot resolve `repository@digest` for it, even if a tag in another repository still holds the content. A restore or rollback that depends on the old image would fail after everything was already stopped.

The CLI therefore gives every image that a restore or rollback may start a tag inside its own repository, unique to its full digest:

```text
repository:lensdock-keep-<64 hex digits of the digest>
```

This happens before anything is stopped. The CLI inspects all images first and fails before writing any tag if an image is missing, has another digest, is built for another platform, or if its keep tag already names a different image. After tagging it checks that both the original reference and the keep tag resolve to exactly the pinned digest and platform. If a running image has already lost its name, the CLI re-tags it from a local image that carries exactly the pinned digest and platform. Nothing is pulled and no mutable tag is trusted. `update plan` only reports whether this is possible; `apply` creates the tags in step 1, and the switch also keeps the images of the new generation. Rollback keeps both the checkpoint's previous-generation images and the images running now before it stops anything.

`lensdock update` never removes these tags or any other image. Do not delete `lensdock-keep-*` tags, and do not prune images, while a checkpoint that references them is retained.

### Clean stop

A checkpoint is labeled clean only when the stop was clean. Before the Gateway and Runtime stop, the Gateway drains: it refuses new event subscriptions and WebSocket upgrades, cancels live SSE streams and upgraded VNC and CDP connections, waits for their handlers to return, lets ordinary requests finish, and only then closes the database. Event subscribers read a normal end of stream, so a live subscription or an open VNC session does not hold the Gateway until its stop grace expires. Step 5 also checks exit codes: a Gateway or Runtime that exited non-zero (137 means it was killed after the grace period) turns the checkpoint into a crash snapshot, and the update stops.

The Runtime closes Chromium through `Browser.close` before it signals the process (it waits up to 3 seconds for the request and the exit, then falls back to signals), because the SIGTERM path exits without removing the profile locks. The update then requires all of:

- Runtime and Gateway exit code 0;
- no top-level `Singleton*` entries left in the profile volume;
- `profile.exit_type` of `Normal` or `SessionEnded` in `Default/Preferences`;
- no unflushed SQLite journal. A nonempty rollback journal counts only when its 8-byte header is not the all-zero header written by `journal_mode=PERSIST`; a committed persistent journal is expected after a clean close and is left as it is. The CLI never deletes profile journals. Browser WAL files may remain in the profile snapshot, while a Gateway database WAL must have been checkpointed.

If a check fails, the previous services are restored and nothing is switched. `--accept-crash-snapshot` continues with a snapshot that is not clean. The checkpoint is then marked as a crash checkpoint, and rolling back to it requires `--allow-crash-checkpoint`. Use it only when you understand what was not flushed.

## Repeatable success, failure and rollback test

Use `tests/fixtures/handoff.html` as the Profile fixture. Before applying an update, set marker `pre-<run-id>`; the page writes and reads a persistent Cookie, Local Storage and IndexedDB. Create one feedback row and a small published artifact with the same marker through the real Gateway API, and record their IDs and hashes. This exercises Profile, Gateway database and retained artifact behavior without any real account.

A deterministic failed candidate uses a derived image whose entrypoint exits:

```dockerfile
FROM lensdock/runtime:dev
ENTRYPOINT ["/bin/false"]
```

Build it locally under a tag of its own (keep `lensdock/runtime:dev` for the successful runs), generate a dev release for its exact digest, plan with `--allow-dev`, then apply. Candidate readiness fails before the switch. Expected result:

1. The previous Runtime and Gateway restart on their original volumes.
2. The encrypted checkpoint remains.
3. The failed candidate branch stays visible in `update branches list` for diagnosis. A failure while copying removes only the incomplete candidate.
4. `handoff.html` still reads the pre-update Cookie, Local Storage and IndexedDB values, and the Gateway still reads the pre-update row and artifact.

This image is a test fixture only. Never sign or publish it as a release.

For success, use the real candidate and approve the switch. Verify the same Profile marker and pre-update Gateway row and artifact. Then set marker `post-<run-id>` and create a second Gateway row and artifact. Roll back with the checkpoint printed by apply:

```bash
lensdock update rollback \
  --deployment "$LENSDOCK_DEPLOYMENT" \
  --checkpoint CHECKPOINT_ID --dry-run
lensdock update rollback \
  --deployment "$LENSDOCK_DEPLOYMENT" \
  --checkpoint CHECKPOINT_ID --yes
```

The rollback decrypts the checkpoint into **new** volumes, compares fingerprints, then starts the paired old Runtime and Gateway images with the pre-update Profile, keyring and database. Expected result:

- `pre-<run-id>` exists in Cookie, Local Storage and IndexedDB.
- `post-<run-id>` is absent from the restored Profile and Gateway database. The recovery point is the checkpoint time.
- Both feedback artifacts remain, because artifacts are retained rather than rolled back. The post-update feedback row is gone from the restored database and its API read returns 404.
- The update-generation volumes containing `post-<run-id>` stay listed as a retired branch and can be inspected or recovered. The old image never opens them.
- The Runtime generation changes, all users and Agents acquire new leases, and Gateway browser sessions must be paired again.

### Database schema changes and paired rollback

A forward Gateway migration runs first on the clone made from the checkpoint, before any candidate boots. The live database moves to the new schema only at the switch. On the way back, the checkpoint holds the older-schema database. Rollback restores that database and starts the old Gateway binary against it. It never starts an old Gateway binary against a newer-schema database. The plan refuses a release whose Gateway would read a schema newer than it knows unless `--allow-downgrade` is given (the update still uses a paired checkpoint).

Schema 2 lets the Gateway store a standalone drawing capture with no Runtime, target or navigation, and checks that such a capture carries no runtime context. Its manifest records `--gateway-db 2 --min-gateway-db 1`. After rolling back from schema 2:

- the old Gateway reports schema 1 and previously published feedback reads back identical;
- reading a drawing capture created after the upgrade returns 404, because it was not part of the restored schema 1 database;
- to move forward again, create a new plan for the same release with a new `--output` name and apply it.

## Recovery

`status` shows every operation, the pending plan and the recovery text of a stopped operation:

```bash
lensdock update status --deployment "$LENSDOCK_DEPLOYMENT"
lensdock update checkpoints list --deployment "$LENSDOCK_DEPLOYMENT"
lensdock update branches list --deployment "$LENSDOCK_DEPLOYMENT"
```

| Journal status | Meaning | Operator action |
|---|---|---|
| `awaiting-approval` | Candidate verified, old services still stopped for maintenance | `update approve --update ID --yes`, or `update abort --update ID` (add `--keep-candidate` to keep the candidate volumes) |
| `aborted` | Stopped before the switch; previous services run again on their own volumes | Fix the cause and plan again. The volumes of a failed candidate are kept for diagnosis |
| `failed-reverted` | The switch failed; the previous generation runs again | Same as `aborted`. Inspect the journal for the cause |
| `failed-held` | Services are stopped or in an unknown state | Read the recovery text in `status`, fix the cause, then run `update abort --update ID` again. The checkpoint and every volume are intact |
| `completed` | Update or rollback finished | Roll back with `update rollback --checkpoint ID` if the new generation misbehaves |

A CLI process killed mid-operation, or a host restart, leaves its journal in place, and `abort` rebuilds the run from it. For a held rollback, start the running generation with `update compose -- up -d --wait`, fix the cause and retry the rollback.

Do not work around these states. Do not start the stack with a raw `docker compose` call, delete volumes or journals by hand, retry with `--accept-crash-snapshot`, or point the Gateway at a database it did not migrate. `branches delete` refuses current, in-flight and foreign volumes.

List and verify checkpoints and branches, then prune only unreferenced checkpoints:

```bash
lensdock update checkpoints list --deployment "$LENSDOCK_DEPLOYMENT"
lensdock update checkpoints verify CHECKPOINT_ID --deployment "$LENSDOCK_DEPLOYMENT"
lensdock update checkpoints prune --deployment "$LENSDOCK_DEPLOYMENT"
lensdock update branches list --deployment "$LENSDOCK_DEPLOYMENT"
# Explicit only; refuses current, in-flight and foreign volumes:
lensdock update branches delete VOLUME --deployment "$LENSDOCK_DEPLOYMENT" --yes
```

`deployment-status.json` holds only release, update and checkpoint summaries. It never contains tokens, certificate or key paths, host paths, Docker volume names, or Profile and database content. The Gateway serves it read-only, so the web UI cannot apply or roll back an update. `status --publish` rewrites it atomically.

## Cleanup of unused old system data

`update cleanup` and the automatic triggers share one implementation (`Engine.ExpireUnused`). The runbook's [Retention and expiry](UPDATE-RUNBOOK.md#retention-and-expiry-of-unused-old-system-data) section has the policy, defaults, scopes, precedence and refusals; this section records what the code does.

```bash
lensdock update expiry --deployment "$LENSDOCK_DEPLOYMENT"                           # show policy and last run
lensdock update expiry --deployment "$LENSDOCK_DEPLOYMENT" --expire-after-days 14    # store 14 days; 0 disables
lensdock update cleanup --deployment "$LENSDOCK_DEPLOYMENT" --dry-run                # preview, nothing removed
lensdock update cleanup --deployment "$LENSDOCK_DEPLOYMENT" --scope volumes --older-than-days 7 --yes
```

- The run takes the deployment lock (`<stateDir>/state/lock`). While an update or rollback holds it, the run returns the operation-in-progress error.
- The run reads `state.json` and every journal first. If either is unreadable, or `state.json` is missing although completed operations exist, it deletes nothing.
- Scopes run in the order `checkpoints`, `volumes`, `generations`, `staging`. A scope that cannot build its inventory records an entry in `scopeErrors` and the others continue. Items sort by id within a scope.
- Age starts from: the checkpoint's `createdAt`; for a volume the `io.lensdock.created-at` label, moved forward to the finish of the journal that recorded it as retired (completed operations only) or, for a volume of an aborted or reverted operation, to that operation's finish; for a generation the `updatedAt` of the next valid generation record; for staging the UTC timestamp embedded in the checkpoint or plan id. A volume of a completed operation that no record says was retired is kept. A missing, unparsable or future time keeps the item. An item is due when its age is at least the configured number of days.
- A volume must also be unmounted by every container at removal time. The check repeats immediately before `docker volume rm`, so a volume that gained a user after the preview is kept. Expiry never passes `--force`.
- The report is written atomically (0600) to `<stateDir>/state/expiry-last.json` after each real run. `update expiry` prints it as `lastRun`, and a corrupt or unsupported file is reported as `lastRunError` rather than ignored.
- `AutoExpire` skips when the age is 0 and when the last recorded run was automatic, clean (no failed item, no scope error) and less than 24 hours old. After a failed run it is attempted again at the next trigger. A manual run replaces the recorded last run.
- Triggers: a standalone `update apply` that reaches `completed`, `update approve`, `update abort` and a real `update rollback` call it after success. For managed deployments the manager's housekeeping queues a typed `expire` update marked `automatic` once a day when it is idle; clients cannot set that marker. `update cleanup` without `--dry-run` goes through the manager as a confirmed typed operation.
- `update expiry --expire-after-days N` rereads `deployment.json` under the deployment lock and changes only `expireAfterDays`, so a concurrent edit of another setting is kept. A managed deployment's file is written by the manager's worker as a typed policy operation, under the project and helper locks.

## Deployment data on disk

`<state>` is the directory of `deployment.json` (`~/.local/share/lensdock` for the installer, `<manager>/projects/<id>` for a managed project) and the deployment's `stateDir` is `<state>/product`.

| Path under `<stateDir>` | Content | Removal notes |
|---|---|---|
| `operator-token`, `*-token`, `ca.crt`, `gateway.*`, `runtime.*`, `gateway.json` | Pairing tokens, Runtime certificates and keys, Gateway configuration | Written into the credential volumes at first start; keep together with the volumes |
| `compose.env`, `runtime.env` | Compose and Runtime settings, including the pinned image references | Same |
| `release/`, `images.pin.yaml` | Private copy of the release bundle and the first-start image pins | Same |
| `state/` (`state.json`, `lock`, `expiry-last.json`) | Running generation and volume mapping | The only file that maps compose volume keys to the current generation's Docker volume names |
| `updates/`, `generations/`, `plans/` | Journals, per-generation `state.json` and `compose.override.yaml`, saved plans | Expiry removes obsolete generations and old plans only |
| `backups/` (or `backupDir`), `keys/backup.key` (or `backupKeyFile`) | Encrypted checkpoints and their key | A custom location is outside the state directory and survives its removal |
| `releases/` (or `releasesDir`) | Installed release manifests used by later plans | Can be external |
| `public/` | Sanitized `deployment-status.json` for the Gateway | Safe to regenerate with `update status --publish` |

A deployment initialized from a source checkout keeps its state in the directory chosen at initialization (`.lensdock` by default) and points back at the checkout. Treat that directory like `<state>`; never remove the checkout itself.

## Volume identity

Docker volumes of a deployment come from four sources, and the inventory in the runbook uses their union:

- The running generation: `<stateDir>/state/state.json`, `components.<service>.volumes`, maps each compose volume key to the Docker volume in use. Before the first switch the file is absent and the Compose project's own volumes (label `com.docker.compose.project`) are the running ones.
- Compose-created volumes carry `com.docker.compose.project=<project>` and `com.docker.compose.volume=<key>`. The project is `lensdock-product` by default for a standalone deployment and `ld-<project id>` for a managed one. Volumes cloned by an update or rollback do not carry these labels.
- Update-created volumes carry `io.lensdock.managed=true` plus `io.lensdock.update-id`, `io.lensdock.role`, `io.lensdock.dataset` and `io.lensdock.created-at`. The managed label is shared by every deployment on the engine and is not evidence of ownership; `update branches list` accepts a volume only when the running generation uses it, its `update-id` names one of this deployment's journals, or a journal records it as candidate, scratch or retired. It reports `volume`, `role`, `updateId`, `dataset`, `createdAt` and `inUse`, and includes retired baseline volumes that have no labels.
- Containers of the project: `docker inspect` of its containers lists the mounted volumes while they exist. Managed containers also carry `io.lensdock.manager-id` and `io.lensdock.project-id`.

`update branches delete VOLUME --yes` removes exactly one volume and refuses names that are invalid, used by the running generation, referenced by an unfinished operation, not managed, or not created or retired by this deployment. A volume that a journal records as retired is removed through plain `docker volume rm`, which Docker itself refuses while a container uses it. No LensDock command removes the running generation's volumes, runs a global prune or removes images; removal of those is the manual procedure in the runbook.
