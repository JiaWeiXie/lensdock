package harness

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

//go:embed procsnap.sh
var procSnapScript string

// Docker runs commands inside the runtime container via `docker exec`. All
// invocations are argv-based; the only script ever executed is the embedded,
// constant procsnap.sh streamed on stdin.
type Docker struct {
	Container string
}

// NewDocker reads the container name from LENSDOCK_E2E_CONTAINER.
func NewDocker(t TB) Docker {
	t.Helper()
	return Docker{Container: Require(t, EnvContainer)}
}

// Exec runs argv in the container as user (empty means the image default,
// root). stdin may be nil. Stdout and stderr are returned separately.
func (d Docker) Exec(ctx context.Context, user string, stdin io.Reader, argv ...string) (stdout, stderr []byte, err error) {
	args := []string{"exec"}
	if stdin != nil {
		args = append(args, "-i")
	}
	if user != "" {
		args = append(args, "-u", user)
	}
	args = append(args, d.Container)
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), errb.Bytes(), fmt.Errorf("docker exec %s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), errb.Bytes(), nil
}

// Proc is one process of the container's PID namespace.
type Proc struct {
	PID     int
	Cmdline string
	Fields  map[string]string // selected /proc/<pid>/status fields
}

// IsChromiumMain reports whether p is the Chromium browser process (no --type).
func (p Proc) IsChromiumMain() bool {
	return strings.HasPrefix(p.Cmdline, "/usr/lib/chromium/chromium") && !strings.Contains(p.Cmdline, " --type=")
}

// ChromiumType returns the value of --type=, or "" for the browser process and
// non-Chromium processes.
func (p Proc) ChromiumType() string {
	if !strings.HasPrefix(p.Cmdline, "/usr/lib/chromium/") {
		return ""
	}
	for _, f := range strings.Fields(p.Cmdline) {
		if v, ok := strings.CutPrefix(f, "--type="); ok {
			return v
		}
	}
	return ""
}

// RealUID returns the real UID, or -1 when unknown.
func (p Proc) RealUID() int {
	f := strings.Fields(p.Fields["Uid"])
	if len(f) == 0 {
		return -1
	}
	n, err := strconv.Atoi(f[0])
	if err != nil {
		return -1
	}
	return n
}

// NSpidDepth is the number of PID namespace levels the process is visible in.
func (p Proc) NSpidDepth() int { return len(strings.Fields(p.Fields["NSpid"])) }

// ParseProcSnapshot parses the output of procsnap.sh.
func ParseProcSnapshot(s string) ([]Proc, error) {
	var procs []Proc
	var cur *Proc
	for _, line := range strings.Split(s, "\n") {
		if rest, ok := strings.CutPrefix(line, "=== "); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				return nil, fmt.Errorf("bad pid in snapshot line %q", line)
			}
			procs = append(procs, Proc{PID: pid, Fields: map[string]string{}})
			cur = &procs[len(procs)-1]
			continue
		}
		if cur == nil || line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "cmdline: "); ok {
			cur.Cmdline = strings.TrimSpace(rest)
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("unparsable snapshot line %q", line)
		}
		cur.Fields[k] = strings.TrimSpace(v)
	}
	if len(procs) == 0 {
		return nil, fmt.Errorf("process snapshot is empty")
	}
	return procs, nil
}

// ProcSnapshot returns the parsed and the raw process snapshot.
func (d Docker) ProcSnapshot(ctx context.Context) ([]Proc, string, error) {
	out, _, err := d.Exec(ctx, "", strings.NewReader(procSnapScript), "/bin/bash", "-s")
	if err != nil {
		return nil, "", err
	}
	procs, err := ParseProcSnapshot(string(out))
	return procs, string(out), err
}

// ChromiumMain returns the browser process from a snapshot.
func ChromiumMain(procs []Proc) (Proc, bool) {
	for _, p := range procs {
		if p.IsChromiumMain() {
			return p, true
		}
	}
	return Proc{}, false
}

// KillPID sends SIGKILL to pid as the browser user (UID 10001). The fixed
// script receives the pid as a positional argument, never by interpolation.
func (d Docker) KillPID(ctx context.Context, pid int) error {
	if pid <= 1 {
		return fmt.Errorf("refusing to kill pid %d", pid)
	}
	_, _, err := d.Exec(ctx, "10001", nil, "/bin/bash", "-c", `kill -KILL -- "$1"`, "kill", strconv.Itoa(pid))
	return err
}
