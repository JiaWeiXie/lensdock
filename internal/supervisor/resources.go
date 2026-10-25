package supervisor

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	cgroupRoot      = "/sys/fs/cgroup"
	maxCgroupFile   = 64 << 10
	cgroupUsecToNs  = 1000
	unlimitedMarker = "max"
)

// Resources is a bounded, read-only sample of the container's own resource
// use: cgroup v2 CPU and memory counters and filesystem space of the data
// volumes. Anything that cannot be measured is null, never zero, so a
// display cannot mistake "unknown" for "idle".
type Resources struct {
	SampledAt time.Time   `json:"sampledAt"`
	CPU       CPUUsage    `json:"cpu"`
	Memory    MemoryUsage `json:"memory"`
	Disk      []DiskUsage `json:"disk"`
}

// CPUUsage is the container's cumulative CPU time. Two samples give a rate:
// delta(usageNanos) / delta(sampledAt). Cores is the cgroup CPU limit, null
// when unlimited or unknown.
type CPUUsage struct {
	UsageNanos *uint64  `json:"usageNanos"`
	Cores      *float64 `json:"cores"`
}

// MemoryUsage is the cgroup memory counter and its limit (null when
// unlimited or unknown).
type MemoryUsage struct {
	CurrentBytes *uint64 `json:"currentBytes"`
	MaxBytes     *uint64 `json:"maxBytes"`
}

// DiskUsage is the capacity of the filesystem behind one data volume.
type DiskUsage struct {
	Name           string  `json:"name"`
	TotalBytes     *uint64 `json:"totalBytes"`
	AvailableBytes *uint64 `json:"availableBytes"`
}

// ReadResources samples the cgroup v2 files under cgroupDir and the volumes
// named in disks (name -> path).
func ReadResources(now time.Time, cgroupDir string, disks []NamedPath) Resources {
	r := Resources{SampledAt: now.UTC(), Disk: make([]DiskUsage, 0, len(disks))}
	if v, ok := readCgroupStatField(filepath.Join(cgroupDir, "cpu.stat"), "usage_usec"); ok {
		ns := v * cgroupUsecToNs
		r.CPU.UsageNanos = &ns
	}
	if q, p, ok := readCPUMax(filepath.Join(cgroupDir, "cpu.max")); ok {
		cores := float64(q) / float64(p)
		r.CPU.Cores = &cores
	}
	if v, ok := readCgroupUint(filepath.Join(cgroupDir, "memory.current")); ok {
		r.Memory.CurrentBytes = &v
	}
	if v, ok := readCgroupUint(filepath.Join(cgroupDir, "memory.max")); ok {
		r.Memory.MaxBytes = &v
	}
	for _, d := range disks {
		r.Disk = append(r.Disk, statDisk(d))
	}
	return r
}

// NamedPath names a data volume path.
type NamedPath struct{ Name, Path string }

func statDisk(d NamedPath) DiskUsage {
	out := DiskUsage{Name: d.Name}
	var st unix.Statfs_t
	if err := unix.Statfs(d.Path, &st); err != nil {
		return out
	}
	bsize := uint64(st.Bsize)
	total, avail := uint64(st.Blocks)*bsize, uint64(st.Bavail)*bsize
	out.TotalBytes, out.AvailableBytes = &total, &avail
	return out
}

func readSmallFile(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	buf := make([]byte, maxCgroupFile)
	n, _ := f.Read(buf)
	return string(bytes.TrimSpace(buf[:n])), n > 0
}

// readCgroupUint parses a single-number cgroup file; the literal "max" means
// unlimited and is reported as unknown (null).
func readCgroupUint(path string) (uint64, bool) {
	s, ok := readSmallFile(path)
	if !ok || s == unlimitedMarker {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	return v, err == nil
}

func readCgroupStatField(path, key string) (uint64, bool) {
	s, ok := readSmallFile(path)
	if !ok {
		return 0, false
	}
	for _, line := range strings.Split(s, "\n") {
		k, v, found := strings.Cut(line, " ")
		if !found || k != key {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// readCPUMax parses "<quota|max> <period>".
func readCPUMax(path string) (quota, period uint64, ok bool) {
	s, found := readSmallFile(path)
	if !found {
		return 0, 0, false
	}
	q, p, cut := strings.Cut(s, " ")
	if !cut || q == unlimitedMarker {
		return 0, 0, false
	}
	quota, err1 := strconv.ParseUint(q, 10, 64)
	period, err2 := strconv.ParseUint(strings.TrimSpace(p), 10, 64)
	if err1 != nil || err2 != nil || quota == 0 || period == 0 {
		return 0, 0, false
	}
	return quota, period, true
}

// Resources samples this container's resource use.
func (s *Supervisor) Resources() Resources {
	return ReadResources(s.now(), cgroupRoot, []NamedPath{
		{Name: "profile", Path: profileDir},
		{Name: "downloads", Path: downloadsDir},
	})
}
