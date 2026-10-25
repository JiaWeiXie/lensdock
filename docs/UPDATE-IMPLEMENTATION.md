# Host update implementation and acceptance procedure

This document is the executable companion to [SPEC §15](../SPEC.md) and
[UPDATE-RUNBOOK.md](UPDATE-RUNBOOK.md). All examples run on the host from the
repository root. Runtime, Gateway and browser containers never mount the Docker
socket.

The examples call the host CLI as `bin/lensdock` (built by `mise run build`) and
read the deployment file from `LENSDOCK_DEPLOYMENT`:

```bash
mise run build
export LENSDOCK_DEPLOYMENT="$PWD/.lensdock/deployment.json"
```

Releases built from a working tree are development releases. They are unsigned
and carry no BuildKit provenance and no scanner output. The signed release
channel is described below; it has not been run end to end. What was exercised
and what was not is listed in [Exercised evidence and
limits](#exercised-evidence-and-limits).

## Deployment contract

`lensdock update` reads `--deployment FILE`, then `$LENSDOCK_DEPLOYMENT`, then
`<state-dir>/deployment.json`. Paths relative to this file resolve against its
directory; unknown fields are errors. `docker compose` is always invoked with
the configured project, base files, `envFiles`, project directory and the
current generation override. This preserves a product init's custom state
directory, port, origin and workspace.

`mise run product:init` writes this file once per state directory. It refuses to
overwrite an existing `product/` directory. Do not run it again on a managed
deployment and do not delete `product/` to force it: that directory holds the
pairing tokens, the Runtime certificates and the Gateway and Runtime
configuration. The shape it writes:

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
| `retainCheckpoints` | 3 |
| `minFreeBytes` | 512 MiB |
| `stopGraceSeconds` | 40 |
| `drainTimeoutSeconds` | 45 |
| `healthTimeoutSeconds` | 180 |
| `gateway.user` | `10000:10000` (the migration container) |
| `trustedKeys` | none; required to admit a `release` channel manifest |

Each dataset has a role:

- `snapshot` volumes are encrypted into every checkpoint, cloned for the
  candidate and restored as a pair on rollback. They never set `candidateAccess`.
- `retained` volumes are never copied or rolled back. The new generation
  reattaches them. `candidateAccess` decides what verification containers see:
  `scratch` is a new empty volume, `readonly` is the real volume mounted
  read-only. The defaults are `scratch` for `downloads` and `private`, and
  `readonly` for `artifacts` and `other`.

`candidateAccess` describes the candidate only. The live services keep the access
they need: the live Gateway mounts `gateway-artifacts` writable because it stores
published feedback there, while Gateway and Runtime credentials are mounted
read-only. The Gateway migration container mounts every Gateway dataset at its
`mountPath` according to `candidateAccess`, so a writable live artifacts mount
does not block a read-only migration. The candidate Runtime does inherit the compose mounts of
the Runtime service: any dataset shared read-only with it, such as the Runtime
credentials, must be mounted `:ro` in compose, and the plan reports a blocker
otherwise.

Setup-only `config-init` is not an owned service and is never started by
candidate verification. It writes the credential volumes during the one-time
bootstrap.

The profile and Gateway database must be named volumes, never bind mounts.
Downloads, feedback artifacts, private drafts and the runtime usage policy are
not rolled back. Runtime credentials and Gateway credentials are retained
configuration, not backup content. The backup key is a private 0600 file outside the backup directory. The
public status directory is a read-only bind mount into the Gateway and is not a
dataset.

Initialize the backup key without replacing an existing one:

```bash
bin/lensdock update init --deployment "$LENSDOCK_DEPLOYMENT"
```

For the M0 compose only, `update init` can create the documented runtime,
profile, downloads and runtime-policy scaffold. Product init writes its full
deployment JSON before this command runs.

### Adding the runtime policy volume

New installations declare `lensdock-runtime-policy:/data/runtime`. The Runtime
stores `agent-control.json` there, with mode `0600` inside a `0700` directory owned
by UID/GID `10000:10000`. Chromium runs as UID `10001` and cannot read or change
this policy. Do not put it in the browser-owned Profile or Downloads volume.

For an existing installation, keep its current generation and data:

1. Add the runtime-policy mount and named volume declaration from the current
   Compose file. Add the `lensdock-runtime-policy` dataset shown above to that
   workspace in the deployment JSON, including `"candidateAccess": "scratch"`.
2. Create the new empty volume before planning. For the default product project:

   ```bash
   docker volume create \
     --label com.docker.compose.project=lensdock-product \
     --label com.docker.compose.volume=lensdock-runtime-policy \
     lensdock-product_lensdock-runtime-policy
   ```

   Use the actual Compose project name if it differs. Preserve existing volume
   mappings; do not rerun `product-init`, delete `state.json`, or replace the
   active generation override.
3. Build and plan the release normally. Planning resolves newly declared
   retained datasets from Compose without changing the active generation.
   Capacity checks require the volume to exist. A successful switch persists
   the new mapping.

Candidate verification gets an empty policy volume and `LENSDOCK_MODE=AGENT`
so it can exercise CDP without changing the live workspace's policy. The live
Runtime reattaches the retained volume: a saved NOVNC mode or pause still applies
after the update, regardless of the startup mode environment variable.

Compatible rollback keeps this current policy instead of restoring an older
permission decision from the Profile checkpoint. A Runtime release from before
usage modes existed does not enforce the saved policy. Do not rely on NOVNC or
pause as a protection when rolling back to such a release; keep Agent access
disabled outside that old deployment until a compatible Runtime is running.

## Operating the stack

After the first managed generation, start, stop and inspect the stack only through
the CLI. The CLI layers the generated override for the current generation, which
pins exact image digests and maps each volume key to the volume of that
generation. A raw `docker compose` call omits the override and would start the
baseline images on baseline volumes.

```bash
bin/lensdock update compose --deployment "$LENSDOCK_DEPLOYMENT" -- up -d --wait
bin/lensdock update compose --deployment "$LENSDOCK_DEPLOYMENT" -- ps
bin/lensdock update compose --deployment "$LENSDOCK_DEPLOYMENT" -- logs --tail 100 gateway
bin/lensdock update compose --deployment "$LENSDOCK_DEPLOYMENT" -- restart gateway
bin/lensdock update compose --deployment "$LENSDOCK_DEPLOYMENT" -- down
```

The first compose argument must be one of `up`, `down`, `stop`, `start`,
`restart`, `ps`, `logs` or `config`. Global Compose flags are not accepted
because the deployment owns the project, files and env files. `down -v`,
`--volumes` and `--rmi` are refused, and so are `up --build` and `up --pull`
(`--pull=never` is accepted), because a generation runs images that were already
verified by digest. `mise run product:up` and `mise run product:down` use the
same command; `product:up` also runs the setup-only `config-init` step that
prepares the credential volumes, which belongs to the first start. Restart a
running deployment with `update compose`, not by repeating the first-start task.

The Gateway keeps browser sessions in memory by design. After any Gateway restart
(an update switch, a rollback, `restart gateway`, a host reboot) every browser
session is gone. Open the UI and pair again with the human or viewer pairing
token from the private product directory; never put the token in a URL. Control
leases do not survive a Runtime restart either, so humans and agents acquire new
ones.

## Release evidence

An image tag is not a release. `manifest.json` pins each architecture by exact
`repository@sha256:digest`, the Debian base digest, every dpkg package/version,
Chromium/noVNC/agent-browser/Gateway/CLI versions, database schema (`gatewayDb`
and the oldest `minGatewayDb` it can migrate from), Linux CLI hash, rollback
policy, tests and evidence files. Release-channel manifests also require:

- an SPDX or CycloneDX SBOM for every image;
- a BuildKit SLSA in-toto provenance statement bound to the platform manifest
  digest;
- a Trivy JSON scan of the exact index digest (zero critical findings);
- a real passed acceptance report with commands, platforms and timestamps;
- `manifest.sig`, an Ed25519 signature by a key trusted by the deployment.

`tools/release/platform-evidence.sh` is the native amd64/arm64 evidence job. It
uses Docker Buildx `--attest type=sbom` and
`--attest type=provenance,mode=max,version=v1`, exports the actual attestations,
scans the exact digest with an installed `trivy`, builds the exact Linux CLI
with `mise exec -- go`, and creates one single-architecture dev manifest. It
requires explicit registry refs because OCI attestations are preserved in a
registry, not in the classic Docker image store. Running it is the explicit
release publication action; `update check`, `plan` and `apply` never run it.

After both native jobs and an acceptance run produce their evidence:

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

Assembly merges architecture manifests, validates and hashes real evidence,
promotes from `dev` to `release`, signs exact manifest bytes, and verifies with
the signing key. It does not run tests or claim they passed. An invalid or
failed test report cannot be promoted. `release manifest` generates only `--dev`
manifests; the release channel exists only through `release promote` and this
script.

The required test report is real machine output in this minimum form. Never set
`passed` to true unless every listed command actually succeeded, and list only
commands that ran:

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

The repository has no single mise task that produces a promotable integrated
M1 to M3 acceptance report. The integrated demo evidence described at the end of
this document is a local run, not an input to `release promote`.

## Building a local development release

For a local, non-publishable release, build the images and the Linux CLI for the
Docker engine architecture, then record them. The manifest holds a real SPDX
package SBOM of the Runtime image (its dpkg database and the Go binary). It has
no Gateway SBOM, no provenance, no scan, no test report and no signature, and it
invents none of them. `LENSDOCK_ARCH` defaults to the host architecture, which
matches the engine on OrbStack and Docker Desktop; set it explicitly if the
engine differs. The manifest needs an immutable local image digest, so the
engine must use Docker's containerd image store (or the images must be pushed or
loaded with a digest); without one the command fails.

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

