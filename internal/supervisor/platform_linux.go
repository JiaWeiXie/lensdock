//go:build linux

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	preflightTimeout = 15 * time.Second
	pidfdFD          = 3 // fd the signal helper receives the pidfd on
)

// childSysProcAttr runs a child as the browser user with no supplementary
// groups, in its own process group, killed when the supervisor dies.
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
		Credential: &syscall.Credential{
			Uid:    browserUID,
			Gid:    browserGID,
			Groups: []uint32{},
		},
	}
}

// startProcess starts cmd as the browser user and returns a pidfd for it.
func startProcess(cmd *exec.Cmd) (*os.File, error) {
	cmd.SysProcAttr = childSysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		return nil, fmt.Errorf("pidfd_open: %w", err)
	}
	return os.NewFile(uintptr(fd), "pidfd"), nil
}

// runHelper re-executes the runtime binary as the browser user with a hidden
// subcommand.
func runHelper(ctx context.Context, extra []*os.File, args ...string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate runtime binary: %w", err)
	}
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Env = childEnv()
	cmd.Dir = "/"
	cmd.ExtraFiles = extra
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig:  syscall.SIGKILL,
		Credential: &syscall.Credential{Uid: browserUID, Gid: browserGID, Groups: []uint32{}},
	}
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("helper %s: %w: %s", args[0], err, strings.TrimSpace(truncate(string(out), 512)))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// runSignalHelper delivers sig as the browser user: to the process behind
// pidfd, or to process group pgid when pgid > 0.
func runSignalHelper(ctx context.Context, pidfd *os.File, sig syscall.Signal, pgid int) error {
	args := []string{signalHelperName, strconv.Itoa(int(sig))}
	var extra []*os.File
	if pgid > 0 {
		args = append(args, strconv.Itoa(pgid))
	} else {
		extra = []*os.File{pidfd}
	}
	return runHelper(ctx, extra, args...)
}

// RunSignalHelper implements the hidden `__signal` subcommand; args are the
// arguments after the subcommand name. It must run as the browser user.
// With one argument it signals the process behind inherited fd 3 (a pidfd);
// with two it signals the given process group. A group with no processes
// is not an error.
func RunSignalHelper(args []string) error {
	sig, pgid, err := parseSignalArgs(args)
	if err != nil {
		return err
	}
	if pgid > 0 {
		if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("kill group %d: %w", pgid, err)
		}
		return nil
	}
	if err := unix.PidfdSendSignal(pidfdFD, unix.Signal(sig), nil, 0); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("pidfd_send_signal: %w", err)
	}
	return nil
}

// RunUsernsPreflight implements the hidden `__preflight-userns` subcommand:
// as the browser user it must be able to clone /bin/true into fresh user,
// PID and network namespaces, which is what Chromium's namespace sandbox does.
func RunUsernsPreflight(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: __preflight-userns")
	}
	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}},
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clone with CLONE_NEWUSER|CLONE_NEWPID|CLONE_NEWNET failed: %w", err)
	}
	return nil
}

// Preflight verifies the host supports the user-namespace sandbox. A failure
// must keep Chromium from starting; there is no weaker fallback.
func Preflight(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	if err := runHelper(ctx, nil, "__preflight-userns"); err != nil {
		return fmt.Errorf("userns sandbox preflight failed: %w", err)
	}
	return nil
}

// PrepareRoot runs as root before DropPrivileges. It creates the runtime
// directories with their fixed owners and modes, writes the Xauthority file
// for the browser user, and verifies the persistent data directories. It
// needs only CAP_CHOWN beyond root's file ownership: directories that must end
// up browser-owned are created root-owned, populated, then handed over.
func PrepareRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("prepare root: must run as root")
	}
	for _, dir := range []string{profileDir, downloadsDir} {
		if err := VerifyDataDir(dir); err != nil {
			return fmt.Errorf("prepare root: %w", err)
		}
	}

	if err := ensureDir(runDir, 0o755, 0, 0); err != nil {
		return err
	}
	if err := ensureDir(browserRunDir, 0o700, 0, 0); err != nil {
		return err
	}
	if err := clearDir(browserRunDir); err != nil {
		return err
	}
	if err := WriteXauthority(xauthorityPath, browserUID, browserGID); err != nil {
		return fmt.Errorf("prepare root: %w", err)
	}
	// 0710 browser:supervisor-group: the supervisor may traverse to known
	// paths (Xauthority existence check, dbus gate dial) but cannot list the
	// directory or read the 0600 Xauthority cookie. chmod precedes chown.
	if err := os.Chmod(browserRunDir, 0o710); err != nil {
		return fmt.Errorf("prepare root: chmod %s: %w", browserRunDir, err)
	}
	if err := os.Lchown(browserRunDir, browserUID, supervisorGID); err != nil {
		return fmt.Errorf("prepare root: chown %s: %w", browserRunDir, err)
	}

	if err := ensureDir(filepath.Dir(xdgRuntimeDir), 0o755, 0, 0); err != nil {
		return err
	}
	if err := ensureDir(xdgRuntimeDir, 0o700, browserUID, browserGID); err != nil {
		return err
	}
	if err := ensureDir(x11SocketDir, fs.ModeSticky|0o777, 0, 0); err != nil {
		return err
	}
	return nil
}

