//go:build !linux

package main

import (
	"errors"
	"os"
)

func dropPrivileges() error {
	if os.Geteuid() == 0 {
		return errors.New("run the host development Gateway as an unprivileged user")
	}
	return nil
}