- `--output` must not exist yet, and its parent must. The command creates one
  directory, so run `mkdir -p` on the parent first. An existing output directory
  is refused so evidence is never overwritten.
- `--uncommitted` records a working tree with uncommitted changes (or a
  repository without a commit to name). It cannot be combined with
  `--git-commit` or `--source-clean`, and such a release can never be promoted.
- `--linux-cli` is the Linux binary for the image architecture
  (`bin/linux-arm64/lensdock` here, `bin/linux-amd64/lensdock` on an amd64 engine).
  The manifest covers that single architecture, and the plan checks its SHA-256
  again before maintenance starts.
- With a Gateway image, `--gateway-db` and `--min-gateway-db` are required. This
  release's Gateway migrates the database to schema 2 and can migrate from
  schema 1. A release whose Gateway cannot read the running schema is refused by
  the plan, and a downgrade needs `--allow-downgrade`.
- Without `--gateway-image` the release covers the Runtime only; omit both
  schema flags then.

Plan admits a dev release only with `--allow-dev`. The plan then prints warnings
for the missing SBOM, provenance, scan, test report and signature, so the release
is visibly not supply chain verified.

## Check, dry plan and apply

`check` reads release directories and writes the sanitized status mounted
read-only by the Gateway. It never pulls, stops or restarts anything:

```bash
bin/lensdock update check --deployment "$LENSDOCK_DEPLOYMENT" --workspace default
```

Incomplete release directories remain visible with `issues` and `risk.level =
"blocked"`. A bare image tag or a manifest missing evidence/signature is
rejected (UP-07).

Create a dry plan. It checks the manifest/signature/evidence and exact local
image digests, architecture, schema compatibility, Linux verifier hash, current
lease/maintenance impact, named volume mapping, the compose mounts inherited by
the candidate, backup key and both backup and Docker free space (Docker Compose
2.24 or newer is required). It also checks that each running image can be kept
for restore and rollback (see [Image retention](#image-retention)). It does not
modify the browser and does not tag anything.

```bash
mkdir -p .lensdock/product/plans
bin/lensdock update plan \
  --deployment "$LENSDOCK_DEPLOYMENT" \
  --release local-update-1 --workspace default --allow-dev \
  --output .lensdock/product/plans/local-update-1.json
bin/lensdock update apply \
  --deployment "$LENSDOCK_DEPLOYMENT" \
  --plan .lensdock/product/plans/local-update-1.json --dry-run
```

The parent of `--output` must exist; without `--output` the plan is written to
`<stateDir>/plans/<plan-id>.json`, which the CLI creates. A plan file is never
overwritten, so choose a new name for every plan. A plan with blockers is printed
and no applicable file is written. A plan is valid for 24 hours and is rejected
when the deployment changed since it was made.

Start maintenance and candidate verification. `--yes` confirms stopping and
checkpointing; it does **not** approve the switch:

```bash
bin/lensdock update apply \
  --deployment "$LENSDOCK_DEPLOYMENT" \
  --plan .lensdock/product/plans/local-update-1.json --yes
```

The command runs these steps in order, and each one is journaled under
`<stateDir>/updates/<id>/`:

1. Verifies the release, the exact local image digests and the Linux verifier
   hash, then keeps the running images (see [Image retention](#image-retention)).
   If an image cannot be kept, nothing has been stopped yet.
2. Checks free space for the checkpoint and the candidates.
3. Stops the Gateway, then fences the Runtime in `MAINTENANCE` so no new lease is
   issued.
4. Waits for the current lease to drain. A lease that is still held after the
   drain timeout stops the update unless `--revoke-leases` was given; revoking
   cannot undo what an agent already sent to a website.
5. Stops the Runtime cleanly and requires a clean result (see [Clean
   stop](#clean-stop)).
6. Encrypts Profile, keyring and Gateway DB into one authenticated checkpoint,
   decrypts every checkpoint file and walks each tar before publishing it.
7. Creates new candidate volumes from the checkpoint and compares complete
   content and metadata fingerprints.
8. Runs the Gateway migration on the cloned database in a container with no
   network, a read-only root, no capabilities and the configured user. Retained
   datasets follow their `candidateAccess`, so the migration sees feedback
   artifacts and credentials read-only. Then starts the candidate Runtime in its
   own Compose project on the cloned volumes, with no published ports, its own
   operator token and its Runtime TLS variables forced empty so the local probe
   speaks plain HTTP. Its scratch usage policy starts in AGENT mode; the saved
   production mode and pause are untouched. The probe verifies readiness,
   sandbox, fonts, exact dpkg versions, Chromium, CDP and a built-in loopback-only
   UTF-8/PNG fixture.
9. Requires the probe's egress check to pass (public addresses, DNS and the
   cloud metadata address are unreachable) and the candidate network to be
   internal. Then stops the candidate, checks that its Profile has no lock files
   and a clean `profile.exit_type`, and removes the candidate's scratch volumes.
   The cloned volumes stay: they become the live volumes at the switch.
10. Switches, after approval (below).
11. Starts the new generation and requires health.

If any step before the switch fails, the previous Runtime and Gateway restart on
their original volumes and the journal records `aborted`. The encrypted
checkpoint stays.

After verification, the command prints the exact checkpoint and version diff
and asks for explicit switch approval. If stdin is not interactive it exits in
`awaiting-approval`; inspect and finish it with:

```bash
bin/lensdock update status --deployment "$LENSDOCK_DEPLOYMENT"
bin/lensdock update approve --deployment "$LENSDOCK_DEPLOYMENT" --update UPDATE_ID --yes
# or leave the old data untouched and restart it:
bin/lensdock update abort --deployment "$LENSDOCK_DEPLOYMENT" --update UPDATE_ID
```

`apply --approve-switch` approves the switch in the same invocation, after the
candidate verification passed. Use it only when no one needs to read the diff.

The switch writes a new generation override with exact image digests and new
snapshot volume names, then starts Runtime and Gateway and requires health.
Every old control lease is gone and nothing automatically resumes. Because the
Gateway restarts, browser sessions are gone too; pair the UI again.

### Image retention

A generation runs its images by `repository@digest`. When `docker build -t
repository:dev` is repeated, the tag moves to the new image and the old image
keeps no name in that repository. The engine then cannot resolve
`repository@digest` for it, even if a tag in another repository still holds the
content. A restore or rollback that depends on the old image would fail after
everything was already stopped.

The CLI therefore gives every image that a restore or rollback may start a tag
inside its own repository, unique to its full digest:

```text
repository:lensdock-keep-<64 hex digits of the digest>
```

This is done before anything is stopped. The CLI inspects all images first and
fails the operation before writing any tag if an image is missing, has another
digest, is built for another platform, or if its keep tag already names a
different image. After tagging it checks that both the original reference and the
keep tag resolve to exactly the pinned digest and platform. If a running image has
already lost its name, the CLI re-tags it from a local image that carries exactly
the pinned digest and platform (nothing is pulled and no mutable tag is trusted).
`update plan` only reports whether this is possible; `apply` creates the tags
during step 1, and the switch also keeps the images of the new generation.
Rollback keeps both the images of the checkpoint's previous generation and the
images that are running now before it stops anything.

`lensdock update` never removes these tags or any other image. Do not delete
`lensdock-keep-*` tags, and do not prune images, while a checkpoint that
references them is retained.

### Clean stop

A checkpoint is only labeled clean when the stop was clean. Before the Gateway
and Runtime stop, the Gateway drains: it refuses new event subscriptions and
WebSocket upgrades, cancels the live SSE streams and the upgraded VNC and CDP
connections, waits for their handlers to return, lets ordinary requests finish,
and only then closes the database. Event subscribers read a normal end of
stream. A live subscription or an open VNC session therefore does not hold the
Gateway until its stop grace expires. This matters for the update because step 5
also checks the exit codes: a Gateway or Runtime that exited non-zero (137 means
it was killed after the grace period) turns the checkpoint into a crash
snapshot, and the update stops.

The Runtime closes Chromium through `Browser.close` before it signals the
process (it waits up to 3 seconds for the request and the exit, then falls back
to signals), because the SIGTERM path exits without removing the profile locks.
The update then requires all of:

- Runtime and Gateway exit code 0;
- no top-level `Singleton*` entries left in the profile volume;
- `profile.exit_type` of `Normal` or `SessionEnded` in `Default/Preferences`;
- no unflushed SQLite journal. A nonempty rollback journal counts only when its
  8-byte header is not the all-zero header written by `journal_mode=PERSIST`; a
  committed persistent journal is expected after a clean close and is kept as it
  is. The CLI never deletes profile journals. Browser WAL files may remain in the
  profile snapshot, while a Gateway database WAL must have been checkpointed.

If a check fails, the previous services are restored and nothing is switched.
`--accept-crash-snapshot` continues with a snapshot that is not clean. The
checkpoint is then marked as a crash checkpoint, and rolling back to it requires
`--allow-crash-checkpoint`. Use it only when you understand what was not
flushed.

## Reproducible success, failure and rollback acceptance

Use `tests/fixtures/handoff.html` as the Profile fixture. Before applying an
update, set marker `pre-<run-id>`; its page writes and reads a persistent Cookie,
Local Storage and IndexedDB. Create one feedback row and a small published
artifact with the same marker through the real Gateway API. Record their IDs
and hashes. This proves Profile, Gateway DB and retained artifact behavior
without any real account.

A deterministic failed candidate uses a derived image whose entrypoint exits:

```dockerfile
FROM lensdock/runtime:dev
ENTRYPOINT ["/bin/false"]
```

Build it locally, generate a dev release for its exact digest, plan with
`--allow-dev`, then apply. Candidate readiness fails before the switch. Expected
result:

1. the previous Runtime/Gateway restart on their original volumes;
2. the encrypted checkpoint remains;
3. the failed candidate branch remains visible in `update branches list` for
   diagnosis (a failure while copying removes only the incomplete candidate);
4. `handoff.html` still reads the pre-update Cookie/Local Storage/IndexedDB and
   the Gateway still reads the pre-update row/artifact.

This image is a deliberate acceptance fixture only. It must never be signed or
published as a release. Tag it under a name of its own and keep the normal
`lensdock/runtime:dev` image for the successful runs.

For success, use the real candidate and approve the switch. Verify the same
Profile marker and pre-update Gateway row/artifact. Then set marker
`post-<run-id>` and create a second Gateway row/artifact. Roll back with the
checkpoint printed by apply:

```bash
bin/lensdock update rollback \
  --deployment "$LENSDOCK_DEPLOYMENT" \
  --checkpoint CHECKPOINT_ID --dry-run
bin/lensdock update rollback \
  --deployment "$LENSDOCK_DEPLOYMENT" \
  --checkpoint CHECKPOINT_ID --yes
```

The rollback decrypts the checkpoint into **new** volumes, compares
fingerprints, then starts the paired old Runtime/Gateway images with the
pre-update Profile/keyring/DB. Expected evidence:

- `pre-<run-id>` exists in Cookie, Local Storage and IndexedDB;
- `post-<run-id>` is absent from restored Profile and Gateway DB (the stated
  RPO is the checkpoint time);
- both feedback artifacts remain, because artifacts are retained rather than
  rolled back, while the post-update feedback row is gone from the restored
  database and its API read returns 404;
- the update-generation volumes containing `post-<run-id>` remain listed as a
  retired branch and can be inspected/recovered; the old image never opens
  them;
- Runtime generation changes and all users/agents acquire new leases, and the
  browser sessions of the Gateway must be paired again.

### Database schema 2 and its paired rollback

Gateway database migration 1 is untouched. The forward migration 2 lets the
Gateway store a standalone drawing capture with no Runtime, target or
navigation, and checks that such a capture carries no runtime context. The
release manifest records `--gateway-db 2 --min-gateway-db 1`.

On the way forward, the migration runs first on the clone made from the
checkpoint, before any candidate boots. The live database moves to schema 2 only
at the switch.

On the way back, the checkpoint holds the schema 1 database. Rollback restores
that database and starts the old Gateway binary against it. It never starts an
old Gateway binary against the schema 2 database, and the plan refuses a release
whose Gateway would read a newer schema than it knows unless `--allow-downgrade`
is given (the update still uses a paired checkpoint). Expected evidence after
rolling back from schema 2:

- the old Gateway reports schema 1 and the previously published feedback reads
  back identical;
- reading the drawing capture created after the upgrade returns 404, because it
  was not part of the restored schema 1 database;
- to move forward again, create a new plan for the same release with a new
  `--output` name and apply it.

### Recovery

`status` shows every operation, the pending plan and the recovery text of a
stopped operation:

```bash
bin/lensdock update status --deployment "$LENSDOCK_DEPLOYMENT"
bin/lensdock update checkpoints list --deployment "$LENSDOCK_DEPLOYMENT"
bin/lensdock update branches list --deployment "$LENSDOCK_DEPLOYMENT"
```

| Journal status | Meaning | Operator action |
|---|---|---|
| `awaiting-approval` | Candidate verified, old services still stopped for maintenance | `update approve --update ID --yes`, or `update abort --update ID` (add `--keep-candidate` to keep the candidate volumes) |
| `aborted` | Stopped before the switch; previous services run again on their own volumes | Fix the cause and plan again. The candidate volumes of a failed candidate are kept for diagnosis |
| `failed-reverted` | The switch failed; the previous generation runs again | Same as `aborted`; inspect the journal for the cause |
| `failed-held` | Services are stopped or in an unknown state | Read the recovery text in `status`, fix the cause, then run `update abort --update ID` again. The checkpoint and every volume are intact |
| `completed` | Update or rollback finished | Roll back with `update rollback --checkpoint ID` if the new generation misbehaves |

A CLI process killed mid-operation, or a host restart, leaves its journal in
place, and `abort` rebuilds the run from the journal. For a held rollback, start
the running generation with `update compose -- up -d --wait`, fix the cause and
retry the rollback.

Do not work around these states. Do not start the stack with a raw `docker
compose` call, delete volumes or journals by hand, retry with
`--accept-crash-snapshot`, or point the Gateway at a database it did not
migrate. `branches delete` refuses current, in-flight and foreign volumes.

List/verify backups and branches, then prune only unreferenced checkpoints:

```bash
bin/lensdock update checkpoints list --deployment "$LENSDOCK_DEPLOYMENT"
bin/lensdock update checkpoints verify CHECKPOINT_ID --deployment "$LENSDOCK_DEPLOYMENT"
bin/lensdock update checkpoints prune --deployment "$LENSDOCK_DEPLOYMENT"
bin/lensdock update branches list --deployment "$LENSDOCK_DEPLOYMENT"
# Explicit only; refuses current/in-flight/foreign volumes:
bin/lensdock update branches delete VOLUME --deployment "$LENSDOCK_DEPLOYMENT" --yes
```

`deployment-status.json` contains only release/update/checkpoint summaries. It
never contains tokens, certificate/key paths, host paths, Docker volume names
or Profile/DB content. The Gateway serves this file read-only; Web/UI code
cannot apply or roll back an update. `status --publish` rewrites it atomically.

## Exercised evidence and limits

The 2026-10-25 run used a local macOS arm64 host with OrbStack, the deployment
`.lensdock/deployment.json`, the Gateway at `http://127.0.0.1:18081`, and
development releases such as `local-final-demo-8` and `local-final-demo-11`. The
last one ran as generation 8 on Gateway schema 2 (`feedback` 2, `storage` 1 in
the evidence files).
Evidence is kept in `.lensdock/evidence/final-runtime/`, a private directory
excluded from version control. It also contains private lease files from the
integration runs, so never publish the directory. This document names evidence
files and results only.

| Scenario | Result | File |
|---|---|---|
| Upgrade from schema 1 to 2 (`local-final-demo-8`) | A standalone 640x360 drawing capture was created afterward (`source: drawing`, `provenance: generated-canvas`, empty `runtimeGeneration`, null `targetId` and `navigationId`); the previously published feedback was unchanged | `db2-upgrade-drawing.json` |
| Paired rollback from schema 2 | Old Gateway on schema 1 (`feedback` 1) answered ready; reading the drawing capture created after the upgrade returned 404; the previously published feedback was unchanged; a live SSE stream opened before the rollback ended cleanly | `db2-paired-rollback.json` |
| Re-apply `local-final-demo-8` after the rollback | Applied with a new plan | `plans/local-final-demo-8-reapply.json` (state directory) |
| Update with Profile markers, then paired rollback | `pre` Cookie, Local Storage and IndexedDB restored; `post` additions absent; original published feedback unchanged; the post-update feedback read returned 404; retained artifact files kept their SHA-256 | `update-six-pre-profile.json`, `update-six-post-profile.json`, `paired-rollback.json`, `rollback-retained-artifacts.json` |
| Failed candidate (`local-final-failure-9`, Runtime entrypoint `/bin/false`) with a live VNC session and SSE stream | The failed update left the healthy `local-final-demo-8` on schema 2 running; ready answered 200; previously published feedback unchanged; the browser was repaired and connected | `candidate-failure-recovery.json` |
| `local-final-demo-10` and `local-final-demo-11` | Both applied and passed all 8 candidate checks | journals in the state directory |
| Image retention with disposable tags in the real Docker (containerd) store | After the original tag moved, the image stayed resolvable by digest and platform (`linux/arm64`) through the same-repository tag; without that tag the check was rejected; recovery from a cross-repository tag restored the same digest | `image-retention-smoke.json` |
| Clean Runtime stop | `profile.exit_type` `Normal`, no lock files | `normal-shutdown.json` |
| Enroll the runtime policy volume (`local-toolbar-modes-16`, generation 13) | The read-only plan accepted the new retained dataset; the normal candidate and switch completed with existing Profile and Gateway DB mappings preserved | Update journal `u-20261025T070146Z-d54070` |
| Update while the live Agent is paused (`local-toolbar-modes-17`, generation 14) | Candidate checks succeeded with its separate AGENT policy; production remained AGENT, paused, revision 6 with its original policy timestamp | `mode17-update-persistence.json`, update journal `u-20261025T071829Z-b21d5a` |

Container boundary snapshot (`container-boundaries.json`): neither the Runtime nor
the Gateway container is privileged or has a Docker socket mount. Both use
`cap_drop: ALL` and `no-new-privileges`, and add back a short list (Runtime:
CHOWN, KILL, SETGID, SETPCAP, SETUID; Gateway: CHOWN, SETGID, SETPCAP, SETUID).
The Runtime publishes no port and mounts its credentials read-only, and the
Gateway publishes only `127.0.0.1:18081`, mounts `/data/artifacts`, `/data/db`
and `/data/private` writable, and mounts `/run/lensdock` and the status
directory read-only.

Limits:

- These are development releases, unsigned and without BuildKit provenance or
  scanner output. The signed release channel and `tools/release/*.sh` were not
  run in this pass.
- The integrated product was exercised on macOS arm64 with OrbStack only. The
  M0 evidence on amd64 applies to M0 alone; there is no integrated amd64 run.
- No real account was used. Profile proof uses `handoff.html` markers.
- No exercised release changed the Chromium version: the plans for
  `local-final-demo-8`, `local-final-demo-10`, `local-final-demo-11` and
  `local-update-demo-5` all record `156.0.8078.47-1~deb13u1`. A rollback after a
  Chromium upgrade has therefore not been demonstrated, although the Profile is
  restored from the checkpoint in either case.
- The only injected failure was an unreadiness failure of the candidate Runtime.
  Disk full, an interrupted download, a failed migration on the clone, a host
  restart during an update and candidate network leaks were not injected. No
  evidence for them is recorded here.
- Runtime mutual-TLS certificates written by product init expire after 365 days;
  no rotation procedure is documented or exercised.
- Browser sessions in the Gateway are process-local. Every Gateway restart
  requires pairing again.
