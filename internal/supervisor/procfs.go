package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Capability numbers (linux/capability.h).
const (
	capSetgid = 6
	capSetuid = 7
)

// keptCaps is the supervisor's permitted and effective set after the drop:
// just enough to start children as the browser user.
const keptCaps = uint64(1<<capSetuid | 1<<capSetgid)

// procStatus holds the /proc/<pid>/status fields the supervisor relies on.
type procStatus struct {
	UIDs       []uint32 // real, effective, saved, filesystem
	GIDs       []uint32
	Groups     []uint32
	NoNewPrivs int
	Seccomp    int // -1 when the field is absent
	// SeccompFilters is the number of attached seccomp filters (Linux 5.9+);
	// -1 when the field is absent.
	SeccompFilters int
	NSpid          []int
	CapInh         uint64
	CapPrm         uint64
	CapEff         uint64
	CapBnd         uint64
	CapAmb         uint64
}

func parseProcStatus(data []byte) (procStatus, error) {
	st := procStatus{Seccomp: -1, SeccompFilters: -1}
	var parseErr error
	u32s := func(fields []string) []uint32 {
		out := make([]uint32, 0, len(fields))
		for _, f := range fields {
			n, err := strconv.ParseUint(f, 10, 32)
			if err != nil && parseErr == nil {
				parseErr = err
			}
			out = append(out, uint32(n))
		}
		return out
	}
	hex := func(fields []string) uint64 {
		if len(fields) != 1 {
			return 0
		}
		n, err := strconv.ParseUint(fields[0], 16, 64)
		if err != nil && parseErr == nil {
			parseErr = err
		}
		return n
	}
	num := func(fields []string) int {
		if len(fields) != 1 {
			return 0
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil && parseErr == nil {
			parseErr = err
		}
		return n
	}

	seen := false
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		seen = true
		switch key {
		case "Uid":
			st.UIDs = u32s(fields)
		case "Gid":
			st.GIDs = u32s(fields)
		case "Groups":
			st.Groups = u32s(fields)
		case "NoNewPrivs":
			st.NoNewPrivs = num(fields)
		case "Seccomp":
			st.Seccomp = num(fields)
		case "Seccomp_filters":
			st.SeccompFilters = num(fields)
		case "NSpid":
			for _, f := range fields {
				n, err := strconv.Atoi(f)
				if err != nil && parseErr == nil {
					parseErr = err
				}
				st.NSpid = append(st.NSpid, n)
			}
		case "CapInh":
			st.CapInh = hex(fields)
		case "CapPrm":
			st.CapPrm = hex(fields)
		case "CapEff":
			st.CapEff = hex(fields)
		case "CapBnd":
			st.CapBnd = hex(fields)
		case "CapAmb":
			st.CapAmb = hex(fields)
		}
	}
	if !seen {
		return procStatus{}, errors.New("proc status: no fields")
	}
	if parseErr != nil {
		return procStatus{}, fmt.Errorf("proc status: %w", parseErr)
	}
	return st, nil
}

// procStat holds the /proc/<pid>/stat fields the supervisor relies on.
type procStat struct {
	PID      int
	Comm     string
	PPID     int
	PGRP     int
	UTime    uint64 // clock ticks
	STime    uint64 // clock ticks
	RSSPages int64
}

// parseProcStat parses /proc/<pid>/stat. The command name is parenthesized and
// may itself contain spaces and parentheses, so the split is anchored on the
// last ')'.
func parseProcStat(data []byte) (procStat, error) {
	open := bytes.IndexByte(data, '(')
	closeIdx := bytes.LastIndexByte(data, ')')
	if open < 1 || closeIdx < open {
		return procStat{}, errors.New("proc stat: malformed command field")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data[:open])))
	if err != nil {
		return procStat{}, fmt.Errorf("proc stat: pid: %w", err)
	}
	// Fields after the command start at field 3 (state); index 0 is state.
	rest := strings.Fields(string(data[closeIdx+1:]))
	const (
		idxPPID  = 1
		idxPGRP  = 2
		idxUTime = 11
		idxSTime = 12
		idxRSS   = 21
	)
	if len(rest) <= idxRSS {
		return procStat{}, fmt.Errorf("proc stat: %d fields after command, need %d", len(rest), idxRSS+1)
	}
	st := procStat{PID: pid, Comm: string(data[open+1 : closeIdx])}
	var perr error
	atoi := func(s string) int {
		n, e := strconv.Atoi(s)
		if e != nil && perr == nil {
			perr = e
		}
		return n
	}
	atou := func(s string) uint64 {
		n, e := strconv.ParseUint(s, 10, 64)
		if e != nil && perr == nil {
			perr = e
		}
		return n
	}
	st.PPID = atoi(rest[idxPPID])
	st.PGRP = atoi(rest[idxPGRP])
	st.UTime = atou(rest[idxUTime])
	st.STime = atou(rest[idxSTime])
	st.RSSPages = int64(atoi(rest[idxRSS]))
	if perr != nil {
		return procStat{}, fmt.Errorf("proc stat: %w", perr)
	}
	return st, nil
}

