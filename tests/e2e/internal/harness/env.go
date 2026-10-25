// Package harness holds the shared plumbing of the LensDock end-to-end tests:
// environment configuration, a raw runtime API client, the evidence writer,
// docker and lensdock-CLI runners and the fixture web server.
//
// Configuration comes from the environment and a missing required variable is
// a test failure, never a skip: a silently skipped e2e test looks like a pass.
package harness

import "os"

// TB is the subset of testing.TB the harness needs, so helpers can be tested
// with a fake.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
	Cleanup(func())
}

// Environment variable names read by the tests.
const (
	EnvAPI           = "LENSDOCK_E2E_API"                 // runtime API base URL, e.g. http://127.0.0.1:18080
	EnvOperatorToken = "LENSDOCK_E2E_OPERATOR_TOKEN_FILE" // file holding the ldo_ operator token
	EnvWorkspace     = "LENSDOCK_E2E_WORKSPACE"           // workspace id, default "default"
	EnvOut           = "LENSDOCK_E2E_OUT"                 // evidence directory
	EnvContainer     = "LENSDOCK_E2E_CONTAINER"           // runtime container name (host tests)
	EnvLensdockBin   = "LENSDOCK_E2E_LENSDOCK_BIN"        // path of the lensdock CLI for this OS
	EnvFixtures      = "LENSDOCK_E2E_FIXTURES"            // directory with the HTML fixtures (sidecar tests)
	EnvVNCHost       = "LENSDOCK_E2E_VNC_HOST"            // default 127.0.0.1 (shared netns in the sidecar)
)

// Require returns the value of an environment variable and fails the test when
// it is unset or empty.
func Require(t TB, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

// Default returns the value of an environment variable or def when it is empty.
func Default(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