// PreparePolicyDir runs as root before DropPrivileges. dir must already exist
// (it is the mount point of the persistent policy volume): a missing
// directory is an error, never created on the read-only root. A fresh volume
// is root-owned; it is handed to the supervisor with mode 0700. A directory
// that already belongs to the supervisor with mode 0700 is accepted without
// any change (root has no CAP_FOWNER, and a read-only mount must keep
// working). Anything else is refused, so the browser user can never reach
// the policy.
func PreparePolicyDir(dir string) error {
	if os.Geteuid() != 0 {
		return errors.New("prepare policy dir: must run as root")
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("prepare policy dir: %w (mount a persistent volume there)", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("prepare policy dir: %s is not a directory", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("prepare policy dir: cannot read ownership of %s", dir)
	}
	if st.Uid == 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("prepare policy dir: chmod %s: %w", dir, err)
		}
		if err := os.Lchown(dir, supervisorUID, supervisorGID); err != nil {
			return fmt.Errorf("prepare policy dir: chown %s: %w", dir, err)
		}
	}
	if err := verifyDir(dir, supervisorUID); err != nil {
		return fmt.Errorf("prepare policy dir: %w", err)
	}
	return nil
}

// ensureDir creates path (if needed) and forces the exact owner and mode. It
// refuses symlinks and non-directories.
func ensureDir(path string, mode fs.FileMode, uid, gid int) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("prepare root: mkdir %s: %w", path, err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("prepare root: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("prepare root: %s is not a directory", path)
	}
	// chmod first: once the directory belongs to another UID, root needs
	// CAP_FOWNER (not granted) to change its mode.
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("prepare root: chmod %s: %w", path, err)
	}
	if err := os.Lchown(path, uid, gid); err != nil {
		return fmt.Errorf("prepare root: chown %s: %w", path, err)
	}
	return nil
}

// clearDir removes stale files (previous sockets, Xauthority) from dir.
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("prepare root: %w", err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("prepare root: remove stale %s: %w", e.Name(), err)
		}
	}
	return nil
}

// DropPrivileges moves the supervisor from root to UID/GID 10000 keeping only
// CAP_SETUID and CAP_SETGID (needed to start children as the browser user).
//
// Order: KEEPCAPS; shrink the bounding set to {SETUID, SETGID}; clear
// supplementary groups; setresgid/setresuid; capset effective = permitted =
// {SETUID, SETGID}, inheritable and ambient empty; set NO_NEW_PRIVS and clear
// dumpable. Everything is applied to all threads via
// syscall.AllThreadsSyscall, which requires CGO_ENABLED=0. The result is
// verified against /proc/self/status.
func DropPrivileges() error {
	if os.Geteuid() != 0 {
		return errors.New("drop privileges: must start as root")
	}
	keep := keptCaps

	if err := allThreadsPrctl(unix.PR_SET_KEEPCAPS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("drop privileges: keepcaps: %w", err)
	}
	last, err := lastCap()
	if err != nil {
		return fmt.Errorf("drop privileges: %w", err)
	}
	for c := 0; c <= last; c++ {
		if keep&(1<<uint(c)) != 0 {
			continue
		}
		if err := allThreadsPrctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil {
			return fmt.Errorf("drop privileges: drop bounding cap %d: %w", c, err)
		}
	}
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("drop privileges: setgroups: %w", err)
	}
	if err := syscall.Setresgid(supervisorGID, supervisorGID, supervisorGID); err != nil {
		return fmt.Errorf("drop privileges: setresgid: %w", err)
	}
	if err := syscall.Setresuid(supervisorUID, supervisorUID, supervisorUID); err != nil {
		return fmt.Errorf("drop privileges: setresuid: %w", err)
	}
	if err := allThreadsCapset(keep); err != nil {
		return fmt.Errorf("drop privileges: capset: %w", err)
	}
	if err := allThreadsPrctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("drop privileges: clear ambient caps: %w", err)
	}
	if err := allThreadsPrctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("drop privileges: no_new_privs: %w", err)
	}
	if err := allThreadsPrctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("drop privileges: dumpable: %w", err)
	}
	if v, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0); err != nil || v != 0 {
		return fmt.Errorf("drop privileges: process is still dumpable (%d, %v)", v, err)
	}

	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return fmt.Errorf("drop privileges: verify: %w", err)
	}
	st, err := parseProcStatus(data)
	if err != nil {
		return fmt.Errorf("drop privileges: verify: %w", err)
	}
	return verifyDropped(st)
}

func allThreadsPrctl(option int, a2, a3, a4, a5 uintptr) error {
	_, _, errno := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, uintptr(option), a2, a3, a4, a5, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// allThreadsCapset sets effective = permitted = mask and inheritable = empty
// on every thread.
func allThreadsCapset(mask uint64) error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{
		{Effective: uint32(mask), Permitted: uint32(mask)},
		{Effective: uint32(mask >> 32), Permitted: uint32(mask >> 32)},
	}
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func lastCap() (int, error) {
	data, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 || n > 63 {
		return 0, fmt.Errorf("unexpected cap_last_cap %q", strings.TrimSpace(string(data)))
	}
	return n, nil
}
