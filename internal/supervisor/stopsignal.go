package supervisor

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// capKill is CAP_KILL (linux/capability.h).
const capKill = 5

// pid1StatusPath is the container init's status; the runtime reads it once at
// startup only.
const pid1StatusPath = "/proc/1/status"

// ErrStopSignalUnreachable reports a deployment that cannot shut the container
// down gracefully.
var ErrStopSignalUnreachable = errors.New("supervisor: the container init cannot signal this runtime")

// CheckStopSignalPath verifies that the container's PID 1 can deliver its stop
// signal to this process. Tini (PID 1) forwards SIGTERM with kill(2). Once the
// runtime has dropped to its unprivileged uid, kill(2) needs CAP_KILL in the
// container's user namespace, because the sender's uid no longer matches the
// target's. With cap_drop: [ALL] and no KILL, tini fails with EPERM, logs a
// FATAL and exits; the container is then SIGKILLed, so Chromium never runs its
// shutdown and the profile is left marked as crashed. This check only reports
// the misconfiguration at startup; signal handling itself is unchanged.
func CheckStopSignalPath() error { return checkStopSignalPath(pid1StatusPath, os.Geteuid()) }

// checkStopSignalPath is CheckStopSignalPath with an injectable status path
// and target uid, for tests.
func checkStopSignalPath(path string, targetUID int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: read %s: %v", ErrStopSignalUnreachable, path, err)
	}
	return stopSignalWarning(data, targetUID)
}

// stopSignalWarning reports the EPERM case from a /proc/<pid>/status body.
func stopSignalWarning(status []byte, targetUID int) error {
	uid, capEff, ok := parseInitStatus(status)
	if !ok {
		return fmt.Errorf("%w: /proc/1/status has no Uid/CapEff", ErrStopSignalUnreachable)
	}
	if uid == targetUID || capEff&(1<<capKill) != 0 {
		return nil
	}
	return fmt.Errorf("%w: PID 1 runs as uid %d and lacks CAP_KILL, so it cannot send SIGTERM to uid %d; add KILL to the runtime's cap_add (keep cap_drop: ALL)",
		ErrStopSignalUnreachable, uid, targetUID)
}

// parseInitStatus extracts the real uid and the effective capability mask.
func parseInitStatus(status []byte) (uid int, capEff uint64, ok bool) {
	var haveUID, haveCaps bool
	for _, line := range strings.Split(string(status), "\n") {
		switch {
		case strings.HasPrefix(line, "Uid:"):
			fields := strings.Fields(strings.TrimPrefix(line, "Uid:"))
			if len(fields) == 0 {
				return 0, 0, false
			}
			n, err := strconv.Atoi(fields[0])
			if err != nil {
				return 0, 0, false
			}
			uid, haveUID = n, true
		case strings.HasPrefix(line, "CapEff:"):
			v, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), 16, 64)
			if err != nil {
				return 0, 0, false
			}
			capEff, haveCaps = v, true
		}
	}
	return uid, capEff, haveUID && haveCaps
}
