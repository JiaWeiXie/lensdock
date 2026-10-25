// Package e2e holds the LensDock M0 end-to-end tests (SPIKE-01..04).
//
// Every test file carries the `e2e` build tag, so a plain `go test ./...` never
// touches a runtime. Build the test binary with
//
//	go test -c -tags e2e -o bin/e2e.test ./tests/e2e
//
// and run it through the scripts in this directory, which export the
// LENSDOCK_E2E_* variables documented in tests/e2e/internal/harness/env.go.
//
// There are two kinds of tests:
//
//   - Host tests (TestRT02, TestRT03, TestCT03, TestCT10) talk to the published API
//     at 127.0.0.1:18080 and use `docker exec`. Run them with run-host.sh.
//   - Sidecar tests (TestCT04, TestTX01..03, TestSpike02*, TestSpike03*,
//     TestRT03Viewers) need the loopback-only VNC (5900/5901) and CDP ports. They
//     run inside a throwaway container that joins the runtime container's network
//     namespace. That pattern is test-only; run them with run-sidecar.sh.
//
// The reusable pieces (RFB client, CDP client, API/evidence/docker helpers) live
// in internal packages with their own fake-server unit tests that run under a
// plain `go test ./...`.
package e2e