// readProcTable reads /proc/<pid>/stat for every process under procRoot.
// Processes that vanish while scanning are skipped.
func readProcTable(procRoot string) ([]procStat, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	table := make([]procStat, 0, len(entries))
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			continue
		}
		st, err := parseProcStat(data)
		if err != nil {
			continue
		}
		table = append(table, st)
	}
	return table, nil
}

// groupMembers returns the processes whose process group is pgid plus all of
// their descendants, so sandboxed children that left the group are included.
func groupMembers(table []procStat, pgid int) []procStat {
	children := make(map[int][]int, len(table))
	byPID := make(map[int]procStat, len(table))
	for _, p := range table {
		byPID[p.PID] = p
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	seen := make(map[int]bool)
	var queue []int
	for _, p := range table {
		if p.PGRP == pgid {
			seen[p.PID] = true
			queue = append(queue, p.PID)
		}
	}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if !seen[c] {
				seen[c] = true
				queue = append(queue, c)
			}
		}
	}
	members := make([]procStat, 0, len(seen))
	for pid := range seen {
		members = append(members, byPID[pid])
	}
	sort.Slice(members, func(i, j int) bool { return members[i].PID < members[j].PID })
	return members
}

// SandboxProcess is the sandbox evidence for one Chromium process.
type SandboxProcess struct {
	PID     int    `json:"pid"`
	PPID    int    `json:"ppid"`
	Type    string `json:"type"` // Chromium --type value; "browser" when absent
	Seccomp int    `json:"seccomp"`
	// SeccompFilters counts attached filters; a renderer must carry more than
	// the browser process (Chromium's own BPF on top of the container's).
	SeccompFilters int    `json:"seccompFilters"`
	NSpid          []int  `json:"nspid"`
	NoNewPrivs     int    `json:"noNewPrivs"`
	CapEff         string `json:"capEff"`
}

// SandboxReport is the evidence gathered by InspectSandbox. Enabled reports
// whether the Chromium sandbox was requested by configuration, independent of
// whether it has been proven yet; Verified is true only after the renderer
// sandbox was actually proven, so it is always false when Enabled is false.
type SandboxReport struct {
	Enabled   bool             `json:"enabled"`
	Verified  bool             `json:"verified"`
	Processes []SandboxProcess `json:"processes"`
}

// chromiumType extracts the --type=<x> argument from a cmdline. Chromium
// rewrites child argv with setproctitle, which joins arguments with spaces,
// so both NUL and space separate arguments.
func chromiumType(cmdline []byte) string {
	args := bytes.FieldsFunc(cmdline, func(r rune) bool { return r == 0 || r == ' ' })
	for _, arg := range args {
		if v, ok := bytes.CutPrefix(arg, []byte("--type=")); ok {
			return string(v)
		}
	}
	return "browser"
}

