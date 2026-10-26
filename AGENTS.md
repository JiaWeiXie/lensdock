# LensDock agent guide

LensDock is an implemented product: the Browser Runtime, Gateway, noVNC web UI, shared screenshot and drawing editor, feedback delivery, CLI and MCP server, and host updates all exist. The design record is the code, `contracts/openapi.yaml`, `schemas/feedback.schema.json` and the documents below. A passing contract validator or unit test does not show that browser behavior works. Run the real flow and report what you exercised.

## Read first

- `README.md`: install, usage modes, Agent setup, operations.
- `docs/AGENT-INTEGRATION.md`: automatic control, full-browser desktop/CDP tools, MCP and feedback handling.
- `docs/UPDATE-RUNBOOK.md` and `docs/UPDATE-IMPLEMENTATION.md`: Host Admin operations, retention, scoped cleanup, recovery and verification.
- `docker/browser/README.md`: Runtime identity, seccomp, host requirements, network egress.
- `apps/web/README.md`: web UI behavior and Gateway API assumptions.
- `docs/REFERENCES.md`: upstream projects and third-party licenses.

## Product defaults

- Optimize for ordinary local use: automatic setup and control, no default product quotas or extra approval steps. Advanced restrictions are explicit settings.
- A user or an authorized Agent can operate the host Admin CLI. Workspace Agent credentials stay scoped to browser collaboration; they do not become Host Admin credentials.
- System-data expiry applies only to owned, unused artifacts. Protect active workspace data and recovery references. Keep uninstalling the executable separate from permanent data deletion.

## Runtime and data contracts

- Chromium is headful and uses a persistent dedicated Profile. noVNC and CDP drive the same browser.
- Use only the public noVNC RFB API. Never rely on private fields.
- Usual browser workflows acquire and renew control automatically. The Runtime must reject stale leases and connections; frontend `viewOnly` is not the enforcement mechanism.
- Never commit Profiles, original screenshots, downloads or feedback data. Never put login credentials in tests or examples.
- Backups and rollbacks must include the compatible Profile, keyring and database. Never roll back by changing only the image tag. Keep generated-canvas provenance and published revisions intact.
- Keep the primary feedback UI simple. Its underlying published, queued, acknowledged, executing and verified states must remain truthful.

## Conventions

- Source identifiers, comments, tests, logs and documentation use en-US. The UI defaults to English with a complete zh-TW translation. Keep both translations in sync, and keep CJK fixtures used by tests.
- Documentation stays English-first and operational. A Traditional Chinese README is the only Chinese documentation.
- TypeScript runs in strict mode. Go code handles input, cancellation, timeouts and errors explicitly.
- Run external commands with argv. Never interpolate task text into a shell string.
- Every change states the reason, the tests actually run and what was not verified. A dependency bump updates the version pins, the SBOM inputs and the compatibility notes. Explain every new snapshot or golden image difference instead of accepting all changes.
- To operate an initialized product, use `lensdock update compose --deployment FILE -- ...` so the current generation override stays in effect. Never rerun `product-init` as an update.

## Key commands

```bash
mise install
mise run check          # contract validator, gofmt, go vet, go test -race, shellcheck, pin check
mise run build          # host binaries into bin/
mise run web:build      # frontend install and build
mise run image:build    # Runtime image
mise run gateway:image  # Gateway image with the UI
```

The contract validator is `tools/validate_contracts.py`. It checks the feedback schema, OpenAPI structure and references, synthetic examples, annotation and coordinate rules, font configuration and the CJK fixture. It starts no container or browser. Frontend tests run with `pnpm test` in `apps/web`. The `e2e:*` mise tasks need a running Docker engine.
