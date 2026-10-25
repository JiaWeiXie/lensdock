package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/hostmanager"
)

func TestManagerKeepsDockerMulticallInvocationName(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "ldcli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	implementation := filepath.Join(base, "docker-tools")
	writeFile(t, implementation, `#!/bin/sh
case "$0" in */docker) ;; *) echo 'unsupported argv0' >&2; exit 127 ;; esac
if [ "$1" = context ] && [ "$2" = show ]; then
  printf 'local\n'
elif [ "$1" = context ] && [ "$2" = inspect ]; then
  printf '[{"Endpoints":{"docker":{"Host":"unix:///tmp/lensdock-fake-engine.sock"}}}]\n'
else
  printf '{"ID":"multicall-engine","MemTotal":25769803776,"NCPU":16,"OSType":"linux"}\n'
fi
`, 0700)
	invocation := filepath.Join(base, "docker")
	if err := os.Symlink(implementation, invocation); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", base)
	root := filepath.Join(base, "manager")
	result := runCLI(t, context.Background(), "", map[string]string{"LENSDOCK_MANAGER_DIR": root}, "admin", "manager", "init", "--bundle-dir", writeBundle(t, "1.0.0"))
	if result.code != 0 {
		t.Fatalf("initializing with multicall Docker: %s", result.stderr)
	}
	cfg, err := hostmanager.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := hostmanager.InspectEngine(context.Background(), cfg)
	if err != nil || inventory.EngineID != "multicall-engine" {
		t.Fatalf("fixed Docker invocation no longer works: %+v %v", inventory, err)
	}
}