// InspectSandbox examines the process group pgid (and its descendants) under
// procRoot and proves the renderer sandbox is active: at least one renderer
// must exist, and every renderer must run inside a nested PID namespace (NSpid
// with at least two levels) under more seccomp filters than the browser
// process (pid == pgid). "Seccomp: 2" alone proves nothing inside a container,
// because the container runtime's own filter already sets it for every
// process. The report is returned even when verification fails so callers
// can expose the evidence.
//
// Only renderers are judged; the zygote and other utility processes are
// reported without being judged.
func InspectSandbox(procRoot string, pgid int) (SandboxReport, error) {
	table, err := readProcTable(procRoot)
	if err != nil {
		return SandboxReport{}, fmt.Errorf("inspect sandbox: %w", err)
	}
	members := groupMembers(table, pgid)
	if len(members) == 0 {
		return SandboxReport{}, fmt.Errorf("inspect sandbox: no processes in group %d", pgid)
	}

	report := SandboxReport{Processes: make([]SandboxProcess, 0, len(members))}
	var problems []error
	browserFilters := -1
	var rendererProcs []SandboxProcess
	for _, m := range members {
		dir := filepath.Join(procRoot, strconv.Itoa(m.PID))
		statusData, err := os.ReadFile(filepath.Join(dir, "status"))
		if err != nil {
			continue // exited while scanning
		}
		st, err := parseProcStatus(statusData)
		if err != nil {
			problems = append(problems, fmt.Errorf("pid %d: %w", m.PID, err))
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		sp := SandboxProcess{
			PID:            m.PID,
			PPID:           m.PPID,
			Type:           chromiumType(cmdline),
			Seccomp:        st.Seccomp,
			SeccompFilters: st.SeccompFilters,
			NSpid:          st.NSpid,
			NoNewPrivs:     st.NoNewPrivs,
			CapEff:         fmt.Sprintf("%016x", st.CapEff),
		}
		report.Processes = append(report.Processes, sp)
		if m.PID == pgid {
			browserFilters = sp.SeccompFilters
		}
		if sp.Type != "renderer" {
			continue
		}
		rendererProcs = append(rendererProcs, sp)
		if sp.Seccomp != 2 {
			problems = append(problems, fmt.Errorf("renderer %d: seccomp filter not active (Seccomp=%d)", sp.PID, sp.Seccomp))
		}
		if len(sp.NSpid) < 2 {
			problems = append(problems, fmt.Errorf("renderer %d: not in a nested PID namespace (NSpid=%v)", sp.PID, sp.NSpid))
		}
	}
	if browserFilters < 0 {
		problems = append(problems, errors.New("browser process seccomp filter count unavailable"))
	} else {
		for _, r := range rendererProcs {
			if r.SeccompFilters <= browserFilters {
				problems = append(problems, fmt.Errorf("renderer %d: no Chromium seccomp filter (filters=%d, browser=%d)", r.PID, r.SeccompFilters, browserFilters))
			}
		}
	}
	if len(rendererProcs) == 0 {
		problems = append(problems, errors.New("no renderer process found"))
	}
	if len(problems) > 0 {
		return report, fmt.Errorf("inspect sandbox: %w", errors.Join(problems...))
	}
	report.Verified = true
	return report, nil
}

// verifyDropped checks the post-DropPrivileges process state: only the kept
// capabilities remain, the bounding set is shrunk to them, and NO_NEW_PRIVS
// is set.
func verifyDropped(st procStatus) error {
	keep := keptCaps
	const wantNNP = 1
	var problems []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			problems = append(problems, fmt.Errorf(format, args...))
		}
	}
	allEqual := func(ids []uint32, want uint32) bool {
		if len(ids) == 0 {
			return false
		}
		for _, id := range ids {
			if id != want {
				return false
			}
		}
		return true
	}
	check(allEqual(st.UIDs, supervisorUID), "uids %v, want all %d", st.UIDs, supervisorUID)
	check(allEqual(st.GIDs, supervisorGID), "gids %v, want all %d", st.GIDs, supervisorGID)
	check(len(st.Groups) == 0, "supplementary groups %v, want none", st.Groups)
	check(st.CapEff == keep, "CapEff %016x, want %016x", st.CapEff, keep)
	check(st.CapPrm == keep, "CapPrm %016x, want %016x", st.CapPrm, keep)
	check(st.CapInh == 0, "CapInh %016x, want 0", st.CapInh)
	check(st.CapAmb == 0, "CapAmb %016x, want 0", st.CapAmb)
	check(st.CapBnd == keep, "CapBnd %016x, want %016x", st.CapBnd, keep)
	check(st.NoNewPrivs == wantNNP, "NoNewPrivs %d, want %d", st.NoNewPrivs, wantNNP)
	if len(problems) > 0 {
		return fmt.Errorf("privilege drop verification failed: %w", errors.Join(problems...))
	}
	return nil
}
