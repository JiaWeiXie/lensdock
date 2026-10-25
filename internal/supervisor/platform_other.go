//go:build !linux

package supervisor

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

func startProcess(*exec.Cmd) (*os.File, error) { return nil, ErrUnsupportedPlatform }

func runSignalHelper(context.Context, *os.File, syscall.Signal, int) error {
	return ErrUnsupportedPlatform
}

// RunSignalHelper is the hidden `__signal` subcommand (linux only).
func RunSignalHelper([]string) error { return ErrUnsupportedPlatform }

// RunUsernsPreflight is the hidden `__preflight-userns` subcommand (linux only).
func RunUsernsPreflight([]string) error { return ErrUnsupportedPlatform }

// Preflight verifies sandbox host support (linux only).
func Preflight(context.Context) error { return ErrUnsupportedPlatform }

// PrepareRoot creates runtime directories as root (linux only).
func PrepareRoot() error { return ErrUnsupportedPlatform }

// PreparePolicyDir prepares the policy directory as root (linux only).
func PreparePolicyDir(string) error { return ErrUnsupportedPlatform }

// DropPrivileges drops root privileges (linux only).
func DropPrivileges() error { return ErrUnsupportedPlatform }
