//go:build linux

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func dropPrivileges() error {
	if os.Geteuid() != 0 {
		return nil
	}
	if err := unix.Setgroups([]int{}); err != nil {
		return err
	}
	if err := unix.Setresgid(10000, 10000, 10000); err != nil {
		return err
	}
	if err := unix.Setresuid(10000, 10000, 10000); err != nil {
		return err
	}
	if os.Getuid() != 10000 || os.Geteuid() != 10000 {
		return errors.New("Gateway privilege drop failed")
	}
	return nil
}
