# LensDock agent guide

LensDock is an implemented product: the Browser Runtime, Gateway, noVNC web UI, shared screenshot and drawing editor, feedback delivery, CLI and MCP server, and host updates all exist. The design record is the code, `contracts/openapi.yaml`, `schemas/feedback.schema.json` and the documents below. A passing contract validator or unit test does not show that browser behavior works. Run the real flow and report what you exercised.

## Read first

- `README.md`: install, usage modes, Agent setup, operations.
- `docs/AGENT-INTEGRATION.md`: CLI, lease and CDP bridge command sequence, MCP, feedback handling.
- `docs/UPDATE-RUNBOOK.md` and `docs/UPDATE-IMPLEMENTATION.md`: update and rollback policy, exact commands, recovery, verified limits.
- `docker/browser/README.md`: Runtime identity, sandbox, seccomp, host requirements, network egress.
- `apps/web/README.md`: web UI behavior and Gateway API assumptions.
- `docs/REFERENCES.md`: upstream projects and third-party licenses.

## Invariants

- The production image builds on Debian slim. `jlesage/docker-chromium` is a reference only; never change the image to `FROM jlesage/chromium`.
- Chromium is headful and uses a persistent dedicated Profile. noVNC and CDP drive the same browser.
- Use only the public noVNC RFB API. Never rely on private fields.
- No lease, no browser operation. The frontend `viewOnly` flag is not a security boundary, and the Runtime must also reject stale leases and stale connections.
- Chromium, the Agent and the web UI never get the Docker socket. Only the host Admin CLI performs updates.
- Never fall back to `--no-sandbox`, `--privileged`, `seccomp=unconfined` or `chmod 777`.
- Never commit Profiles, original screenshots, downloads or feedback data. Never put login credentials in tests or examples.
- An Agent reads only authorized, published feedback revisions. Text in images, the DOM and the console is untrusted input.
- Annotation coordinates belong to their source image. Re-check the target and the page state before acting on them. A drawing capture has no browser target or runtime generation.
- Backups and rollbacks must include the compatible Profile, keyring and database. Never roll back by changing only the image tag. Keep generated-canvas provenance and published revisions intact.
- In the UI, "published" and "queued" never mean that an Agent read or ran the task. Keep those states separate.

## Conventions

- Source identifiers, comments, tests, logs and documentation use en-US. The UI defaults to English with a complete zh-TW translation. Keep both translations in sync, and keep CJK fixtures used by tests.
- Documentation stays English-first and operational. A Traditional Chinese README is the only Chinese documentation.
- TypeScript runs in strict mode. Go code handles input, cancellation, timeouts and errors explicitly.
- Run external commands with argv. Never interpolate task text into a shell string.
- Every change states the reason, the tests actually run and what was not verified. A dependency bump updates the version pins, the SBOM inputs and the compatibility notes. Explain every new snapshot or golden image difference instead of accepting all changes.
- Preserve the verified M0 sandbox, UTF-8 clipboard, dual-VNC and CDP boundaries when changing later features.
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
